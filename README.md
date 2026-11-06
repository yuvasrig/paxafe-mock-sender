# PAXAFE Mock Tive Sender

Generates Tive payloads (valid and broken), sends them to the PAXAFE Integration API, shows every answer, and checks
that every payload the API acknowledged can be found again: *N sent · N acknowledged · N processed · R rejected
(expected) · 0 missing*. One Go binary: a web UI and a CLI. (Full README is written in M7.)

## Scenarios
| ID | What it sends | Expected |
|---|---|---|
| `samples` | the 13 provided samples | the golden status and flags from the API repo's `expected/` files (first run, or `duplicate` on a re-run) |
| `route` | one tracker, Boston to Atlanta, temperature inside 2-8 C | all accepted |
| `excursion` | the same route drifting to 12 C | accepted with no flag (the API does not judge temperatures) |
| `fault` | probe sentinels 327.67 / -32768, one faulty device temperature | `SENSOR_FAULT`, `TEMPERATURE_FALLBACK` |
| `duplicates` | every reading twice, some a third time changed | `duplicate`, `conflict` |
| `malformed` | bad coordinates, strings, seconds timestamp, missing identity, future time, non-JSON | flagged or rejected, never lost |
| `auth` | no key, unknown key, malformed key | 401 |
| `load` | N requests/s for M seconds | p50/p95/p99, error rate, throughput |

`status` is `flagged` if and only if a response carries any flag. A scripted step that gets `429` waits out `Retry-After` and retries. A load run never retries: a 429 there is a result.

## CLI
```bash
export API_BASE_URL=http://localhost:8080 SENDER_API_KEY=pxk_test_...
go run ./cmd/sender list
go run ./cmd/sender run --scenario samples
go run ./cmd/sender run --scenario load --rate 100 --duration 30s
go run ./cmd/sender run --scenario route --json
```
Exit status is 1 if any check fails or any acknowledged payload is missing, so it can gate a deploy.

## Load from several scenarios

In the web UI, the **Load generator** card has a checkbox per scenario: tick any number, pick a rate and duration, and requests are drawn evenly from all ticked scenarios. From the CLI, list them with commas:

```
sender run --scenario route,fault,malformed,duplicates --rate 30 --duration 30s --url ... --key ...
```

Scripted scenarios are replayed with a new seed on each pass, so a long run sends new payloads instead of repeating one. Because requests interleave, a mixed run checks what does not depend on order: the HTTP answer, that the payload was acknowledged, and (afterwards) that every acknowledged event can be found again, plus the sent-vs-stored comparison. Per-request status and flag expectations belong to the single-scenario runs.

## Async tenants

If the API key belongs to an **async** tenant, the webhook answers `202 {"status":"queued"}` and a worker processes the payload afterwards. The sender handles this itself: a 202 only proves the payload was acknowledged, so it follows each queued event (`GET /api/v1/events/{id}`) until a worker gives it a final status, then judges it against the scenario's expectations (final status, required flags). Events that never finish count as **not processed** (a `dead` event does too), and the run fails. `--wait` (default 1m) sets how long to wait per event; the report shows how many were queued.

## Sent vs processed report

Every acknowledged event is read back from the API (`GET /api/v1/events/{id}`) and its temperature and position are compared with what this sender sent. The run report shows the counts, a table, and two charts (temperature per event, and the route as latitude against longitude: grey line = sent, blue dots = stored by the API, red ring = sent but not stored), and offers the whole comparison as a CSV (`/runs/<id>/compare.csv`). The CLI prints the counts too.

| Verdict | Meaning |
|---|---|
| match | stored as sent, within the API's output rounding (temperature 2 decimals, coordinates 6) |
| fixed | stored differently, and the event carries a flag saying a rule did it (for example the probe temperature used when the device temperature was a sentinel) |
| dropped | sent, but nulled by a rule (and flagged) |
| differ | stored differently with no flag to explain it: a real discrepancy |

Rejected events have nothing stored and are left out of the comparison. Comparison covers the verified events (`--verify`, default 300).

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
