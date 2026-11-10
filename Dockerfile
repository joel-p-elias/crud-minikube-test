ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/server .
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server
ENV GIN_MODE=release \
    PORT=3000
EXPOSE 3000
USER 65532:65532
ENTRYPOINT ["/server"]
