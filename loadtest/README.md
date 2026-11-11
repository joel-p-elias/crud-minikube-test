# Load test K6 and Locust

| File | Tool | What it does |
|---|---|---|
| `crud-k6.js` | k6 | Create -> Read -> Update -> Delete. 1 user = functional check, 500 users for 2 min = load test |
| `locustfile.py` | Locust | The same flow with a second tool, 500 users for 2 min |

Load shape: 20 s ramp up to 500 users, 80 s hold, 20 s ramp down. Each user pauses 1 s after every step.

## Setup (PowerShell)

```powershell
mkdir loadtest\results
minikube service notes --url          # prints the app URL, keep this terminal open
$URL = "http://127.0.0.1:PORT"        # in a second terminal, use the URL printed above
```
If the `minikube service` tunnel struggles at 500 users, use `kubectl port-forward svc/notes 8080:80`
and `$URL = "http://127.0.0.1:8080"`. Say in the report which one you used.

## 1. Functional check (1 user)

```powershell
k6 run -e BASE_URL=$URL -e VUS=1 loadtest/crud-k6.js
```
All four checks (create, read, update, delete) must show a green tick. Screenshot it.

## 2. k6 load test + report

```powershell
$env:K6_WEB_DASHBOARD = "true"
$env:K6_WEB_DASHBOARD_EXPORT = "loadtest/results/k6-report.html"
k6 run -e BASE_URL=$URL --summary-export=loadtest/results/k6-summary.json loadtest/crud-k6.js
```
- The terminal summary shows `http_req_duration` (med = p50, p90, p95, p99) and `http_req_failed` (error rate).
- `k6-report.html` (open it in a browser) has charts over time: response times, requests per second, failures.
  Screenshot the latency and failure charts for the report. It needs k6 0.49 or newer (`k6 version`).

## 3. Locust load test + report

```powershell
pip install locust
locust -f loadtest/locustfile.py --headless -u 500 -r 25 -t 2m --host $URL --html loadtest/results/locust-report.html --csv loadtest/results/locust
```
`locust-report.html` has the table of p50/p95/p99 per endpoint and charts of response times and failures
over time. `locust_stats_history.csv` has the same numbers per second if you want your own table.

Wait about 2 minutes between the two runs so the HPA scales back down.

## While each test runs (2 extra terminals)

```powershell
kubectl top pods                  # run it 3-4 times during the hold phase
kubectl get hpa notes-hpa -w      # shows the app scaling up (evidence for Deliverable 4)
```
Compare the readings with the limits in the app's Deployment:
- **CPU-bound:** CPU sits at or near the 200m limit while latency rises.
- **Memory-bound:** memory climbs toward the 128Mi limit, or the pod shows `OOMKilled`.

`kubectl top` needs `minikube addons enable metrics-server`.

## Notes for the report
- k6 and Minikube share your PC's CPU, so the laptop itself can become the limit.
- If thresholds are missed or errors appear at 500 users, that is a valid result: say where it started
  (look at the charts) and what you think caused it.
- Do not commit `results/`, paste tables and screenshots into the report instead.
