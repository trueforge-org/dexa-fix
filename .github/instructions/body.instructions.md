---
applyTo: "pkg/backend/body/**"
paths:
  - "pkg/backend/body/**"
description: "Rules for the body service: scale readings, DEXA scans, estimates and the import rules; trusts only a signed token from the main backend; knows nothing about people."
---

# Body service instructions

`pkg/backend/body` (package `bodybackend`) holds a user's scale readings and DEXA scans, computes their estimates and decides what an import from a source such as Withings stores. The main backend ([`backend.instructions.md`](backend.instructions.md)) is its only caller. The contract is `pkg/shared/bodyapi` ([`shared.instructions.md`](shared.instructions.md)).

## Hard rules

- **It knows nothing about people.** No sessions, passwords, roles, e-mail, providers, settings or audit. Never import `pkg/backend/main`, `pkg/frontend`, `pkg/workers`, `pkg/app`, `pkg/mailer`, `apiclient`, `workerapi`, `go-oidc`, `go-webauthn` or bcrypt. `boundary_test.go` checks it.
- **A call is allowed only with both the shared secret and a valid token** (`guard`): the token's scope must be enough, and its subject must be the user in the path (or 0 for a call about nobody in particular). Every failure answers the same. The data of a call is the token's subject's: never take a user id, role or ownership from a request body.
- **The token carries the user's settings** the estimates and the import depend on (`EstimateParams`: time zone, ranking, athlete default, height). Use those and nothing else; a call that needs them and has none is refused.
- **Validate again.** The main backend checked the values; check ranges, sizes and batch limits here too (`validate.go`), with the same messages.
- **Keep the slots apart:** estimates (CPU), imports and exports (long), reads (short). A flood of one must not starve the others, and no call may hold a database connection while it waits for a slot.
- **The store is a `store.BodyStore`.** Nothing here reads the users table; a user id is an opaque reference. Deleting a user is an explicit call (`DELETE /users/{id}`), and the main backend sweeps for what it missed.
- **The cache** (`estcache.go`) is keyed by the body version from the store, a hash of the settings and the day. It is the only process memory that is allowed, and it must never be the only place that knows something.

## Changing the API

Add the DTO and the client method in `pkg/shared/bodyapi`, the handler in `handlers.go` behind `guard`, and the wrapper in `pkg/backend/main/body.go`. Test the guard (wrong secret, expired token, another user's id, too small a scope) as well as the behaviour: `service_test.go` shows how, over a real HTTP server and the real client.

## Tests

White-box (`package bodybackend`). `newRig` in `service_test.go` starts a service over an in-memory store with a client. Run them with `-race`.
