---
applyTo: "pkg/workers/**"
paths:
  - "pkg/workers/**"
description: "Rules for the workers: processes without a database that talk to outside services for the backend, the job queue contract, fail-safes, and how to add a provider."
---

# Worker instructions

`pkg/workers/` holds the code of `dexa-fix serve worker` (and of the workers the single-process mode runs inside itself): one package per provider (`withings`, `strengthlevel`) next to the generic loop (`runner`) and the list of built-in providers (`builtin`). A worker talks to an outside service for the backend. The backend owns the queue, the credentials and every decision about what is stored; its half of each provider is `pkg/backend/main/workerprovider_<name>.go`. The contract between the two is `pkg/shared/workerapi`.

## Hard rules

- **No database, no keys.** `boundary_test.go` runs `go list -deps` and fails on `pkg/backend`, `pkg/frontend`, `pkg/app`, `pkg/store`, `database/sql`, bun, pgx, sqlite and goose. A worker learns what it needs from its job: the application's credentials, the user's login or tokens and the parameters arrive in the payload of one lease, and are used for that job only. Do not cache credentials between jobs. Do not log a payload.
- **A worker fetches or sends; the backend decides.** The backend validates every result (`Apply` of the kind: source, ids, sizes), sets the user and the ids itself, and compares readings with what is stored (`reconcile.go`). Never trust a worker's idea of whose data it is.
- **Errors reach people.** The text of an error is stored with the job, shown to the administrator and, for some kinds, to the person. Write a short sentence, never include a credential, a token, a code or a response body.
- **Say what kind of failure it is.** `runner.Permanent(err)` for what no retry fixes (a refused login, a revoked token, a bad request); `runner.Retryable(err, after)` for a temporary problem, with the wait the provider asked for; anything else counts as temporary. Return `ctx.Err()` when the context ended, so the runner can tell a cancel or a shutdown from a failure. A wrong login that the backend should act on (count against the user) is an answer in the result, not a failed job (`CheckLoginResult`).
- **Renewed credentials go to the backend first.** A token that works once (Withings refresh tokens) is handed over with `job.SaveCredentials` before it is used for anything, with a few tries; if that fails the job fails permanently rather than carrying on with a pair the backend does not know.
- **Respect the context.** Every call to the outside takes the job's context: it ends at the hard time limit, when a heartbeat says the job was canceled or the lease was lost, and at shutdown. Pauses (`time.After`) select on `ctx.Done()`.
- **Pace requests.** Pauses between requests and similar tuning are options of the kind that an administrator sets (`job.Config.OptionMillis`), not constants and not flags.

## Fail-safes the framework gives, and what you must not break

Leases with heartbeats (a worker that stops is reaped), a lease token on every call (a worker that lost its job cannot report), a hard time limit per attempt, a bounded number of attempts with backoff and a dead-letter state, permanent versus temporary failures, expiry of jobs nobody picked up in time, per-user schedule pause after repeated failures, a circuit breaker per kind, cancel, release on shutdown, and retention. They live in `pkg/store/sqlstore/jobs.go`, `pkg/backend/main/{queue,internal_worker,scheduler}.go` and the runner. A new provider gets them by using the kinds, not by reimplementing them.

## Adding a provider (for example another source of scale readings)

1. **Name and kinds.** Choose the role: scale source (`<name>.connect` for OAuth providers, `<name>.import`) or exporter (`<name>.check_login`, `<name>.day`, `<name>.sync`). Their payloads and results are in `pkg/shared/workerapi` (`scalesource.go`, `exporter.go`); reuse them. Add a constant for the name to `workerapi/kinds.go`.
2. **Worker package** `pkg/workers/<name>`: a `Worker` with `Handlers() map[string]runner.Handler`. Keep the logic in plain methods that take the decoded payload and a `save` function, so tests need no runner (see `withings/worker.go`).
3. **Register it** in `pkg/workers/builtin/builtin.go` (one entry in `providers`, an option for the test base URL).
4. **Backend descriptor** `pkg/backend/main/workerprovider_<name>.go`: a `workerProvider` with its `kindSpec`s (defaults for retries, time limit, schedule, circuit breaker; options sent to workers), `Lease` (reads and decrypts what the worker needs), `Apply` (validates and stores), `Credentials` (stores renewed credentials), `ScheduleFor` (users to run on a schedule) and, for OAuth, `OAuth` (configured? authorize URL). Add it to `NewBackend`. The Admin page lists it, the scheduler runs it and the settings keys (`workers.<kind>.*`) exist without further code.
5. **Pages** that connect, import or export call `enqueueJob`/`waitJob` from the descriptor; they never call the outside service.
6. **Tests**: the worker against a fake server (no network), the descriptor's `Lease`/`Apply` including a bad result, and a flow through the real queue (`workers_test.go` has a demo provider).
7. **Docs** (`docs/16-workers.md`, the in-app copy) and this file if a rule changes.

## Tests

No fixed ports and no fixed sleeps (use `httptest` and polling with a deadline); shorten intervals through package variables and restore them in `t.Cleanup`. Stop every runner you start before the test ends (a leaked runner keeps polling and races with later tests). Test the failure of every rule: wrong token, lost lease, a result that names another user, an unreachable service.
