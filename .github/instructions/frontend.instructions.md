---
applyTo: "pkg/frontend/**"
paths:
  - "pkg/frontend/**"
description: "Rules for the web frontend process: no database access, everything through the backend's internal API, listeners, request limits. Templates and static files have their own file."
---

# Frontend instructions

These rules apply to `pkg/frontend/` (package `frontend`), the process started by `dexa-fix serve web`. They add to the general rules in [`AGENTS.md`](../../AGENTS.md). Templates and static files: [`frontend-templates.instructions.md`](frontend-templates.instructions.md).

## What the frontend is

It renders pages and owns the public listeners (plain HTTP, HTTPS, HTTP/3). It has **no database, no keys and no providers**. For everything else it calls the backend through `pkg/shared/apiclient` (`s.api`). A handler does four things: read the form, call the backend, render or redirect, map errors.

## Hard rules

- **Never import** the backend, `pkg/app`, `pkg/store` (anything under it), `pkg/workers`, `pkg/mailer`, a database driver or query library, `go-oidc` or `go-webauthn`. `boundary_test.go` runs `go list -deps` and fails on them. If you need data, add an internal API endpoint instead (see [`backend-internal-api.instructions.md`](backend-internal-api.instructions.md)).
- **Never seal, open or inspect sensitive cookies or tokens.** The frontend forwards the browser's `Cookie` header and relays the backend's `Set-Cookie` (done by `apiclient.WithRequest` in `withVisitor`). Do not parse the session cookie value, and never log cookies.
- **Do not decide anything about security here.** Guards like `s.user`, `s.adminPage` only redirect for convenience; the backend enforces the real rules again.
- **Keep user-visible strings and status codes.** Messages for failed steps come from the backend (`apiclient.Error.Message`, `SignInOutcome.Error`); render them as they are.

## Patterns

- Forms are parsed here (`forms.go`: `parseMeasures`, `parseWhen`, ...) into typed values and sent to the backend, which validates them again.
- Errors: `s.fail(w, err)` maps only `apiclient` errors (`ErrUnavailable` becomes the 503 page with `Retry-After`; `*apiclient.Error` by status). To re-render a form with a message use `apiclient.IsCode(err, apiclient.CodeInvalid)` and friends.
- Settings the frontend acts on (base URL, trusted proxies, PROXY protocol, TLS material) come from the backend: `RefreshConfig` (ETag, polled every ~5 s by `StartConfigPoll`). After an Admin save that changes network or TLS settings the handler calls `afterAdminChange`, which refreshes before redirecting. Do not store such settings in the frontend.
- Request limits (`frontendLimiter`: concurrency cap plus per-address rate, stricter for unauthenticated POSTs) protect the backend. Do not bypass them; tests turn them off with `DisableLimits`.
- The listeners (`StartListeners`, `applyListeners`, `startHTTPS`, `startHTTP3`) follow the settings; a wrong certificate or port must never take down the plain listener.
- `/healthz` never calls the backend; `/readyz` does.
- **`/status` is the same for everybody** (a state per part: web, api, database, body, workers per provider; 503 while one is down) and holds no version, name or reason. The detail is a separate page, `/admin/status`, behind the admin guard. The backend decides what each may see (`GET /internal/v1/status`, `/admin/status`); never merge the two into one URL that varies by cookie.
- **Nothing personal may be cached by a shared cache or a CDN.** `edge.SecureHeaders` sets `Cache-Control`, `CDN-Cache-Control` and `Surrogate-Control` to `no-store` on everything except `/static/` (use `edge.NoStore` for a response you write by hand, such as an error). The API proxy keeps the upstream's browser caching but never lets `public` or nothing through. `cache_test.go` in `pkg/backend/main` asserts it for every kind of route; add a case when you add a kind of response.
- `/api/` is not served here: it is passed to the backend only when `--api-upstream` is set (`SetAPIUpstream`), with forwarded headers rebuilt from what this process settled, never copied.

## Tests

Frontend tests live in `pkg/frontend/*_test.go`; they may import the backend and `sqlstore` **in test files** to build a real backend (see `frontend_test.go`, `stack_test.go`). The browser-level flow tests live with the backend ([`tests.instructions.md`](tests.instructions.md)).
