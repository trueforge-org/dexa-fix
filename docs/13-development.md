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

The browser-level tests live in `pkg/backend/main`: `stack_test.go` builds a
main backend with a frontend over the in-process internal API, as `pkg/app`
does. To run them against the split deployment (a frontend without a database
that talks over TCP to a separate main backend), set `DEXAFIX_TEST_SPLIT`. CI
runs both ways. The tests that cover the combined server's listeners are
skipped there; `pkg/frontend/frontend_tls_test.go` covers HTTPS in the split.

```bash
go test ./pkg/backend/... ./pkg/frontend ./pkg/app
DEXAFIX_TEST_SPLIT=1 go test ./pkg/backend/... ./pkg/frontend
```

## Layout

- `main.go`: entry point. Sets up logging and prints the version.
- `cmd/`: `serve` runs the web app in one process. `serve web` and `serve api`
  (`serve_split.go`) run the frontend and the backend as two processes. `db`
  migrates and copies the database, `secrets` creates the service secret, and
  `info` prints binary information.
- `pkg/frontend/`: package `frontend`. The HTML side: templates and static
  files, the page handlers (`handlers_*.go`), the public listeners (plain, HTTPS
  and HTTP/3), the config poller and the request limiter. It has no database
  and asks the main backend for everything through `apiclient.Client`.
- `pkg/backend/main/`: package `mainbackend`, the main backend (`dexa-fix serve
  api`). The store, the keys and every security decision: the internal API under
  `/internal/v1` (`internal*.go`), the public API (`api.go`), importers,
  and maintenance. It reaches the scale readings, DEXA scans and estimates only
  through `body.go`, a wrapper of the `bodyapi` client. `pkg/backend` is a plain
  parent folder with a service in each subfolder.
- `pkg/backend/body/`: package `bodybackend`, the body service (`dexa-fix serve
  body`, or inside the api process through `NewInProcess`): the readings and
  scans, the estimates and their cache (`estcache.go`) and the rules for
  importing readings. It knows nothing about people and trusts only the signed
  token of the api process. See [Body service](17-body-service.md).
- `pkg/app/`: package `app`, the single-process mode. `app.New` builds the main
  backend and the frontend over `apiclient.InProcess`; `serve` uses it.
- `pkg/shared/`: what both sides use, in small packages named by function:
  `edge/` (the visitor's address behind a proxy, the PROXY protocol, request
  limits, security headers), `apiclient/` (the contract between the two: the
  client, the DTOs, header and cookie names, error codes, and the in-process
  transport), `bodyapi/` (the contract between the main backend and the body
  service, with its tokens), `inproc/` (the in-process transport),
  `tlsutil/` (certificate parsing), `timezones/` and `jobs/`.

The rules are tests. `pkg/frontend/boundary_test.go` fails when the frontend
imports (directly or not) anything under `pkg/backend/`, `pkg/store`, the
importers, the mailer, Strength Level, a database driver, OIDC or WebAuthn.
`pkg/shared/boundary_test.go` keeps `pkg/shared` independent of both sides and
of the store. The main backend does not import the frontend or the body service
(its tests may, to drive the HTML flows and to run a body service next to it),
and the body service does not import the main backend, the frontend, the workers,
the mailer or a sign-in library.
- `pkg/estimate/`: estimation logic.
- `pkg/strengthlevel/`: the Strength Level entries built from the estimates
  (`strengthlevel.go`) and the sync client (`client/`).
- `pkg/importer/`: the importer interfaces and the registry.
- `pkg/importer/withings/`: OAuth and API client (`client.go`), the rules for
  what to write (`plan.go`), and the importer (`withings.go`).
- `pkg/models/`: domain types.
- `pkg/store/`: the `Store` interface. `sqlstore/` implements it with bun for
  SQLite and Postgres. Its goose migrations live under `migrations/`.

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.

## Guidance for AI coding agents

`AGENTS.md` is the canonical, tool-agnostic instruction file. The rules for one
area of the code are in `.github/instructions/<area>.instructions.md`, with
`applyTo:` (read by GitHub Copilot) and `paths:` (read by Claude Code)
frontmatter. `.claude/rules/` holds a symlink to each of them, plus
`dexa-fix.md` with the Claude-only rules, and `.github/copilot-instructions.md`
is a short summary. When you add an area, add its file, the symlink
(`ln -s ../../.github/instructions/<area>.instructions.md .claude/rules/`) and
a row in the table of `AGENTS.md`. `guidance_test.go` fails when the two path
lists differ, a symlink is missing, a link is broken, or a path no longer
matches any file after a directory moved.

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
srv, err := app.New(st, importer.NewRegistry(wi, myImporter))
```

The import API works for any registered provider. The estimates page has
buttons only for Withings (`providerWithings` in `pkg/backend/main/consts.go` and
`pkg/frontend/templates/estimates.html`), so a new provider needs its own
buttons there.
