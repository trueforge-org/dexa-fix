# Development

```bash
go test ./...
```

The tests use in-memory SQLite. To run the store tests against Postgres too,
set `DEXAFIX_TEST_POSTGRES_DSN`. Each test gets its own schema. Postgres 18 is
the supported version, so test against that:

```bash
docker run -d --rm --name dexa-pg -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:18
DEXAFIX_TEST_POSTGRES_DSN='postgres://postgres:x@localhost:5432/postgres?sslmode=disable' \
  go test ./pkg/store/sqlstore/
```

To run the browser-level tests of `pkg/web` against the split deployment (a
frontend without a database that talks over TCP to a separate backend), set
`DEXAFIX_TEST_SPLIT`. CI runs both ways. The tests that cover the combined
server's listeners are skipped there; `frontend_tls_test.go` covers HTTPS in
the split.

```bash
DEXAFIX_TEST_SPLIT=1 go test ./pkg/web
```

## Layout

- `main.go`: entry point. Sets up logging and prints the version.
- `cmd/`: `serve` runs the web app in one process. `serve web` and `serve api`
  (`serve_split.go`) run the frontend and the backend as two processes. `db`
  migrates and copies the database, `secrets` creates the service secret, and
  `info` prints binary information.
- `pkg/edge/`: HTTP edge code that both processes share: the visitor's address
  behind a proxy, the PROXY protocol and request limits. It must not import the
  store.
- `pkg/apiclient/`: the frontend's client for the backend's internal API under
  `/internal/v1`, and the in-process transport that `serve` uses to run both
  halves in one process. It must not import the store or `pkg/web`.
- `pkg/estimate/`: estimation logic.
- `pkg/strengthlevel/`: the Strength Level entries built from the estimates
  (`strengthlevel.go`) and the sync client (`client/`).
- `pkg/importer/`: the importer interfaces and the registry.
- `pkg/importer/withings/`: OAuth and API client (`client.go`), the rules for
  what to write (`plan.go`), and the importer (`withings.go`).
- `pkg/models/`: domain types.
- `pkg/store/`: the `Store` interface. `sqlstore/` implements it with bun for
  SQLite and Postgres. Its goose migrations live under `migrations/`.
- `pkg/web/`: HTML handlers and templates, the JSON API (`api.go`), the
  integration routes (`handlers_import.go`), routes in `server.go`, and the
  in-memory estimate cache (`estcache.go`). The split lives here too: `frontend.go`
  is the frontend process, `backend.go` the public handlers of the backend, and
  `internal.go` the internal API under `/internal/v1`.

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.

## Schema changes

Until the first release (1.0.0) the schema is kept in one migration per database,
`00001_init.sql`: change it there instead of adding a file. After the release,
every schema change adds a migration with the same version number to both
`pkg/store/sqlstore/migrations/sqlite/` and `migrations/postgres/`, for example
`00002_add_notes.sql`. Each file has `-- +goose Up` and `-- +goose Down`
sections. `TestMigrationParity` fails if the two directories differ.

## Adding an importer

An importer writes readings from an external service. It implements:

```go
type Importer interface {
    Name() string
    Import(ctx context.Context, st store.Store, userID int64, o Options) (Result, error)
}
```

`Name` is the provider name used in URLs and as the readings' `Source`. Write
through `st.UpsertScaleReadings` with a stable `ExternalID`, so repeated
imports are idempotent. If the user must link an account, also implement
`importer.OAuthProvider`. Its OAuth routes are generic over the provider name.

Register the importer in `cmd/serve.go`:

```go
srv, err := web.New(st, importer.NewRegistry(wi, myImporter))
```

The import API works for any registered provider. The estimates page has
buttons only for Withings (`providerWithings` in `pkg/web/handlers_estimates.go`
and `pkg/web/templates/estimates.html`), so a new provider needs its own
buttons there.
