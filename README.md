# dexa-fix

Corrects smart-scale results using DEXA scan results. A Go web app that stores
per-user DEXA scans and scale readings, and shows one estimated value per day.

There is no authentication. The front page lists users as buttons, and anyone
can add a name. Read [Limitations and security](#limitations-and-security)
before you expose it.

## How it works

A smart scale is convenient but biased. A DEXA scan is accurate but rare. The
app uses the difference between the two (the offset) to correct the scale
between scans.

- The offset is `DEXA - scale` at each DEXA scan, using the scale reading of
  the nearest day within 3 days.
- On a day with a DEXA value, that value is used.
- On a scale-only day, the scale value is corrected by the nearby offset.

Example: one DEXA scan on 2026-03-01 with weight 80.0 kg, while the scale says
82.0 kg. The offset is -2.0 kg.

| Date       | Scale (kg) | Estimate (kg) | Origin            |
| ---------- | ---------- | ------------- | ----------------- |
| 2026-03-01 | 82.0       | 80.0          | `dexa`            |
| 2026-03-02 | 82.5       | 80.5          | `scale-corrected` |
| 2026-03-03 | 81.8       | 79.8          | `scale-corrected` |

The full rules are in [Estimation details](#estimation-details).

## Features

- Per-user DEXA scans and scale readings, entered in the browser or by JSON API.
- One estimate per day for weight, body fat, fat mass, fat-free mass, muscle
  mass, hydration, bone mass and visceral fat index.
- Optional Withings import, so scale readings do not need typing in.
- SQLite (default) or Postgres. The schema is created on startup.

## Quick start

Requires the Go version in `go.mod`.

```bash
go run . serve
```

Open <http://localhost:8080>, add a user and enter some data. By default the
SQLite database is `dexa-fix.db` in the current directory.

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
| `--dsn`                    | `DEXAFIX_DSN`                    | `file:dexa-fix.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)` | Database connection string.                            |
| `--withings-client-id`     | `DEXAFIX_WITHINGS_CLIENT_ID`     | empty                                                                 | Withings application client id.                        |
| `--withings-client-secret` | `DEXAFIX_WITHINGS_CLIENT_SECRET` | empty                                                                 | Withings application client secret.                    |
| `--withings-redirect-url`  | `DEXAFIX_WITHINGS_REDIRECT_URL`  | `http://localhost:8080/integrations/withings/callback`                | Callback URL registered with the Withings application. |

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

## Using the app

- `/`: list users, add one, or open one by name. Names have 1 to 64
  characters. Entering an existing name opens that user.
- `/u/{id}`: redirects to the estimates page.
- `/u/{id}/estimates`: one row per day with the estimated values, the
  calibrations in use, and the Withings buttons when Withings is set up.
- `/u/{id}/dexa`: add and delete DEXA scans. A scan needs a date and at least
  one measurement.
- `/u/{id}/scale`: add and delete scale readings. The time is UTC.

Numbers may use a decimal comma. Notes are limited to 500 characters.

On the estimates page, a `DEXA` badge marks a value from a DEXA scan, and a
`corr.` badge marks a corrected scale value. Hovering a value shows its origin,
and notes when the daily-change limit applied. The calibrations section lists
each scan's offsets (DEXA minus scale). Days with only a DEXA scan appear too.

## Withings import

Each user links their own Withings account. Imported measurements are stored
as scale readings with the source `withings`.

### Setup

1. In the Withings developer dashboard, create an application and register the
   callback URL, for example
   `https://dexa.example.com/integrations/withings/callback`.
2. Start the server with the application's credentials:

   ```bash
   export DEXAFIX_WITHINGS_CLIENT_ID=...
   export DEXAFIX_WITHINGS_CLIENT_SECRET=...
   export DEXAFIX_WITHINGS_REDIRECT_URL=https://dexa.example.com/integrations/withings/callback
   ```

   The Withings buttons appear only when all three are set. The redirect URL
   has a local default, so set it explicitly for any public deployment, to the
   URL registered with Withings.
3. On the estimates page, choose **Connect Withings** and grant access. Finish
   within 10 minutes, the lifetime of the connection state.
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

Estimates are computed from the stored data on each request. Nothing is cached.

1. **Daily average.** Readings are averaged per day for each field, ignoring
   missing values.
2. **Daily-change limit.** The daily value may change by at most a fixed amount
   per day, and the allowance grows with the days between two measured days.
   Per day: weight 1.0 kg, body fat 0.3 %, fat mass 0.2 kg, fat-free mass
   0.5 kg, bone mass 0.005 kg, muscle mass 0.05 kg, hydration 1.0 kg, visceral
   fat index 0.2.
3. **Calibration.** Each DEXA scan is paired with the nearest scale day within
   3 days. The offset (DEXA minus scale) is recorded for weight, body fat, fat
   mass, fat-free mass and bone mass. Muscle mass, hydration and visceral fat
   index are never corrected, because DEXA does not measure them here.
4. **Estimate.** For each field and day:
   - A DEXA value is used as it is (origin `dexa`).
   - Before the first calibration the scale value is used (origin `scale`).
   - Between two calibrations the offset is blended with a smoothstep curve
     (origin `scale-corrected`). The first half stays near the earlier scan,
     and the second half moves towards the later one.
   - After the last calibration the last offset is kept (origin
     `scale-corrected`).

## JSON API

All routes are under `/api/v1`. Bodies are JSON, and unknown request fields are
rejected. Errors have the form `{"error": "..."}`.

| Method | Path                            | Description                                                          |
| ------ | ------------------------------- | -------------------------------------------------------------------- |
| GET    | `/users`                        | List users.                                                          |
| POST   | `/users`                        | Create a user. Body: `{"name": "..."}`.                              |
| GET    | `/users/{id}/dexa-scans`        | List DEXA scans.                                                     |
| POST   | `/users/{id}/dexa-scans`        | Add a scan. `measured_at` is required (RFC 3339).                    |
| GET    | `/users/{id}/scale-readings`    | List scale readings.                                                 |
| POST   | `/users/{id}/scale-readings`    | Add a batch: a JSON array of up to 5000 readings.                    |
| GET    | `/users/{id}/estimates`         | Daily estimates and calibrations.                                    |
| POST   | `/users/{id}/import/{provider}` | Import from a provider. Optional `since` and `day` query parameters. |

Batch readings with an `external_id` are upserted on `source` and
`external_id`, so repeated imports do not duplicate. Readings without a
`source` get `manual`.

```bash
curl -s -X POST localhost:8080/api/v1/users \
  -H 'Content-Type: application/json' -d '{"name": "Alice"}'

curl -s -X POST localhost:8080/api/v1/users/1/dexa-scans \
  -H 'Content-Type: application/json' \
  -d '{"measured_at": "2026-03-01T08:00:00Z", "weight_kg": 80.0}'

curl -s -X POST localhost:8080/api/v1/users/1/scale-readings \
  -H 'Content-Type: application/json' \
  -d '[{"measured_at": "2026-03-01T07:00:00Z", "weight_kg": 82.0}]'

curl -s localhost:8080/api/v1/users/1/estimates
```

### Import endpoint

```bash
curl -s -X POST 'localhost:8080/api/v1/users/1/import/withings?since=2026-01-01T00:00:00Z'
curl -s -X POST 'localhost:8080/api/v1/users/1/import/withings?day=2026-03-01'
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

### Layout

- `main.go`: entry point. Sets up logging and prints the version.
- `cmd/`: `serve` runs the web app, `info` prints binary information.
- `pkg/estimate/`: estimation logic.
- `pkg/importer/`: the importer interfaces and the registry.
- `pkg/importer/withings/`: OAuth and API client (`client.go`), the rules for
  what to write (`plan.go`), and the importer (`withings.go`).
- `pkg/models/`: domain types.
- `pkg/store/`: the `Store` interface. `sqlstore/` implements it for SQLite and
  Postgres, with migrations under `migrations/`.
- `pkg/web/`: HTML handlers and templates, the JSON API (`api.go`), the
  integration routes (`handlers_import.go`), and routes in `server.go`.

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.

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

- **No authentication.** Anyone who can reach the app can view and change any
  user's data, add users, and trigger imports or disconnect Withings. Put it
  behind a reverse proxy that handles authentication. Do not expose it directly.
- **Withings tokens are stored unencrypted** in the `user_integrations` table.
  Protect the database file or Postgres database.
- **Behind a proxy.** The OAuth state cookie is marked `Secure` only when the
  app itself serves HTTPS, so the proxy should enforce HTTPS. The Withings
  redirect URL must be the public URL.
- Estimates are recalculated on every request. Nothing is cached.
