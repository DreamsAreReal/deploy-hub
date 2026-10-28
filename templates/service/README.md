# starter

A service made from the deploy-hub template. It already does every standard thing
the one way the hub expects, so the first push passes the gate:

| Need | The one way | Where |
|---|---|---|
| Deploy | the hub's reusable workflow, gated on every push | `.github/workflows/deploy.yml` |
| Health | `GET /healthz`, and the binary probes itself for the container health check | `internal/web`, `-healthcheck` |
| Metrics | Prometheus text on `METRICS_PORT` (default 9100), scraped over the `shared` network | `GET /metrics` |
| Logs | one JSON line per request on stdout | `log/slog` |
| Errors | panics go to Bugsink through the Sentry SDK when `SENTRY_DSN` is set | `internal/web` |
| Secrets | the hub's store lays them into `.env`; the repo only lists names in `.env.example` | `.env.example` |
| Alerts | vmalert rules in the observability repo; `TargetDown` already covers a dead service | — |
| Security headers | a strict CSP and friends on every answer | `internal/web` |

Run the checks the gate runs most often:

```
go vet ./...
go test -race -cover ./...
go test -run '^$' -fuzz FuzzEveryAnswerKeepsTheSecurityHeaders -fuzztime 15s ./internal/web
```

Code has no comments (the gate refuses them), functions stay at cognitive complexity 9 or
less, and coverage and mutation scores stay at 90% or more.
