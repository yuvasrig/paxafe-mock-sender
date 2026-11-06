# PAXAFE Mock Tive Sender

Generates Tive payloads (valid and broken), sends them to the PAXAFE Integration API, shows every answer, and checks
that every payload the API acknowledged can be found again: *N sent · N acknowledged · N processed · R rejected
(expected) · 0 missing*. One Go binary: a web UI and a CLI. (Full README is written in M7.)

## Scenarios
| ID | What it sends | Expected |
|---|---|---|
| `samples` | the 13 provided samples | each sample's expected status and flags (first run, or `duplicate` on a re-run) |
| `route` | one tracker, Boston to Atlanta, temperature inside 2-8 C | all accepted |
| `excursion` | the same route drifting to 12 C | accepted (the excursion flag is designed, not built) |
| `fault` | probe sentinels 327.67 / -32768, one faulty device temperature | `SENSOR_FAULT`, `TEMPERATURE_FALLBACK` |
| `duplicates` | every reading twice, some a third time changed | `duplicate`, `conflict` |
| `malformed` | bad coordinates, strings, seconds timestamp, missing identity, future time, non-JSON | flagged or rejected, never lost |
| `auth` | no key, unknown key, malformed key | 401 |
| `load` | N requests/s for M seconds | p50/p95/p99, error rate, throughput |

A scripted step that gets `429` waits out `Retry-After` and retries. A load run never retries: a 429 there is a result.

## CLI
```bash
export API_BASE_URL=http://localhost:8080 SENDER_API_KEY=pxk_test_...
go run ./cmd/sender list
go run ./cmd/sender run --scenario samples
go run ./cmd/sender run --scenario load --rate 100 --duration 30s
go run ./cmd/sender run --scenario route --json
```
Exit status is 1 if any check fails or any acknowledged payload is missing, so it can gate a deploy.

## Web UI
```bash
make run API=http://localhost:8080 KEY=pxk_test_...    # then open http://localhost:8080
```
The key field overrides `SENDER_API_KEY`; a typed key is used for that run only. The UI pings `/healthz` on load to wake a
sleeping free-tier API. Web load tests are capped at 100/s and 30 s; use the CLI for more.

## Deploy (Render)
`render.yaml` creates the service. Set `API_BASE_URL` (the API's public URL) and `SENDER_API_KEY` (a key for tenant
`reviewer`, created with `api keys create --tenant reviewer --name sender`) in the dashboard. The sender key stays on the server.

## Test
```bash
make test
```
Tests use a fake API; the real-API checks are the CLI scenarios above.
