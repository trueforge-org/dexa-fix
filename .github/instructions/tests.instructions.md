---
applyTo: "**/*_test.go"
paths:
  - "**/*_test.go"
description: "How to write and run tests here: the test modes, the backend stack harness, Postgres, split mode, no weakened assertions, noisy logs."
---

# Test instructions

## Where tests live

- `pkg/backend/main/*_test.go` (white-box, `package mainbackend`): the backend's internals **and all browser-level flow tests**. `stack_test.go` builds a backend plus a frontend and exposes helpers (`newAuthServer`, `signup`, `serverOf`, `apiCall`, ...). In `DEXAFIX_TEST_SPLIT=1` mode the frontend talks to the backend over a real TCP connection and has no store.
- `pkg/frontend/*_test.go`: frontend internals (listeners, TLS, config, forms, docs, boundary). They may import the backend and `sqlstore` in tests to build a backend.
- `pkg/shared/**`, `pkg/store/sqlstore`, `cmd`, `pkg/app`, domain libraries: next to their code.
- A new internal endpoint needs a test over a real TCP split (`splitClient` helpers) as well as the normal flow test; see [`backend-internal-api.instructions.md`](backend-internal-api.instructions.md).

## Running

```bash
go test -race -count=1 ./...
DEXAFIX_TEST_SPLIT=1 go test -race -count=1 ./pkg/backend/... ./pkg/frontend
DEXAFIX_TEST_POSTGRES_DSN='postgres://postgres:x@HOST:5432/postgres?sslmode=disable' go test ./pkg/store/sqlstore/
```

The output contains many JSON log lines: filter with `| grep -v '^{'`, or redirect to a file and grep for `^(ok|FAIL|--- FAIL|panic)`. In this repository test, lint and validation runs go to the `haiku-triage` agent (see `CLAUDE.md`).

## Writing tests

- **Do not weaken or delete an assertion to make a test pass.** A failing test tells you about behaviour; fix the code, or change the test only when the behaviour change is the point of the task and say so.
- Test the failure case of every security rule: wrong secret, missing client certificate, another user's id, replayed state, expired value, locked-out address.
- No fixed ports and no fixed sleeps: use `httptest`, free ports (`tlsFreePort`) and polling with a deadline. Shorten intervals through package variables (`configPollInterval`, `settingsPollInterval`, `bcryptWait`) and restore them with `t.Cleanup`.
- Tests that cover combined-server internals (listener rigs, `StartListeners`) call `skipInSplit(t)`; the split has its own tests.
- Races matter: concurrency tests run with `-race`, and claims or counters are tested with several goroutines and exactly one winner.
- Leave nothing behind: close servers, stores and containers with `t.Cleanup`.
