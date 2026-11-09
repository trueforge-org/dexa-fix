# Splitting the backend into services

Status: proposal (2026-11-09)

## Context

Today `serve api` (`pkg/backend/main`, package `mainbackend`) is one backend. It holds every route, the only `store.Store` and the only encryption key. `serve web` (`pkg/frontend`) has no database and reaches the backend through `pkg/shared/apiclient`, using the service secret and optional mutual TLS.

The idea under consideration:

- a weight service and a DEXA service, each owning its tables and its own key;
- the main API granted access to both;
- Withings and Strength Level as workers with no database access.

## Verdict

- **Workers: yes, and they give the most security for the least work.** Both integrations are already nearly free of the database. `pkg/strengthlevel/client` uses no store, and the Withings HTTP calls and mapping (`getMeas`, `requestToken`, `mapGroups`) are pure functions. A worker would hold only outbound credentials and talk to no database.
- **Weight and DEXA services: workable, with three caveats.**
  1. **Nothing is encrypted yet.** `scale_readings` and `dexa_scans` are plaintext today, and the one key covers only email, integrations, settings, providers and TOTP. Per-domain keys only pay off once these tables are encrypted.
  2. **Estimates read both tables.** `computeEstimates` (`pkg/backend/main/estimates.go`) loads scans and readings, and so do Strength Level and export. Something must still see both in plaintext, which will be the main API calling both services. A break-in to the main API therefore still exposes everything. The split protects against a leak from one service, or a leaked dump of one schema, but not against a compromised main API.
  3. **Shared version counter.** `users.data_version` (`bumpVersion` in `pkg/store/sqlstore/sqlstore.go`) links the two domains and keys the estimate cache.

  This split mainly pays off together with **separate Postgres roles and schemas**. With separate keys but one database role, any one service can still read every table.

## Target shape

```text
web (no DB) ──mTLS──> api  (identity, auth, settings, audit, users, estimates, integration orchestration, job queue)
                        ├─mTLS─> weight-svc  (schema "weight": scale_readings; key K_w)
                        ├─mTLS─> dexa-svc    (schema "dexa":   dexa_scans;     key K_d)
                        ▲
     withings-worker ───┘ pull jobs over mTLS (no DB, egress to wbsapi.withings.net only)
     sl-worker       ───┘ pull jobs over mTLS (no DB, egress to my.strengthlevel.com only)
```

### Data services (weight-svc, dexa-svc)

- **Own data and key.** Each service owns one Postgres schema with its own database role and its own master key (`DEXAFIX_WEIGHT_KEY`, `DEXAFIX_DEXA_KEY`).
  - Reuse `sqlstore.Cipher` / `NewCipher` and the rekey logic in `pkg/store/sqlstore/crypto.go` and `rekey.go`, changing only the HKDF info strings or context prefixes for each domain.
- **What to encrypt.** Encrypt the measurement value columns, using `weight:<uid>:<id>` or `dexa:<uid>:<id>` as associated data. Keep `user_id`, the timestamp, `source` and `external_id` in plaintext so indexes and upserts still work.
- **No foreign key across schemas.** `users` belongs to the api, so `user_id` becomes an opaque reference. User deletion becomes an explicit `DELETE /users/{id}` call from the api to each service, replacing `ON DELETE CASCADE`.
- **Per-user tokens instead of the shared secret.** The api already authenticates the user. It then signs a short-lived token (user id, scopes such as `weight:read`, `weight:write`, and an audience), and the service checks it.
  - This replaces the one `X-Dexafix-Service` secret, which would let any caller act as any user.
  - Use a per-service mTLS identity. Today `pkg/shared/mtls` fixes the client CN to "dexa-fix web", so extend `Generate` and `cmd/mtls.go` to issue one leaf certificate per service.
- **Own version counter.** Each service keeps its own `data_version` per user and returns it with every list and write. The estimate cache in `estcache.go` keys on `(weight_ver, dexa_ver)`.
- **Import planning moves into weight-svc.** Withings' `plan()` (merge, fill gaps, skip, delete) goes behind an endpoint `POST /users/{id}/readings:import {source, readings, seen_ids, day?, tz}`. The rules then run next to the data.

### api (the current mainbackend, slimmer)

- **What it keeps:** auth, users, settings, admin, audit, the `user_integrations` secrets, `computeEstimates`, export, and the job queue.
- **Interfaces instead of the store.** Measurement access goes through two small interfaces (`WeightStore`, `DexaStore`) with two implementations: one in-process (the existing sqlstore) and one remote (an HTTP client).
  - This keeps plain `serve` working in one process, the way `pkg/app` already does with `apiclient.InProcess`.
  - It also lets you split one service at a time.
- **Audit.** Services send audit events back to the api, or the api records them when it makes the call. Recording in the api is simpler.

### Workers (withings-worker, sl-worker)

- **Pull model.** Workers poll `GET /internal/v1/jobs/next?kind=withings` on the api over mTLS. They open no listening ports. A job record carries everything needed, and results go back with `POST /jobs/{id}/result` and `/progress`.
- **Job queue.** Use a Postgres jobs table owned by the api (lease, attempts, state), replacing the in-memory `slJobs` and the unused `strengthlevel_jobs` table. Change `00001_init.sql` rather than adding a migration, because the project has no migrations before 1.0.
  - This also fixes progress being lost on restart.
- **Withings refresh tokens are single-use.** Today the per-process `lockUser` mutex guards against two refreshes at once. **Keep token refresh in the api, under a database lease per user,** and give the worker only a short-lived access token and a time range.
  - The worker returns mapped `[]models.ScaleReading` and the group ids it saw.
  - The api passes them to weight-svc's import endpoint.
  - The OAuth code exchange also stays in the api, because it needs the client secret.
- **Strength Level.**
  - Job payload: `{email, password, entries []strengthlevel.Entry, since, update}`.
  - The api computes the entries (`computeEstimates` + `FromEstimates`) and enforces the login rate limit.
  - The worker runs `client.Login` + `Sync` and reports `client.Result` and progress.
  - The password still passes through the worker. That is unavoidable, but the worker has no database or key.
- **Triggering.** Clicks still create jobs. Optionally add a scheduled enqueue later; that is the first time real background work would exist.

## Suggested order

Each step can ship on its own.

1. Workers plus the jobs table: the most isolation for the least work, with no data migration.
2. Encrypt `scale_readings` and `dexa_scans` in place, with per-domain derived keys still inside one process.
3. Add the `WeightStore` and `DexaStore` interfaces and per-domain version counters in mainbackend. Everything still runs in one process.
4. Move the tables to their own schemas and roles, then turn on the remote implementations and the signed per-user tokens. Add `serve weight` and `serve dexa` subcommands, plus services and networks in `deploy/docker-compose.yaml`.

## Housekeeping found while exploring

- `pkg/backend/main/backend.go` builds `slJobs{store: st}`, but `slJobs` in `strengthlevel_jobs.go` has no `store` field, so it probably does not compile.
- `docs/15-split-deployment.md` says failed logins, nonces and Strength Level progress are held in memory, but failed logins and nonces are in the database now.

## Verification

- For each step:
  - run `go test ./...`;
  - run the boundary tests, extended so workers cannot import `pkg/store` and data services cannot import auth;
  - do an end-to-end Compose run.
- Checks to run:
  - add a scale reading and a DEXA scan, and confirm the estimates are right;
  - run a Withings import, a single-day re-import and a Strength Level sync;
  - delete a user and confirm the data is gone from both schemas;
  - confirm that `psql` as the weight role cannot read `dexa.*`.
