---
applyTo: "**/*.go"
paths:
  - "**/*.go"
description: "How to write and run tests here, and the 100% coverage rule: the test modes, the backend stack harness, Postgres, split mode, covering error branches, no weakened assertions, noisy logs."
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

## Coverage

The merged coverage of the three test modes is **100%** of statements, and it stays there. This applies to every change to a `.go` file, not only to test files.

- New or changed code comes with its tests in the same commit, at the level where it can fail (unit test, backend flow test, split test when it crosses the process boundary).
- Before you call a change done, measure it (below) and check that nothing you touched is below 100%. Coverage runs go to `haiku-triage` like every other test run.
- Cover an error branch by making the real thing fail: rename a table away with `breakTable` (`pkg/backend/main/faults_test.go`, through `sqlstore.Store.ExecForTests`), wrap the store in a type that fails one method, close the body service, or point the client at a fake server that answers 500 or garbage. The test asserts what a person would notice (status code, message, a row that is or is not there).
- Do not add a seam to production code only to reach a line. A call that cannot fail (`crypto/rand.Read` since Go 1.24, key and certificate generation from fixed valid input) is not checked: ignore the result or use `must.Do` / `must.Value`, whose own test covers the panic.
- Do not game the number: no test without an assertion, no deleting code that can be reached, no files left out of the profile.
- Name tests as sentences (`TestAdminPageFailsCleanlyWhenTheSettingsTableIsGone`) and give each failure mode of one handler its own `t.Run` scenario, so a failing line of output reads as what broke.

### Measuring

```bash
S=$(mktemp -d); mkdir $S/plain $S/split
go test -count=1 -cover -coverpkg=./... ./... -args -test.gocoverdir=$S/plain
DEXAFIX_TEST_SPLIT=1 go test -count=1 -cover -coverpkg=./... ./pkg/backend/... ./pkg/frontend -args -test.gocoverdir=$S/split
go tool covdata textfmt -i=$S/plain,$S/split -o=$S/merged.out
go tool cover -func=$S/merged.out | grep -v '100.0%'
```

`-coverpkg=./...` counts a line as covered when any package's test runs it. With a PostgreSQL DSN set, add a third directory for `DEXAFIX_TEST_POSTGRES_DSN=... go test -count=1 -cover -coverpkg=./... ./pkg/store/sqlstore ./cmd -args -test.gocoverdir=$S/pg` (the store's tests and `db provision` need a real server) and merge it too. A test that makes the shared PostgreSQL roles and database holds the advisory lock `provisionLockKey`, because packages are tested in parallel. Use `go tool cover -html=$S/merged.out` to see the lines that are left.
