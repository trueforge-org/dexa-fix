# AGENTS.md

These are the instructions for AI coding agents working in the DexaFix repository, including GitHub Copilot, OpenAI Codex/ChatGPT, Claude Code and any other tool that reads `AGENTS.md`.

- **`AGENTS.md`** (this file) is the canonical, tool-agnostic source.
- **`CLAUDE.md`** imports this file for Claude Code. It is copied daily from `trueforge-org/.github`, so put Claude-only rules in `.claude/rules/dexafix.md`. The scoped files below are symlinked into `.claude/rules/`, where Claude Code loads each one by its `paths:` frontmatter. Keep `paths:` in sync with `applyTo:`, and add a symlink with the same file name for every new scoped file (`ln -s ../../.github/instructions/<name>.instructions.md .claude/rules/`), so relative links resolve from both places. `guidance_test.go` checks this.
- **`.github/copilot-instructions.md`** is a short, self-contained summary for Copilot surfaces that only read that file.
- **`.github/instructions/*.instructions.md`** hold the area-specific rules. Copilot applies them automatically through `applyTo`. Other agents **must read the matching file before editing**:

| When you change files in | Read first |
| --- | --- |
| `pkg/frontend/**` (pages, listeners) | [`frontend.instructions.md`](.github/instructions/frontend.instructions.md), plus [templates and static files](.github/instructions/frontend-templates.instructions.md) for `templates/**` and `static/**` |
| `pkg/backend/**` (the main backend) | [`backend.instructions.md`](.github/instructions/backend.instructions.md), plus [the internal API](.github/instructions/backend-internal-api.instructions.md) for `internal*.go` and `pkg/shared/apiclient/**` |
| `pkg/backend/body/**` (readings, scans, estimates) | [`body.instructions.md`](.github/instructions/body.instructions.md), and [`shared.instructions.md`](.github/instructions/shared.instructions.md) for the contract `pkg/shared/bodyapi` |
| `pkg/shared/**` (what both sides use) | [`shared.instructions.md`](.github/instructions/shared.instructions.md) |
| `pkg/store/**` (interfaces, SQL, migrations) | [`store.instructions.md`](.github/instructions/store.instructions.md) |
| `cmd/**`, `main.go`, `pkg/app/**` (flags, processes) | [`process-wiring.instructions.md`](.github/instructions/process-wiring.instructions.md) |
| `pkg/estimate`, `pkg/mailer`, `pkg/models` | [`domain-libraries.instructions.md`](.github/instructions/domain-libraries.instructions.md) |
| `pkg/workers/**` (workers for Withings, Strength Level, ...) | [`workers.instructions.md`](.github/instructions/workers.instructions.md), and [`shared.instructions.md`](.github/instructions/shared.instructions.md) for the contract `pkg/shared/workerapi` |
| any `*.go` (coverage) and every `*_test.go` | [`tests.instructions.md`](.github/instructions/tests.instructions.md) |
| `docs/**`, `README.md` | [`docs.instructions.md`](.github/instructions/docs.instructions.md) |
| `deploy/**`, `Dockerfile.ci` | [`deploy.instructions.md`](.github/instructions/deploy.instructions.md) |
| `.github/workflows/**`, `.goreleaser.yaml`, `codecov.yml` | [`ci-release.instructions.md`](.github/instructions/ci-release.instructions.md) |

When you change a rule, update `AGENTS.md` and the scoped files first, then keep `.github/copilot-instructions.md` in sync.

## Repository overview

DexaFix corrects smart-scale body-composition readings with DEXA scan results. It is one Go binary (`github.com/trueforge-org/dexafix`) that runs as one process (`dexafix serve`) or as separate ones (`dexafix serve web` and `dexafix serve api`, plus `dexafix serve body` for the readings, scans and estimates, and any number of `dexafix serve worker`), so that a flood of requests on one cannot take down the other.

```text
browser ──> web (pkg/frontend, no database) ──internal API, service secret (+ mutual TLS)──> api (pkg/backend/main)
                                                                                                   │ signed token per call
                                                                                                   └──> body (pkg/backend/body): readings, scans, estimates
scripts ──────────────── public JSON API /api/v1 ───────────────────────────────────────────> api ──> SQLite or PostgreSQL
                                                                                               ^
worker (pkg/workers, no database) ──worker API /internal/v1/worker, worker secret (+ mutual TLS)┘
   └─> Withings, Strength Level, ... (the only process that talks to them)
```

| Path | Contents |
| --- | --- |
| `pkg/frontend/` | The web process: HTML pages (Go templates and htmx), static files, the plain/HTTPS/HTTP3 listeners, request limits. **Has no database access**; it asks the backend through `pkg/shared/apiclient`. |
| `pkg/backend/main/` | The main backend (package `mainbackend`): store, keys, every security decision, the internal API (`/internal/v1`) and the public API (`/api/v1`), the job queue and its scheduler, the backend side of each provider, maintenance. `pkg/backend/` itself has no Go files: it is the parent folder, with a service in each subfolder. |
| `pkg/backend/body/` | The body service (package `bodybackend`): a user's scale readings and DEXA scans (encrypted with a key of their own), the estimates computed from them and their cache, and the rules for importing readings from a source. It knows nothing about people: the main backend calls it with a short-lived signed token for one user and one scope (`pkg/shared/bodyapi`). It runs inside the api process unless `serve api` gets a `--body-url`; then `dexafix serve body` runs it with its own database, key and secret (`bodybackend.NewInProcess` is the in-process form). |
| `pkg/shared/` | What both sides need, as small packages named by function: `apiclient` (the contract between the web and the api), `workerapi` (the contract between the api and the workers), `bodyapi` (the contract between the api and the body service, with its tokens), `inproc` (the in-process transport), `edge` (proxy headers, PROXY protocol, limiter, security headers), `mtls`, `jobs`, `timezones`, `tlsutil`. |
| `pkg/app/` | The single-process mode: `app.New` wires a backend and a frontend together in memory. |
| `pkg/store/` | The `Store` interface and `sqlstore` (bun; SQLite and PostgreSQL; migrations; encryption of sensitive columns), and `valkeystate` (the optional Valkey home of the failed-attempt counters, single-use nonces and sessions). |
| `pkg/workers/` | The workers: processes **without a database** that talk to Withings, Strength Level and the like for the backend. One package per provider (`withings`, `strengthlevel`), the generic loop (`runner`) and the built-in list (`builtin`). Their jobs come from the queue in the backend's database, over `pkg/shared/workerapi`. |
| `pkg/estimate`, `pkg/mailer`, `pkg/models` | Domain libraries (estimation maths, SMTP, data types). |
| `cmd/`, `main.go` | The command line (`serve`, `serve web`, `serve api`, `serve worker`, `db`, `user`, `settings`, `secrets`, `info`). |
| `deploy/` | The recommended Docker Compose deployment (web, api, body, workers, PostgreSQL from ContainerForge). One Compose file with PostgreSQL and Valkey. Its `init` container is `dexafix secrets init` (`cmd/secrets_init.go`), which makes every secret and the mutual TLS certificates into volumes, and its `db-setup` container is `dexafix db provision` (`sqlstore.Provision`), which makes the database roles and the schema `body`. |
| `docs/` | User and developer documentation (numbered pages and `index.md`). The same content is served in-app from `pkg/frontend/templates/docs.html`. |

The dependency rules are enforced by tests (`go list -deps`), see [`shared.instructions.md`](.github/instructions/shared.instructions.md): the frontend and `pkg/shared` never import the backend, the store, a database library or anything that talks to the outside; the backend never imports the frontend, the workers or the body service (it uses `pkg/shared/bodyapi`); the body service never imports the backend, the frontend, the workers, the mailer or a sign-in library; the workers never import the backend, the frontend, the store or a database library.

## Golden rules

1. **Keep changes minimal.** Stay within the requested area. Do not reformat, reorder or "clean up" unrelated files.
2. **Only the backend touches the database or a key; only workers touch the outside.** The frontend and the workers have no database access. Anything that reads or writes data, checks a password, seals or opens a cookie, sends mail or talks to a provider belongs in the backend, behind an internal API endpoint. The compiler and the boundary tests enforce this; do not work around them.
3. **The backend trusts nothing from the frontend.** It authorises every request again (own data only, admin role, step-up proof), validates every value again, and never takes ownership or roles from a request body.
4. **State that must agree across processes lives in the database, or in Valkey when one is configured.** The job queue (queued, running and finished jobs, leases, schedules, worker liveness) is in tables always. Single-use nonces, failed-attempt counters and sessions are in tables (`used_nonces`, `auth_failures`, `sessions`) or, with `--valkey-url` on `serve api`, in Valkey (`pkg/store/valkeystate`), so that a flood of wrong passwords or of signed-in visitors costs the database nothing; the two are never mixed, and Valkey runs with `maxmemory-policy noeviction`. Memory is for caches only (estimate cache, OIDC discovery cache, audit-noise de-duplication). Security checks that cannot reach the database **or Valkey** **fail closed**. Sessions in Valkey are ended on purpose (sign-out, password change or reset, a deleted account), never left to a cache, and the user row (role) is read from the database on every request.
5. **Settings live in the admin UI, not in flags or environment variables.** Runtime settings (base URL, trusted proxies, PROXY protocol, TLS, SMTP, retention, provider credentials) are stored in the database and applied without a restart. A new flag or variable is only for what must be known before the database is open or before the backend is reachable (listen addresses, database, key and secret files, backend URL, mutual TLS files).
6. **Secrets never appear on a command line or in a URL.** Flags carry paths to files; values come from files or the environment. Tokens travel in headers. Do not log secrets, keys, tokens, passwords or full cookies.
7. **Released schemas change only by a new migration.** v0.0.1 is out, so never edit `00001_init.sql`. Add a numbered migration with the same file name in `pkg/store/sqlstore/migrations/{sqlite,postgres}/` (a working `Down` section too), and test the upgrade of existing data. `TestMigrationParity` checks that the two agree.
8. **Keep behaviour, strings and status codes** when you move or refactor code: tests and users depend on them.
9. **Keep the documentation true.** A change to a flag, setting, route, cookie, header or deployment step updates `docs/`, the in-app `docs.html` and, if it changes how agents should work, these instructions. Document only what you ran.
10. **Do not change CI workflows, release config or repository-wide tooling** unless the task is about them.
11. **Never commit secrets,** real credentials, private keys or certificates. The `deploy/secrets` directory is created locally and must stay out of git.

## Build, test, validate

```bash
go build ./... && go vet ./...        # also: gofmt -l . must print nothing
go test -race -count=1 ./...          # SQLite in memory; this is what CI runs
DEXAFIX_TEST_SPLIT=1 go test -race -count=1 ./pkg/backend/... ./pkg/frontend   # every browser-level test against a real TCP split
```

- PostgreSQL: `docker run -d --rm --name dexa-pg -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:18`, then `DEXAFIX_TEST_POSTGRES_DSN='postgres://postgres:x@localhost:5432/postgres?sslmode=disable' go test ./pkg/store/sqlstore/` (each test gets its own schema). If the Docker daemon is on another host than your shell, use the container's IP instead of `localhost`.
- Test output is noisy JSON log lines. Filter with `| grep -v '^{'` and redirect long output to a file.
- Run the real thing before you call a deployment change done: build the binary, start `serve body`, `serve api` (with `--body-url`), `serve web` and `serve worker` with generated secrets (see [`docs/15-split-deployment.md`](docs/15-split-deployment.md) and [`docs/16-workers.md`](docs/16-workers.md)), and use it.
- CI also runs `go mod tidy` (no diff allowed), `govulncheck` and a build matrix.

## Security

- Report vulnerabilities privately, not in public issues or PRs.
- Password checks are expensive on purpose and capped; failed attempts are counted per login name and per client address (IPv6 per /64) in the database; the answers for unknown and known accounts look the same.
- Sessions and personal access tokens are stored hashed. Sealed cookies (second step, passkey ceremony, provider state, admin step-up) are HMAC-sealed with a key that only the backend holds, and carry a single-use nonce.
- Personal access tokens, Basic credentials and provider ID tokens work on `/api/` only, never on the internal API or the web forms.
- The workers' endpoints (`/internal/v1/worker`) take the **worker secret** only, and the web frontend's service secret does not open them (and the reverse). The two secrets must differ (`serve api` refuses to start otherwise). A worker sees a user's credentials only inside the lease of one job, and every call about a job carries its lease token.
- `X-Dexafix-*` request headers are believed only after the service secret checked out, and never on the public listener.
- Public traffic gets at most half of the database connections, so the pages keep working under an API flood. Do not raise limits without a reason.

## Commits and pull requests

- Conventional prefixes as in the history: `feat:`, `fix:`, `refactor:`, `docs:`, `perf:`, `ci:`, `build:`, `chore:`, followed by a short summary in the imperative. Describe the why in the body when it is not obvious.
- One logical change per commit; keep the tree building and the tests green at each commit.
- A PR description says what changed, how you tested it (which of the commands above) and what you did not test.

## Definition of done

1. `gofmt -l .` is empty; `go build ./... && go vet ./...` pass.
2. `go test -race -count=1 ./...` passes, and the split mode and PostgreSQL runs pass when you touched the store, the internal API or the frontend.
3. New and changed code has tests next to it, at the level where it can fail: unit test, backend flow test, and a split test when it crosses the process boundary. The merged coverage of the test modes stays at 100% of statements; [`tests.instructions.md`](.github/instructions/tests.instructions.md) says how to measure it and how to cover an error branch.
4. Docs and the in-app docs are updated; user-visible strings are unchanged unless the task changes them.
5. No new dependency from the frontend or `pkg/shared` on forbidden packages (the boundary tests say so).
