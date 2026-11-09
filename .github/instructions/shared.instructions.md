---
applyTo: "pkg/shared/**"
paths:
  - "pkg/shared/**"
description: "Rules for code both the frontend and the backend use: what may live here, the dependency rules, the apiclient contract."
---

# Shared packages

`pkg/shared/` holds what the frontend and the backend both need. Packages are small and named by function:

| Package | Role |
| --- | --- |
| `apiclient` | The contract between the processes: client, DTOs, header and cookie names, error codes, the in-process transport. |
| `edge` | Proxy headers, PROXY protocol, security headers, compression, request limiter. |
| `mtls` | Mutual TLS: certificate generation, server and client configs that reload their files. |
| `jobs` | The background job group. |
| `timezones`, `tlsutil` | The common zone list; PEM certificate parsing. |

## Rules

- **Imports:** standard library, `pkg/models`, `pkg/estimate`-style pure libraries and a few small third-party packages only. Never `pkg/frontend`, `pkg/backend`, `pkg/app`, `pkg/store`, a database library, `go-oidc` or `go-webauthn`. `pkg/shared/boundary_test.go` checks it, and `pkg/frontend/boundary_test.go` checks the frontend's whole dependency tree.
- **No grab-bag.** Do not add a `util` or `common` package. A helper used by only one side stays on that side, even if a similar helper exists on the other (a few duplicated lines are better than a shared dependency).
- Something belongs here only when both sides need the same code or the same names, and it has no state of its own that depends on one side.
- **`apiclient` is a compatibility surface between two processes of the same release.** Change DTOs deliberately, keep them JSON-round-trippable, and update both sides in one change.
- **`edge`** code sits on the hot path of every request and is security sensitive (trusted proxies, forwarded headers, limiter). Add tests for every rule, including the negative case ("an untrusted peer cannot set X-Forwarded-For").
- **`mtls`**: TLS 1.3, client certificates verified against the configured CA, certificate files re-read when they change, a broken file keeps the last good one. Do not weaken any of these, and keep the service secret requirement independent of it.
