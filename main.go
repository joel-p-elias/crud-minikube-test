// Notes CRUD API - Go (Gin) + MariaDB.
//
// Rewrite of the Node.js/Express/PostgreSQL notes app. Same endpoints:
//
//	POST   /notes       create a note
//	GET    /notes       list all notes (ordered by id)
//	GET    /notes/:id   read one note
//	PUT    /notes/:id   update a note
//	DELETE /notes/:id   delete a note
//
// Plus health endpoints for Docker / Kubernetes:
//
//	GET /health        readiness: app is up AND the database answers
//	GET /health/live   liveness: the process is up (no database check)
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

// The frontend is compiled into the binary, so the distroless image
// only needs one file (the static binary).
//
//go:embed public
var publicFS embed.FS

// Note is the JSON shape of a row, identical to the Node.js API.
type Note struct {
	ID      int64   `json:"id"`
	Title   string  `json:"title"`
	Content *string `json:"content"` // null when no content was given
}

type notePayload struct {
	Title   *string `json:"title"`
	Content *string `json:"content"`
}

type server struct {
	db    *sql.DB
	ready atomic.Bool // true once the table exists (migration done)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe GET /health on the local server and exit 0 (healthy) or 1 (unhealthy)")
	flag.Parse()

	port := env("PORT", "3000")

	// Docker's HEALTHCHECK cannot use curl/wget in a distroless image
	// (there is no shell), so the binary can probe itself.
	if *healthcheck {
		os.Exit(runHealthcheck(port))
	}

	db, err := openDB()
	if err != nil {
		log.Fatalf("database config error: %v", err)
	}

	s := &server{db: db}

	srv := &http.Server{ // standard net/http server; Gin is just the Handler
		Addr:              ":" + port,
		Handler:           trimTrailingSlash(s.router()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Migrate in the background: the HTTP server (and /health/live) is up
	// immediately, /health reports 503 until the database is ready.
	go func() {
		if err := s.waitAndMigrate(ctx); err != nil {
			log.Printf("could not prepare database: %v", err)
			stop()
			os.Exit(1)
		}
		log.Println("connected to MariaDB, schema ready")
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("API running on port %s", port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		log.Println("shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx) // finish in-flight requests
	_ = db.Close()
}

// ---------------------------------------------------------------- database

func openDB() (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = env("DB_USER", "notes_user")
	cfg.Passwd = env("DB_PASSWORD", "notes_pass")
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", env("DB_HOST", "db"), env("DB_PORT", "3306"))
	cfg.DBName = env("DB_NAME", "notesdb")
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	// Report "rows matched" (not "rows changed") so an UPDATE that sets the
	// same values is not mistaken for "id not found".
	cfg.ClientFoundRows = true

	db, err := sql.Open("mysql", cfg.FormatDSN()) // lazy: no connection yet
	if err != nil {
		return nil, err
	}
	// MariaDB default max_connections is 151; with up to 5 replicas
	// (HPA) 5 x 20 = 100 stays below it.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS notes (
  id      INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  title   TEXT NOT NULL,
  content TEXT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// waitAndMigrate retries until MariaDB accepts connections (like the
// original initDb loop), then creates the table if needed.
func (s *server) waitAndMigrate(ctx context.Context) error {
	const attempts = 30
	var err error
	for i := 1; i <= attempts; i++ {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err = s.db.PingContext(c); err == nil {
			_, err = s.db.ExecContext(c, schema)
		}
		cancel()
		if err == nil {
			s.ready.Store(true)
			return nil
		}
		log.Printf("waiting for database (%d/%d): %v", i, attempts, err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("database not reachable after %d attempts: %w", attempts, err)
}

// ------------------------------------------------------------------ router

func (s *server) router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	_ = r.SetTrustedProxies(nil)
	r.RedirectTrailingSlash = false // handled by trimTrailingSlash (Express treats /notes/ == /notes)
	r.Use(
		gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/health", "/health/live"}}),
		gin.Recovery(),
		limitBody(100*1024), // same default as express.json(): 100kb
	)

	r.GET("/health", s.health)
	r.GET("/health/live", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	r.POST("/notes", s.createNote)
	r.GET("/notes", s.listNotes)
	r.GET("/notes/:id", s.getNote)
	r.PUT("/notes/:id", s.updateNote)
	r.DELETE("/notes/:id", s.deleteNote)

	// Fault-tolerance demo only: lets you crash the container on purpose to
	// watch Kubernetes restart it. Disabled unless ENABLE_CRASH_ENDPOINT=true.
	if env("ENABLE_CRASH_ENDPOINT", "false") == "true" {
		log.Println("WARNING: POST /debug/crash is enabled (demo only)")
		r.POST("/debug/crash", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"message": "crashing now"})
			go func() { time.Sleep(200 * time.Millisecond); os.Exit(1) }()
		})
	}

	// Static frontend (equivalent of express.static("public")).
	sub, err := fs.Sub(publicFS, "public")
	if err != nil {
		log.Fatal(err)
	}
	files := http.FileServer(http.FS(sub))
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			files.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
	})
	return r
}

// trimTrailingSlash makes "/notes/" behave like "/notes".
func trimTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if p := req.URL.Path; len(p) > 1 && strings.HasSuffix(p, "/") {
			req.URL.Path = strings.TrimRight(p, "/")
			req.URL.RawPath = ""
		}
		next.ServeHTTP(w, req)
	})
}

func limitBody(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}

// ---------------------------------------------------------------- handlers

func (s *server) health(c *gin.Context) {
	if !s.ready.Load() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "starting", "database": "not ready"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "database": "down"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "up"})
}

func serverError(c *gin.Context, err error) {
	log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err) // details stay in the log
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}

func notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
}

func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		notFound(c)
		return 0, false
	}
	return id, true
}

func bindNote(c *gin.Context) (notePayload, bool) {
	var p notePayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return p, false
	}
	if p.Title == nil || strings.TrimSpace(*p.Title) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "title is required"})
		return p, false
	}
	return p, true
}

func (s *server) createNote(c *gin.Context) {
	p, ok := bindNote(c)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(c.Request.Context(),
		"INSERT INTO notes (title, content) VALUES (?, ?)", *p.Title, p.Content)
	if err != nil {
		serverError(c, err)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusCreated, Note{ID: id, Title: *p.Title, Content: p.Content})
}

func (s *server) listNotes(c *gin.Context) {
	rows, err := s.db.QueryContext(c.Request.Context(), "SELECT id, title, content FROM notes ORDER BY id")
	if err != nil {
		serverError(c, err)
		return
	}
	defer rows.Close()

	notes := make([]Note, 0) // [] instead of null when empty
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Title, &n.Content); err != nil {
			serverError(c, err)
			return
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, notes)
}

func (s *server) getNote(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var n Note
	err := s.db.QueryRowContext(c.Request.Context(),
		"SELECT id, title, content FROM notes WHERE id = ?", id).Scan(&n.ID, &n.Title, &n.Content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		notFound(c)
	case err != nil:
		serverError(c, err)
	default:
		c.JSON(http.StatusOK, n)
	}
}

func (s *server) updateNote(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	p, ok := bindNote(c)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(c.Request.Context(),
		"UPDATE notes SET title = ?, content = ? WHERE id = ?", *p.Title, p.Content, id)
	if err != nil {
		serverError(c, err)
		return
	}
	if n, err := res.RowsAffected(); err != nil {
		serverError(c, err)
		return
	} else if n == 0 {
		notFound(c)
		return
	}
	c.JSON(http.StatusOK, Note{ID: id, Title: *p.Title, Content: p.Content})
}

func (s *server) deleteNote(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	// Like the original: deleting a missing id still answers 200.
	if _, err := s.db.ExecContext(c.Request.Context(), "DELETE FROM notes WHERE id = ?", id); err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Deleted"})
}

// ------------------------------------------------------------- healthcheck

func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck failed: status", resp.StatusCode)
		return 1
	}
	return 0
}
