# JSON API

All routes are under `/api/v1`. Bodies are JSON, and unknown request fields are
rejected. Errors have the form `{"error": "..."}`.

Every request must be authenticated, except `GET /healthz` and static files.
Responses are 401 when the request is not authenticated, and 403 when the
user's role is not enough. See [Authentication](#authentication).

| Method | Path                            | Description                                                                                                                                             |
| ------ | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET    | `/users`                        | List users. Administrators only.                                                                                                                        |
| POST   | `/users`                        | Create a user. Administrators only. Body: `{"name": "...", "email": "...", "password": "..."}`. `email` is required and unique. `password` is optional. |
| GET    | `/users/{id}/dexa-scans`        | List DEXA scans.                                                                                                                                        |
| POST   | `/users/{id}/dexa-scans`        | Add a scan. `measured_at` is required (RFC 3339).                                                                                                       |
| GET    | `/users/{id}/scale-readings`    | List scale readings.                                                                                                                                    |
| POST   | `/users/{id}/scale-readings`    | Add a batch: a JSON array of up to 5000 readings.                                                                                                       |
| GET    | `/users/{id}/settings`          | Athlete-mode default and method ranking.                                                                                                                |
| PUT    | `/users/{id}/settings`          | Replace them. Body: `{"athlete_default": true, "method_ranking": [...]}`.                                                                               |
| GET    | `/users/{id}/export/profile`    | Download settings and personal details (JSON file). See [Your data](07-accounts-and-sign-in.md#your-data).                                                                        |
| GET    | `/users/{id}/export/data`       | Download all scans and readings (JSON file).                                                                                                            |
| GET    | `/users/{id}/audit`             | Events about your account, newest first.                                                                                                                |
| GET    | `/audit`                        | The whole audit log. Administrators only.                                                                                                               |
| GET    | `/users/{id}/estimates`         | Daily estimates and calibrations.                                                                                                                       |
| POST   | `/users/{id}/import/{provider}` | Import from a provider. Optional `since` and `day` query parameters.                                                                                    |

Scans and readings cannot be deleted through the API, and access tokens cannot
be created or revoked through it. Use the web interface for those. Changing a
password revokes all of the user's access tokens, and so does an administrator
setting it or a password reset, so create new tokens afterwards if you use the API.
The administrator's two-step code for the Admin pages does not apply here: API
calls use access tokens.

The two list endpoints (`dexa-scans` and `scale-readings`) return the newest
first and are paged. Query parameters: `limit` (default 1000, at most 10000),
`before` (the cursor from the previous page), `since` and `until` (RFC 3339 or
`YYYY-MM-DD`, inclusive; a plain date as `until` covers that whole day). When
there are older items, the response has an `X-Next-Cursor` header; pass its
value as `before` to get the next page. The body stays a plain JSON array. The
scale page in the web interface shows 100 readings at a time. The export
endpoints always return everything.

Batch readings with an `external_id` are upserted on `source` and
`external_id`, so repeated imports do not duplicate. Readings without a
`source` get `manual`.

Readings also take `method` (`scale` by default, or `hydrostatic`, `bodpod`,
`caliper`, `tape`, `visual-ai`, `manual-entry`) and `athlete_mode`. When `athlete_mode` is left
out, the user's default applies. A re-import never changes the method or athlete
mode of a stored reading. In `method_ranking`, methods you leave out follow in
the default order.

```bash
export DEXAFIX_TOKEN=dxf_...   # personal access token from Settings

curl -s -X POST localhost:8080/api/v1/users \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name": "Alice", "email": "alice@example.com"}'

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

## Authentication

Send one of these with each request:

- `Authorization: Bearer <personal access token>`. Tokens start with `dxf_`.
  Create them in Settings. A token is shown once and stored only as a hash.
  It works for the API only.
- `Authorization: Bearer <ID token>` from an OpenID Connect or Sign in with
  Apple provider, for an account linked to that provider. The token must be a
  JWT signed by the provider and issued for this application's client ID.
  Opaque access tokens are not accepted.
- HTTP Basic with the e-mail address and password, while password sign-in is
  on:

  ```bash
  curl -s -u you@example.com:password localhost:8080/api/v1/users/1/estimates
  ```

  This is refused for an account with two-step verification on, or required by
  an administrator, because a password alone is not enough for it. Such accounts
  use a personal access token.

- The browser session cookie, set when you sign in on the website.

A user can reach only their own `/users/{id}/...` routes. Other ids answer 404.
`GET /users` and `POST /users` are for administrators only.

## Import endpoint

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

## Other routes

- `GET /healthz` returns `ok`.
- `GET /u/{id}/integrations/{provider}/connect` starts OAuth. The provider then
  calls `GET /integrations/{provider}/callback`. The callback is not per user,
  because Withings accepts one redirect URL. A short-lived cookie set by
  `connect` identifies the user. Both return 404 when the provider is unknown
  or not configured.
- `POST /u/{id}/integrations/{provider}/disconnect` forgets the user's tokens.
