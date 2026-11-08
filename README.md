# dexa-fix

Corrects smart-scale results using DEXA scan results. A Go web app that stores
per-user DEXA scans and scale readings, and shows one estimated value per day.

Each user has an account and sees only their own data. Users sign in with a
password or with an external provider. Read
[Limitations and security](#limitations-and-security) before you expose it.

## How it works

A smart scale is convenient but biased. A DEXA scan is accurate but rare. The
app uses the difference between the two (the offset) to correct the scale
between scans.

- Stored readings are never changed. Everything below is calculated per request.
- First the app builds its own per-day series: the most accurate reading of
  each day, gaps filled in, and a centred 7-day rolling average.
- The offset is `DEXA - series` on the day of each DEXA scan.
- On a day with a DEXA value, that value is used.
- On every other day, the series is corrected by the nearby offset.

Example: one DEXA scan on 2026-03-01 with weight 80.0 kg, while the scale says
82.0 kg every day. The series is 82.0 kg, so the offset is -2.0 kg.

| Date       | Scale (kg) | Estimate (kg) | Origin     |
| ---------- | ---------- | ------------- | ---------- |
| 2026-03-01 | 82.0       | 80.0          | `dexa`     |
| 2026-03-02 | 82.0       | 80.0          | `measured` |
| 2026-03-05 | none       | 80.0          | `interpolated` or `held` |

The full rules are in [Estimation details](#estimation-details).

## Features

- Accounts with password sign-in, plus OpenID Connect, Sign in with Apple and
  OAuth 2.0 providers. See [Accounts and sign-in](#accounts-and-sign-in).
- Per-user DEXA scans and scale readings, entered in the browser or by JSON API.
- Each reading records its measurement method (smart scale, caliper, BodPod,
  hydrostatic, tape, visual AI) and whether athlete mode was on.
- One estimate per day for weight, body fat, fat mass, fat-free mass, muscle
  mass, hydration, bone mass and visceral fat index.
- Optional Withings import, so scale readings do not need typing in.
- SQLite (default) or Postgres. Schema changes are versioned migrations, applied
  on startup. See [Database](#database).

## Quick start

Requires the Go version in `go.mod`.

```bash
go run . serve
```

Open <http://localhost:8080>, create an account and enter some data. The first
account becomes the administrator. By default the SQLite database is
`dexa-fix.db` in the current directory.

To build a binary:

```bash
go build -o dexa-fix .
./dexa-fix serve
```

`.goreleaser.yaml` builds release binaries for linux, darwin and windows on
amd64 and arm64.

## Configuration

Each `serve` flag has an environment variable. A flag on the command line
overrides the variable.

| Flag                       | Environment variable             | Default                                                               | Description                                            |
| -------------------------- | -------------------------------- | --------------------------------------------------------------------- | ------------------------------------------------------ |
| `--listen`                 | `DEXAFIX_LISTEN`                 | `:8080`                                                               | Address to listen on.                                  |
| `--db-driver`              | `DEXAFIX_DB_DRIVER`              | `sqlite`                                                              | `sqlite` or `postgres`.                                |
| `--dsn`                    | `DEXAFIX_DSN`                    | `file:dexa-fix.db`                                                    | Database connection string.                            |
| `--auto-migrate`            | `DEXAFIX_AUTO_MIGRATE`           | `true`                                                                | Apply pending migrations on startup; when `false`, `serve` fails if any are pending. |
| `--withings-client-id`     | `DEXAFIX_WITHINGS_CLIENT_ID`     | empty                                                                 | Withings application client id.                        |
| `--withings-client-secret` | `DEXAFIX_WITHINGS_CLIENT_SECRET` | empty                                                                 | Withings application client secret.                    |
| `--withings-redirect-url`  | `DEXAFIX_WITHINGS_REDIRECT_URL`  | `http://localhost:8080/integrations/withings/callback`                | Callback URL registered with the Withings application. |
| `--force-password-login`   | `DEXAFIX_FORCE_PASSWORD_LOGIN`   | `false`                                                               | Keep password sign-in on, to recover from a lockout.   |
| `--encryption-key-file`    | `DEXAFIX_ENCRYPTION_KEY_FILE`    | `dexa-fix.key`                                                        | Key file for encrypted values. Created if missing.     |
| (none)                     | `DEXAFIX_ENCRYPTION_KEY`         | unset                                                                 | Key itself, overrides the key file. No flag.           |

Other environment variables:

- `LOGLEVEL` sets the log level: `trace`, `debug`, `info`, `warn`, `error`,
  `fatal` or `panic`. The default is `info`.
- `DEBUG` turns off coloured console output when set to any value. It does not
  change the log level.

Postgres example:

```bash
go run . serve --db-driver postgres --dsn postgres://user:pass@localhost/dexafix
```

`dexa-fix info` prints information about the binary.

## Database

The store uses [bun](https://bun.uptrace.dev/), so SQLite and Postgres share one
query path. The schema is versioned with [goose](https://github.com/pressly/goose)
migrations, embedded per backend in `pkg/store/sqlstore/migrations/{sqlite,postgres}/`.

`serve` applies pending migrations on startup. It refuses to start if the
database schema is newer than the binary, so upgrade dexa-fix first.

SQLite connections always enforce foreign keys and wait up to 5 seconds for
locks, so the DSN needs no pragmas. The default DSN is `file:dexa-fix.db`.

| Command                  | Description                                                   |
| ------------------------ | ------------------------------------------------------------- |
| `db migrate [up]`        | Apply all pending migrations. This is the default action.     |
| `db migrate down`        | Roll back the latest migration.                               |
| `db migrate to N`        | Migrate up or down to version N. `0` empties the schema.      |
| `db migrate status`      | List every migration and whether it is applied.               |
| `db migrate version`     | Print the current schema version.                             |
| `db copy`                | Copy all data to another database. See below.                 |

`db migrate` takes `--db-driver` and `--dsn`, with the same environment
variables as `serve`. `db copy` takes `--from-driver`, `--from-dsn`,
`--to-driver` and `--to-dsn`. `--from-dsn` defaults to `DEXAFIX_DSN`.

```bash
go run . db migrate status
go run . db migrate up --db-driver postgres --dsn postgres://user:pass@localhost/dexafix
```

### Moving from SQLite to Postgres

Create an empty Postgres database, then copy and start the server:

```bash
go run . db copy \
  --from-driver sqlite --from-dsn file:dexa-fix.db \
  --to-driver postgres --to-dsn postgres://user:pass@localhost/dexafix

go run . serve --db-driver postgres --dsn postgres://user:pass@localhost/dexafix
```

The target is migrated to the latest schema and must be empty. Ids are kept,
and the Postgres sequences are reset so new rows do not collide. The copy is a
single transaction, so a failure leaves the target untouched. The source is not
changed, but it must already be fully migrated (`db migrate up`).

`db copy` copies encrypted values unchanged, so the target database needs the
same encryption key as the source. Give the server that starts on the target
the same `DEXAFIX_ENCRYPTION_KEY` or key file. See [Encryption](#encryption).

## Using the app

- `/`: the sign-in page. Sign in with a name and password, create an account,
  or use a provider button. Names are unique and have 1 to 64 characters.
- `/u/{id}`: redirects to the estimates page.
- `/u/{id}/estimates`: one row per day with the estimated values, an overview
  with the latest values and a trend chart, the calibrations in use, and the
  Withings buttons.
- `/u/{id}/dexa`: add and delete DEXA scans. A scan needs a date and at least
  one measurement.
- `/u/{id}/scale`: add and delete readings. The time is UTC. Choose the
  measurement method in the drop-down and tick **Athlete mode** if it applies.
- `/u/{id}/settings`: the athlete-mode default and the accuracy rank of each
  method. A Withings section connects, imports from and disconnects the user's
  Withings account. It says so when Withings credentials are missing, either
  on the Admin page or in the environment. Also:
  - **Profile**: name, e-mail and height in cm. The e-mail is contact
    information only. It is never used to match sign-ins.
  - **Access tokens**: create personal access tokens for the API.
  - **Linked accounts**: link or unlink sign-in providers.
  - **Password**: set or change the account's password.
- `/admin`: administrators only. Administrators can add users and open or
  close new accounts. The page shows the server status and has these
  sections:
  - **Users**: grant or remove administrator, set a password, or delete a user
    with all their data. The last administrator cannot be removed.
  - **Password sign-in**: turn it on or off.
  - **Sign-in providers**: add, edit and remove providers.
  - **Withings application**: the client ID, client secret and redirect URL.
    Values saved here override the environment variables.

Numbers may use a decimal comma. Notes are limited to 500 characters.

On the estimates page, a `DEXA` badge marks a value from a DEXA scan, and a
`corr.` badge marks a corrected value. Faded values are interpolated or held.
Hovering a value shows its origin, the method and mode it is based on, and the
raw reading. The calibrations section lists each scan's offsets (DEXA minus
series) per method and mode. Days with only a DEXA scan appear too.

## Accounts and sign-in

Each user has an account and sees only their own data. The first account
created becomes the administrator. Passwords are hashed with bcrypt. They must
be 8 characters or more, and at most 72 bytes.

Administrators can close new accounts on the Admin page. While closed, nobody
can create their own account, with a password or on first sign-in through a
provider, and an administrator adds people from the Users list on the Admin
page. Existing accounts keep working. The very first account on an empty
server can always be created.

### Upgrading an existing database

Upgrading makes the oldest user in the database the administrator. The sign-in
page then offers to set that account's first password (**Claim**). The offer
stays until an administrator can sign in, so do this straight after upgrading.
Other existing users have no password until an administrator sets one on the
Admin page.

### Sign-in providers

Administrators add providers on the Admin page. There are three types:

- **OpenID Connect**, for example Google, with the issuer
  `https://accounts.google.com`.
- **Sign in with Apple**. Enter the Services ID as the client ID, then the team
  ID, the key ID, and the contents of the `.p8` key file as the secret. dexa-fix
  generates Apple's short-lived client secret from these. Apple requires HTTPS.
- **OAuth 2.0**, with the authorisation, token and userinfo URLs.

Register this callback URL with the provider:

```text
https://your-host/auth/<short name>/callback
```

The Admin page shows the callback URL for each provider. Replace `<short name>`
with the provider's short name.

Behind a reverse proxy, forward the `Host` header and set `X-Forwarded-Proto`.
The callback URL is built from them.

Users link a provider in Settings. With **Create accounts on first sign-in**
turned on, a login from an unknown account creates a new account. Accounts are
matched only by the provider's subject, never by name or e-mail. Removing a
provider also removes its links.

### Password sign-in

Password sign-in can be turned off on the Admin page. Providers and access
tokens keep working. The administrator who turns it off must have a linked
provider first.

To recover from a lockout, start the server with password sign-in forced on:

```bash
go run . serve --force-password-login
```

The same is set with `DEXAFIX_FORCE_PASSWORD_LOGIN=true`.

### Encryption

These values are encrypted with AES-256-GCM before they are stored:

- each user's Withings tokens
- sign-in provider secrets, including the Apple key
- Withings application credentials and other server settings
- e-mail addresses

Each value is tied to its row, so copying it to another row makes it unreadable.
Passwords and access tokens were already stored only as hashes. Measurements,
height and names are not encrypted by the application. For those, use disk or
database encryption.

The key comes from one of these, in this order:

1. The environment variable `DEXAFIX_ENCRYPTION_KEY`: 32 bytes as 64
   hexadecimal characters or as base64. There is no command-line flag for it.
2. The key file set with `--encryption-key-file` or
   `DEXAFIX_ENCRYPTION_KEY_FILE`. The default is `dexa-fix.key` in the working
   directory.

If the variable is not set and the key file does not exist, dexa-fix creates
the file with a new random key and mode 0600.

In containers, put the key file on a persistent volume, or set
`DEXAFIX_ENCRYPTION_KEY`. A new key cannot read values encrypted with an old
one. Values stored before encryption are encrypted automatically at startup.

Back up the key separately from the database. Without it, the encrypted values
cannot be read, and the server does not start, because a server setting can no
longer be read. Keeping the key next to database backups defeats the purpose.
There is no built-in key rotation.

### Storage and rate limiting

Some values are encrypted in the database. See [Encryption](#encryption) for
which ones and how to manage the key. Sign-in attempts are not rate limited.
On a public deployment, limit them at a reverse proxy.

## Withings import

Each user links their own Withings account. Imported measurements are stored
as scale readings with the source `withings`.

### Setup

1. In the Withings developer dashboard, create an application and register the
   callback URL, for example
   `https://dexa.example.com/integrations/withings/callback`.
2. Give the server the application's credentials. Either enter them in the
   Withings application section of the Admin page, or start the server with
   environment variables:

   ```bash
   export DEXAFIX_WITHINGS_CLIENT_ID=...
   export DEXAFIX_WITHINGS_CLIENT_SECRET=...
   export DEXAFIX_WITHINGS_REDIRECT_URL=https://dexa.example.com/integrations/withings/callback
   ```

   Values saved on the Admin page override the environment variables. Connecting
   is possible only when all three are set. Until then, the Settings page shows
   Withings as not set up. The redirect URL has a local default, so set it
   explicitly for any public deployment, to the URL registered with Withings.
3. On the estimates or Settings page, choose **Connect Withings** and grant
   access. Finish within 10 minutes, the lifetime of the connection state.
4. Choose **Import from Withings**.

**Disconnect Withings** forgets the user's tokens. Imported readings are kept.

Imported: weight, fat ratio, fat mass, fat-free mass, muscle mass, hydration,
bone mass and visceral fat. Measurements Withings marks as possibly belonging
to someone else (for example a guest) are ignored. Days are UTC days.

### Bulk import

**Import from Withings** fetches the whole history. Each day is handled by what
is already stored:

| Day has                | What happens                                                                                          |
| ---------------------- | ----------------------------------------------------------------------------------------------------- |
| No readings            | Imported first.                                                                                       |
| Withings readings only | Changed readings are updated, identical ones are left alone, new ones are added. Nothing deleted.     |
| Manual readings        | New Withings readings are skipped and the day is listed in the result. Earlier imports are refreshed. |

A manual entry is a deliberate value. Averaging it with a Withings reading of
the same weigh-in would distort the day, so bulk import leaves such days alone.

### Re-import one day

Use **↻** on a row of the estimates page, or `?day=` on the API, to re-import
one day. It replaces that day's Withings data:

- Adds the day's Withings readings, even when manual readings exist.
- Updates changed readings.
- Removes stored Withings readings that Withings no longer returns, for
  example after a weigh-in was deleted in the Withings app.
- Never touches manual readings.

## Estimation details

Estimates are computed from the stored data on each request. Nothing is cached,
and stored readings are never changed. Each field is handled on its own.

1. **Daily average.** Readings are averaged per day, for each field and each
   group. A group is a measurement method plus athlete mode. Missing values are
   ignored.
2. **Most accurate reading.** Each day uses the group whose method ranks
   highest for that field. The default order, most accurate first, is
   hydrostatic, BodPod, caliper, smart scale, tape, visual AI. Change it on the
   settings page. A method without a value for the field is skipped, so a
   caliper reading with only body fat does not hide the scale weight. If the
   normal and athlete group of one method both have a value, the one matching
   the user's athlete-mode default wins.
3. **Missing days.** From the first reading until today, gaps are filled by a
   straight line between the known days (origin `interpolated`). After the last
   reading the last value is repeated (origin `held`).
4. **Smoothing.** The filled series is replaced by a centred 7-day rolling
   average (3 days before, 3 after). The window is shorter at the ends of the
   series. Peaks are not capped. This is the per-day series.
5. **Calibration.** On each DEXA day inside the series, the offset (DEXA minus
   series) is recorded for weight, body fat, fat mass, fat-free mass and bone
   mass. It belongs to the group used around that day (the most common group
   within 3 days, the more accurate one on a tie), so athlete and normal
   readings of the same method get their own offsets. Muscle mass, hydration
   and visceral fat index are never corrected, because DEXA does not measure
   them here. A scan before the first reading cannot calibrate.
6. **Estimate.** For each field and day:
   - A DEXA value is used as it is (origin `dexa`).
   - Before the first calibration of the day's group the series is used as it
     is.
   - Between two calibrations of the group the offset is blended with a
     smoothstep curve. The first half stays near the earlier scan, and the
     second half moves towards the later one.
   - After the last calibration the last offset is kept.
   - Corrected values have `corrected: true`.

## JSON API

All routes are under `/api/v1`. Bodies are JSON, and unknown request fields are
rejected. Errors have the form `{"error": "..."}`.

Every request must be authenticated, except `GET /healthz` and static files.
Responses are 401 when the request is not authenticated, and 403 when the
user's role is not enough. See [Authentication](#authentication).

| Method | Path                            | Description                                                          |
| ------ | ------------------------------- | -------------------------------------------------------------------- |
| GET    | `/users`                        | List users. Administrators only.                                     |
| POST   | `/users`                        | Create a user. Administrators only. Body: `{"name": "...", "password": "..."}`, where `password` is optional. |
| GET    | `/users/{id}/dexa-scans`        | List DEXA scans.                                                     |
| POST   | `/users/{id}/dexa-scans`        | Add a scan. `measured_at` is required (RFC 3339).                    |
| GET    | `/users/{id}/scale-readings`    | List scale readings.                                                 |
| POST   | `/users/{id}/scale-readings`    | Add a batch: a JSON array of up to 5000 readings.                    |
| GET    | `/users/{id}/settings`          | Athlete-mode default and method ranking.                             |
| PUT    | `/users/{id}/settings`          | Replace them. Body: `{"athlete_default": true, "method_ranking": [...]}`. |
| GET    | `/users/{id}/estimates`         | Daily estimates and calibrations.                                    |
| POST   | `/users/{id}/import/{provider}` | Import from a provider. Optional `since` and `day` query parameters. |

Batch readings with an `external_id` are upserted on `source` and
`external_id`, so repeated imports do not duplicate. Readings without a
`source` get `manual`.

Readings also take `method` (`scale` by default, or `hydrostatic`, `bodpod`,
`caliper`, `tape`, `visual-ai`) and `athlete_mode`. When `athlete_mode` is left
out, the user's default applies. A re-import never changes the method or athlete
mode of a stored reading. In `method_ranking`, methods you leave out follow in
the default order.

```bash
export DEXAFIX_TOKEN=dxf_...   # personal access token from Settings

curl -s -X POST localhost:8080/api/v1/users \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' -d '{"name": "Alice"}'

curl -s -X POST localhost:8080/api/v1/users/1/dexa-scans \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"measured_at": "2026-03-01T08:00:00Z", "weight_kg": 80.0}'

curl -s -X POST localhost:8080/api/v1/users/1/scale-readings \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '[{"measured_at": "2026-03-01T07:00:00Z", "weight_kg": 82.0}]'

curl -s -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  localhost:8080/api/v1/users/1/estimates
```

### Authentication

Send one of these with each request:

- `Authorization: Bearer <personal access token>`. Tokens start with `dxf_`.
  Create them in Settings. A token is shown once and stored only as a hash.
  It works for the API only.
- `Authorization: Bearer <ID token>` from an OpenID Connect or Sign in with
  Apple provider, for an account linked to that provider. The token must be a
  JWT signed by the provider and issued for this application's client ID.
  Opaque access tokens are not accepted.
- HTTP Basic with the account name and password, while password sign-in is
  on:

  ```bash
  curl -s -u name:password localhost:8080/api/v1/users/1/estimates
  ```

- The browser session cookie, set when you sign in on the website.

A user can reach only their own `/users/{id}/...` routes. Other ids answer 404.
`GET /users` and `POST /users` are for administrators only.

### Import endpoint

```bash
curl -s -X POST -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  'localhost:8080/api/v1/users/1/import/withings?since=2026-01-01T00:00:00Z'
curl -s -X POST -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  'localhost:8080/api/v1/users/1/import/withings?day=2026-03-01'
```

- `since` is an RFC 3339 timestamp. It limits a bulk import to measurements at
  or after that time.
- `day` is a date such as `2026-03-01`. It re-imports that day. When `day` is
  set, `since` is ignored.

A successful import returns counts:

```json
{"provider":"withings","imported":3,"updated":0,"unchanged":12,"removed":0,"skipped":0}
```

`skipped_days` (a list of `YYYY-MM-DD`) appears only when `skipped` is above
zero.

| Status | Meaning                                         |
| ------ | ----------------------------------------------- |
| 400    | `since` or `day` is malformed.                  |
| 404    | Unknown provider or user.                       |
| 409    | The user has not connected the provider.        |
| 501    | The server has no credentials for the provider. |

### Other routes

- `GET /healthz` returns `ok`.
- `GET /u/{id}/integrations/{provider}/connect` starts OAuth. The provider then
  calls `GET /integrations/{provider}/callback`. The callback is not per user,
  because Withings accepts one redirect URL. A short-lived cookie set by
  `connect` identifies the user. Both return 404 when the provider is unknown
  or not configured.
- `POST /u/{id}/integrations/{provider}/disconnect` forgets the user's tokens.

## Development

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

### Layout

- `main.go`: entry point. Sets up logging and prints the version.
- `cmd/`: `serve` runs the web app, `db` migrates and copies the database,
  `info` prints binary information.
- `pkg/estimate/`: estimation logic.
- `pkg/importer/`: the importer interfaces and the registry.
- `pkg/importer/withings/`: OAuth and API client (`client.go`), the rules for
  what to write (`plan.go`), and the importer (`withings.go`).
- `pkg/models/`: domain types.
- `pkg/store/`: the `Store` interface. `sqlstore/` implements it with bun for
  SQLite and Postgres. Its goose migrations live under `migrations/`.
- `pkg/web/`: HTML handlers and templates, the JSON API (`api.go`), the
  integration routes (`handlers_import.go`), and routes in `server.go`.

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.

### Schema changes

Every schema change adds a migration with the same version number to both
`pkg/store/sqlstore/migrations/sqlite/` and `migrations/postgres/`, for example
`00002_add_notes.sql`. Each file has `-- +goose Up` and `-- +goose Down`
sections. `TestMigrationParity` fails if the two directories differ.

### Adding an importer

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

## Limitations and security

- **Sign-in attempts are not rate limited.** On a public deployment, limit them
  at a reverse proxy. See [Storage and rate limiting](#storage-and-rate-limiting).
- **Measurements, height and names are not encrypted.** Protect the database
  file or Postgres database and its backups with disk or database encryption.
  Losing the encryption key makes the encrypted values unreadable. See
  [Encryption](#encryption).
- **Behind a proxy.** The OAuth state cookie is marked `Secure` only when the
  app itself serves HTTPS, so the proxy should enforce HTTPS. The Withings
  redirect URL must be the public URL.
- Estimates are recalculated on every request. Nothing is cached.
