---
applyTo: "pkg/store/**"
paths:
  - "pkg/store/**"
description: "Rules for the Store interface, the SQL implementation, migrations (edit 00001_init.sql, no new files before 1.0), encryption and the PostgreSQL tests."
---

# Store instructions

`pkg/store` is the interface (`store.Store`) and the error values; `pkg/store/sqlstore` implements it with bun for **SQLite and PostgreSQL through one query path**. Only the backend uses it.

## Schema changes

- **There are no migration files before 1.0.** Edit `sqlstore/migrations/sqlite/00001_init.sql` **and** `sqlstore/migrations/postgres/00001_init.sql`, keep them in step (types differ: `TIMESTAMP`/`TIMESTAMPTZ`, `AUTOINCREMENT`/`BIGSERIAL`), and add the `DROP` to the `Down` section. `TestMigrationParity` fails when they disagree.
- Add the table, its row type (`rows.go` or the file of the area) and the interface method together.
- Existing databases are not migrated by this: say so in the PR when the change would break a database created by an earlier build.

## Queries

- Use bind placeholders (`?`); never build SQL from values. Identifiers come from code, not from requests.
- Times are UTC (`now()`); store and compare through the driver types.
- Everything must work on both dialects. `INSERT ... ON CONFLICT ... DO UPDATE ... WHERE` and `RowsAffected` are used for single-use claims (`UseNonce`); avoid dialect-specific functions.
- Multi-statement changes use a transaction (`RunInTx`). SQLite uses WAL, a busy timeout and a small pool (`MaxConns`); do not hold a connection across slow work.

## Encryption

Sensitive columns (settings values, secrets, TOTP seeds, tokens) go through the cipher with a context string that names the column and row (`settingContext(name)` style) so ciphertext cannot be moved between rows. A key rotation (`db rekey`) must keep working: new encrypted fields are added to the rekey logic and its tests.

## Settings and counters

`PutSetting` and `DeleteSetting` bump the settings generation in the same transaction; processes compare `SettingsGeneration` to know when to reload. Shared state tables (`used_nonces`, `auth_failures`) are purged by the backend's maintenance loop; so are finished jobs (`worker_jobs`, after the retention an administrator set) and workers not seen for a day (`worker_instances`).

## The job queue

`worker_jobs`, `worker_instances` and `worker_schedules` hold the queue the workers draw from (`jobs.go`). Every state change is one conditional `UPDATE ... WHERE id = ? AND state = ? [AND lease_token_hash = ?]` and `RowsAffected` says who won; do not read, decide and write in separate steps. Leasing orders by administrator first, then `users.job_priority` (higher first), then interactive, then oldest. `payload` and `result` are encrypted with a context that names the job (`jobPayloadContext`), the payload is cleared when a job ends, and both are in `rekey` and `encryptExisting`. A unique partial index (`dedupe_key` for queued and running jobs) is what keeps one active job per key. Test races with several goroutines and exactly one winner, and run the PostgreSQL tests too.

## Tests

`newStore` in `sqlstore_test.go` uses in-memory SQLite, and PostgreSQL (own schema per test) when `DEXAFIX_TEST_POSTGRES_DSN` is set. **Run both when you touch SQL**: `docker run -d --rm --name dexa-pg -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:18`. Test races for claims and counters (several goroutines, exactly one winner).
