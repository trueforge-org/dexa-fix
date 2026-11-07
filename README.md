# dexa-fix

Go web app that corrects smart-scale results using DEXA scan results.

It stores per-user DEXA scans and smart-scale readings in a database and
shows one estimated result per day.

There is no authentication. The front page lists users as buttons, and
anyone can add a name.

## Run

```bash
go run . serve
```

Flags, with the environment variable that sets the same value:

- `--listen` (`DEXAFIX_LISTEN`): address to listen on. Default `:8080`.
- `--db-driver` (`DEXAFIX_DB_DRIVER`): `sqlite` or `postgres`. Default
  `sqlite`.
- `--dsn` (`DEXAFIX_DSN`): database connection string. Default
  `file:dexa-fix.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)`.

```bash
go run . serve --db-driver postgres --dsn postgres://user:pass@localhost/dexafix
```

`LOGLEVEL` sets the log level: `trace`, `debug`, `info`, `warn`, `error`,
`fatal` or `panic`. The default is `info`.

`dexa-fix info` prints information about the binary.

## Pages

- `/`: list users, add a user, or open an existing one by name.
- `/u/{id}/estimates`: daily estimates and calibrations. `/u/{id}` redirects
  here.
- `/u/{id}/dexa`: add and delete DEXA scans.
- `/u/{id}/scale`: add and delete scale readings.

## Estimates

Estimates are computed from the stored data on each request.

- Scale readings are averaged per day for each field.
- The daily value can change by at most a fixed amount per day. For example,
  muscle mass is limited to 0.05 kg per day. The allowance grows with the
  number of days between two measured days.
- Each DEXA scan is paired with the nearest scale day within 3 days. The
  offset (DEXA minus scale) is recorded for weight, body fat, fat mass,
  fat-free mass and bone mass. Muscle mass, hydration and visceral fat index
  are not measured by DEXA here, so they are never corrected.
- On a day with a DEXA value for a field, the DEXA value is used (origin
  `dexa`).
- On a scale-only day, the scale value is corrected by an offset (origin
  `scale-corrected`):
  - Before the first scan, the value is not corrected (origin `scale`).
  - Between two scans, the offset is blended from the earlier scan to the
    later one with a smoothstep curve.
  - After the last scan, the last offset is kept constant.

## JSON API

All routes are under `/api/v1`. Request and response bodies are JSON, and
unknown request fields are rejected. Errors are returned as
`{"error": "..."}`.

| Method | Path | Description |
| ------ | ---- | ----------- |
| GET | `/users` | List users. |
| POST | `/users` | Create a user. Body: `{"name": "..."}`. |
| GET | `/users/{id}/dexa-scans` | List DEXA scans. |
| POST | `/users/{id}/dexa-scans` | Add a scan. `measured_at` required. |
| GET | `/users/{id}/scale-readings` | List scale readings. |
| POST | `/users/{id}/scale-readings` | Add a batch (JSON array, max 5000). |
| GET | `/users/{id}/estimates` | Daily estimates and calibrations. |
| POST | `/users/{id}/import/{provider}` | Import. `?since=` is optional. |

Readings in a batch with an `external_id` are upserted on
`source` and `external_id`, so repeated imports do not create duplicates.
Readings without a `source` are stored with source `manual`.

```bash
curl -s localhost:8080/api/v1/users/1/estimates
```

`since` is an RFC 3339 timestamp. The import endpoint returns 501 for
`withings`, which is not implemented yet.

Other routes:

- `GET /healthz` returns `ok`.
- `GET /u/{id}/integrations/{provider}/connect` and
  `GET /u/{id}/integrations/{provider}/callback` are placeholders for OAuth.
  They return 501 for `withings` and 404 for unknown providers.

## Layout

- `main.go`: entry point, sets up logging.
- `cmd/`: CLI commands (`serve`, `info`).
- `pkg/estimate/`: estimation logic.
- `pkg/importer/`: importer interface and registry. `withings/` is a stub.
- `pkg/models/`: domain types.
- `pkg/store/`: store interface. `sqlstore/` implements it for SQLite and
  Postgres.
- `pkg/web/`: HTML pages, JSON API and integration routes.

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.
