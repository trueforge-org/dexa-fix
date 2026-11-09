---
applyTo: "cmd/**,main.go,pkg/app/**"
paths:
  - "cmd/**"
  - "main.go"
  - "pkg/app/**"
description: "Rules for the command line and process wiring: flags and environment variables, secrets handling, the serve modes, and the combined app."
---

# Command line and process wiring

`cmd/` is the cobra command line, `main.go` sets up logging, `pkg/app` builds the single-process mode.

## Flags and environment variables

- **A flag is for what must be known before the database is open or the backend is reachable**: listen addresses, database driver and DSN, key and secret files, the backend URL, TLS files. Runtime settings are Admin-page settings stored in the database; do not add flags or variables for them. (Removed variables such as `DEXAFIX_TRUSTED_PROXIES` only log a warning.)
- Every flag has an environment variable `DEXAFIX_<NAME>` through `envOr`; a flag on the command line wins.
- **Secrets are never flag values.** Flags take a path (`--dsn-file`, `--encryption-key-file`, `--service-secret-file`, `--internal-tls-key-file`, ...); a DSN with a password given as a flag is refused. Do not print secrets; commands that print data (`secrets service-secret`) print only that data on stdout. The startup banner goes to stderr so output can be redirected.
- Container-mounted secret files (`/run/secrets/...`) are exempt from the file-permission warning (`mountedSecret`).
- Validate flag combinations early with clear messages (`mTLS files without an https:// URL`, `one TLS file but not all three`).

## The modes

| Command | What runs |
| --- | --- |
| `dexa-fix serve` | Both halves and the workers in one process through `app.New` (in-process transport, random service and worker secrets). |
| `dexa-fix serve web` | `frontend.NewFrontend`: needs `--backend-url` and a service secret file, waits up to 60 s for the backend, follows its settings. No database flags. |
| `dexa-fix serve api` | `mainbackend.NewBackend`: public listener (`--listen`, may be empty), internal listener (`--internal-listen`, may be empty; requires the service secret), optional worker secret (`--worker-secret-file`), optional mutual TLS, `--maintenance`. The body service runs inside this process unless `--body-url` is set (then `--body-secret-file` and `--body-token-key-file` are required, the main database is opened `WithoutBody`, and an `https://` address needs the three `--body-*` mutual TLS files). The service, worker and body secrets must all differ (`distinctSecrets`); `--without-body` needs `--body-url`. |
| `dexa-fix serve body` | `bodybackend.New` over `sqlstore.OpenBody`: its own database (`--dsn`, `--db-driver`), its own key (`--encryption-key-file`, checked by `body_meta`), `--body-secret-file`, `--body-token-public-key-file` (public keys only: a private key is refused), optional `--body-tls-*` mutual TLS. No maintenance loop. |
| `dexa-fix serve worker` | `runner.Runner` with the built-in providers (`--providers`): needs `--backend-url` and a worker secret file, no database flags, the same mutual TLS client flags as `serve web`. |

`serveCmd` has `Args: cobra.NoArgs` so that a typo (`serve wbe`) cannot start the combined server.

## The body service flags

`bodyClient` (`cmd/body.go`) validates the `--body-*` client flags: without `--body-url` any of the others is an error; with it the secret and the signing key are required, `https://` needs the three TLS files and `http://` refuses them. The management commands (`db`, `user`, `settings`) open the main database through `dbOptions`, which adds `sqlstore.WithoutBody` for `--without-body`: a new command that opens the main database must use it, or on a deployment with a body service it creates empty body tables there. `db rekey --body` and `serve body` use `loadCipherFor` with `HasEncryptedBodyValues`. Tests that set flags on the shared commands must reset them with `t.Cleanup` (cobra keeps flag values between runs).

## When you change a flag

Update the table in `docs/02-configuration.md`, the commands in `docs/05-commands.md`, `docs/15-split-deployment.md` if it affects the two-process setup, the flags table and section in `pkg/frontend/templates/docs.html`, and the `Dockerfile` or `deploy/` if a default changes. Add a test in `cmd/` for the validation rules.

## `pkg/app`

`app` is the only package that imports both halves and the workers for serving. Keep it thin: construct, wire, expose `Handler`, `Close`, `SetVersion`, `WrapListener`. Behaviour belongs in the frontend or the backend.
