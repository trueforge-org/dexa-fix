---
applyTo: "pkg/backend/**/internal*.go,pkg/shared/apiclient/**"
paths:
  - "pkg/backend/**/internal*.go"
  - "pkg/shared/apiclient/**"
description: "How to add or change an internal API endpoint: the DTO and client method in apiclient, the guarded backend handler, the frontend call, errors, cookies and tests."
---

# Internal API instructions

The internal API is `/internal/v1/...`. It is the only way the frontend gets data. It is **private to a deployment, not a supported interface**: it may change between releases, and the frontend and backend must be deployed from the same release. The public API is `/api/v1` ([`backend.instructions.md`](backend.instructions.md)).

## Adding an endpoint

1. **Contract** in `pkg/shared/apiclient/<group>.go`: JSON-round-trippable DTOs and a typed client method (`c.Get`, `c.Post`, `c.Delete`, or `c.Raw` for streams and raw bodies). `models.User` hides some fields from JSON; use `SessionUser`. apiclient imports neither `pkg/frontend`, nor the backend, nor `pkg/store`.
2. **Backend handler** in `pkg/backend/main/internal_<group>.go`, registered by one line in `internalRoutes` (`internal.go`). Wrap it in a guard: `s.internalUser` (own data, 401/404), `s.internalAdmin` (admin role plus step-up proof, 401/403/`CodeStepUp`), `s.internalAdminBasic` (admin without step-up), or none for steps before sign-in.
3. **Frontend handler** in `pkg/frontend/handlers_<group>.go`: parse, call the client method, render or redirect.
4. **Tests**: at least one over a real TCP split (`splitClient`, see `internal_signin_test.go`), covering the guard, an error code, and anything that crosses the process boundary (forwarded address, cookies, headers).
5. **Docs** if the behaviour is user-visible.

## Conventions

- Verbs and paths: `GET .../pages/<page>` returns what one page render needs in one call; mutations use `POST`, `DELETE` on resources; ids in the path (`/users/{id}/...`), never a user id in the body.
- Answer errors with `internalFail(w, status, code, message)`; `message` is the exact user-facing string. Map store and importer errors with `internalError`. Codes are in `apiclient` (`CodeInvalid`, `CodeThrottled`, `CodeForbidden`, `CodeUnauthorized`, `CodeStepUp`, `CodeNotFound`, `CodeExists`, `CodeUnavailable`, `CodeInternal`). Server errors (5xx) reach the frontend as `ErrUnavailable`.
- A step of signing in answers `SignInOutcome` (`Redirect` or `Error` and `Status`), not an HTTP error, for failures the person can fix.
- **Cookies:** the handler reads the browser's cookies as usual (`r.Cookie`) and sets them with `http.SetCookie`; `visitorContext` restores the `Cookie` header and the frontend relays `Set-Cookie`. Keep attributes (`HttpOnly`, `Secure` from `isSecure(r)`, `SameSite`, `Path`) exactly as before.
- **Visitor context:** after the service secret is verified, `visitorContext` sets the client address, host and scheme from `X-Dexafix-*`. Use `clientIP(r)`, `externalURL(r, ...)` and `isSecure(r)`; never read forwarding headers yourself.
- **Secrets and one-time values** (reset token, recovery codes, new access token) travel in headers or response bodies, never in URLs. Reset token: `X-Dexafix-Reset-Token`.
- Raw bodies the browser sends (the passkey `finish` calls) are forwarded unchanged and parsed by the backend.
- The internal API ignores `Authorization`; it authenticates the caller with the service secret (and mutual TLS when configured) and the visitor with the session cookie.
