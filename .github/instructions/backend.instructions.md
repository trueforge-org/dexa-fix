---
applyTo: "pkg/backend/**"
paths:
  - "pkg/backend/**"
description: "Rules for the main backend: owns the database, keys and every security decision; state in the database; internal and public API; several backends; future services."
---

# Backend instructions

These rules apply to `pkg/backend/` (today only `pkg/backend/main`, package `mainbackend`), the process started by `dexa-fix serve api`. They add to [`AGENTS.md`](../../AGENTS.md). The internal API and its contract have their own file: [`backend-internal-api.instructions.md`](backend-internal-api.instructions.md).

## What the backend is

The only owner of the store, the encryption keys, the cookie sealing key, the job queue, the mailer and provider credentials (it hands them to a worker only inside a lease), and the place where **every security decision is made**. It serves two listeners with separate load budgets: the public API (`PublicHandler`, `/api/v1`, behind the reverse proxy) and the internal API (`InternalHandler`, `/internal/v1`, for the frontend on a private network).

## Hard rules

- **Never import** `pkg/frontend` or `pkg/workers`. (`pkg/app` imports all of them and is the only place that wires them.) `boundary_test.go` checks it.
- **Never call an outside service** (Withings, Strength Level, ...) yourself: queue a job (`enqueueJob`, then `waitJob` if somebody waits) and let a worker do it. The backend's half of a provider is a descriptor, `workerprovider_<name>.go`; see [`workers.instructions.md`](workers.instructions.md).
- **Authorise on every request, again.** Own data only (`internalUser` / `apiUser`), admin role plus step-up proof for admin endpoints (`internalAdmin`). Never take a user id, role or ownership from a request body.
- **Validate again.** Values from forms were parsed by the frontend; check ranges and formats here (`validate.go`).
- **State lives in the database.** Single-use nonces (`dbNonces`), failed-attempt counters (`dbLimiter`) and the job queue are tables so that several backends agree. Do not add process memory that has to be consistent. Allowed in memory: caches keyed by the body version and a hash of the user's estimate settings (`estCache`, `estimateKeyFor`), the OIDC discovery cache, audit-noise de-duplication. Nothing about a job lives in memory: a worker may report to any backend.
- **Fail closed.** If the database cannot say whether a nonce was spent or an address is locked out, treat it as spent / locked out and log it.
- **Audit** security-relevant actions with `s.audit` / `s.auditAs` (the address is the visitor's, forwarded by the frontend). Keep the existing action names.
- **No secrets in logs, URLs or error messages.** Passwords are checked with bcrypt behind the shared slots (`bcryptSlots`); the answer for unknown and known accounts must look the same (dummy hash).
- **New runtime setting?** It belongs on the Admin page, stored through `PutSetting` (encrypted by the store) and applied by `reloadSettings`, not in a flag. If the frontend needs it, extend `FrontendConfig` (`internal_config.go`) and `applyFrontendConfig`.

## Running more than one backend

Any number of backends may share a database. Exactly one runs the maintenance loop (`--maintenance`, default true; `StartMaintenance`: audit purge, nonce and failure purge, self-signed certificate renewal). Backends created with `WithoutCertificateGeneration()` (that is `--maintenance=false`) only **load** the stored self-signed certificate; an administrator's explicit TLS save still generates it (`reloadSettingsGenerating`). Settings changed by another process are picked up by `StartSettingsPoll` through the settings generation counter. Do not add per-process singletons that break this.

## Load budgets

The public API admits at most half of the database connections (`publicInFlight`), a bounded queue and a per-address rate; the internal API has its own, larger budget. Basic-auth password checks are capped by `bcryptSlots`. Keep these separate: the point is that a flood on the public listener leaves the pages working.

## Public API

`/api/v1` is a supported interface (see `docs/11-api.md`). Change it compatibly. Personal access tokens, Basic credentials and provider ID tokens are accepted **only** on `/api/` paths (`authBearer`, `authBasic`).

## Layout

`pkg/backend` is a parent folder; the current single backend is `pkg/backend/main`. Another backend service would be a sibling folder with its own package. Do not create shared "backend common" packages or empty service folders in advance; put what both sides need in `pkg/shared`.

## Tests

Backend tests are white-box (`package mainbackend`) and include the browser-level flow tests, driven through a real frontend built in `stack_test.go`. See [`tests.instructions.md`](tests.instructions.md).
