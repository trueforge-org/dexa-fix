---
applyTo: "pkg/estimate/**,pkg/mailer/**,pkg/models/**"
paths:
  - "pkg/estimate/**"
  - "pkg/mailer/**"
  - "pkg/models/**"
description: "Rules for the domain libraries: estimation maths, SMTP and the data types."
---

# Domain libraries

These packages hold the domain logic and the types. They are used by the backend; `pkg/models` and `pkg/estimate` are also used by the frontend and `pkg/shared`.

- **No dependency on the web layers or the store.** They must not import `pkg/frontend`, `pkg/backend`, `pkg/app` or `pkg/store`. `pkg/models` and `pkg/estimate` are pure data and maths: no I/O, no global state, no logging of user data. (The frontend's dependency tree includes them, so they must stay free of database and network libraries too.)
- `pkg/estimate`: numerical code. Keep it deterministic; changes need table tests with known values and, for hot paths, the existing benchmarks (`go test -bench . ./pkg/estimate`).
- `pkg/mailer`: SMTP with explicit TLS mode; no header injection (validate addresses and subjects); test with a fake SMTP server.
- `pkg/models`: JSON tags are part of the public API (`docs/11-api.md`) for the types the API returns. Fields hidden with `json:"-"` are hidden on purpose; `apiclient.SessionUser` carries them to the frontend.
- Use table-driven tests and no network access in unit tests.
