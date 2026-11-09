# Copilot instructions for dexa-fix

This is a condensed summary. The full, canonical instructions are in [`AGENTS.md`](../AGENTS.md). Area-specific rules are in [`.github/instructions/`](instructions/), one file per area (frontend, backend, internal API, shared, store, workers, command line, domain libraries, tests, docs, deployment, CI). Keep this file in sync with them.

## Repository

dexa-fix is one Go binary that corrects smart-scale readings with DEXA scans. It runs as one process (`serve`) or as separate ones (`serve web`, `serve api`, and any number of `serve worker`):

- `pkg/frontend`: the web process. Pages (Go templates and htmx), static files, HTTP/HTTPS/HTTP3 listeners. **No database access.**
- `pkg/backend/main`: the main backend. Store, keys, every security decision, internal API (`/internal/v1`), public API (`/api/v1`).
- `pkg/backend/body`: the body service. A user's scale readings, DEXA scans (encrypted with a key of their own), estimates and the import rules. It knows nothing about people and trusts only the short-lived signed token the main backend sends (`pkg/shared/bodyapi`).
- `pkg/workers`: the workers, processes **without a database** that talk to Withings, Strength Level and the like. They lease jobs from the backend's queue and report back; nothing else touches those services.
- `pkg/shared`: what they need in common (`apiclient` is the web/api contract, `workerapi` the api/worker contract, `bodyapi` the api/body contract, `edge`, `mtls`, small helpers).
- `pkg/store`: the `Store` interface and the SQL implementation for SQLite and PostgreSQL.
- `pkg/app`, `cmd`: the single-process mode and the command line.

## Rules

- Keep changes minimal and within the requested area.
- The frontend never imports the backend, the store, a database library, `go-oidc` or `go-webauthn` (tests enforce it). Data comes through `pkg/shared/apiclient` from an internal API endpoint.
- The backend authorises and validates every request again and never trusts the frontend or a worker; it never imports the frontend, the workers or the body service. The body service never imports the backend. It never calls an outside service: it queues a job. Workers never import the backend, the store or a database library, and see a user's credentials only inside one job's lease.
- State that must agree across processes (single-use nonces, failed-attempt counters, the job queue) lives in the database, not in memory. Security checks fail closed.
- Settings live on the Admin page (stored in the database), not in flags or environment variables. A flag is only for boot-critical things: addresses, database, key and secret files, backend URL, TLS files.
- Secrets are never flag values or URL parts; flags take file paths. Do not log secrets, tokens or cookies.
- No migration files before 1.0: edit `00001_init.sql` for both SQLite and PostgreSQL, in step.
- Keep user-visible strings and status codes. Keep `docs/` and the in-app `docs.html` true.
- Do not weaken a test to make it pass.

## Validate

```bash
gofmt -l .                                   # prints nothing
go build ./... && go vet ./...
go test -race -count=1 ./...
DEXAFIX_TEST_SPLIT=1 go test -race -count=1 ./pkg/backend/... ./pkg/frontend
```

Test logs are noisy JSON lines: filter with `grep -v '^{'`. For SQL changes also run `go test ./pkg/store/sqlstore/` against PostgreSQL 18 (`DEXAFIX_TEST_POSTGRES_DSN`).

## Commits

Conventional prefixes (`feat:`, `fix:`, `refactor:`, `docs:`, `perf:`, `ci:`, `build:`, `chore:`), one logical change per commit, tests green at each commit.
