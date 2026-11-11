---
applyTo: "pkg/store/**"
paths:
  - "pkg/store/**"
description: "Rules for the Store interface, the SQL implementation, migrations (a new numbered migration per schema change; 00001_init.sql is frozen), encryption and the PostgreSQL tests."
---

# Store instructions

`pkg/store` is the interface (`store.Store`, which embeds `store.BodyStore`: scans, readings and their change counter) and the error values; `pkg/store/sqlstore` implements it with bun for **SQLite and PostgreSQL through one query path**. Only the backend uses it.

## Schema changes

- **v0.0.1 is released: never edit `00001_init.sql`.** Add a migration with the same number and name to `sqlstore/migrations/sqlite/` **and** `sqlstore/migrations/postgres/` (types differ: `TIMESTAMP`/`TIMESTAMPTZ`, `AUTOINCREMENT`/`BIGSERIAL`), with a working `Down` section, and migrate existing rows when it changes data. Test an upgrade from the previous version with data in it. `TestMigrationParity` fails when the two directories disagree.
- Add the table, its row type (`rows.go` or the file of the area) and the interface method together.
- Existing databases are not migrated by this: say so in the PR when the change would break a database created by an earlier build.

## User ids

User ids are **random** integers in [1, 2^53), made by `newUserID` and checked for a collision in the transaction (`freeUserID`); there is no sequence. Do not assume the first user is 1 or that ids are ordered: sort by `created_at`. Tests that want 1, 2, 3 call `sqlstore.SequentialUserIDsForTests()` from `TestMain`. Other ids (scans, readings, tokens, audit) stay counted.

## Queries

- Use bind placeholders (`?`); never build SQL from values. Identifiers come from code, not from requests.
- Times are UTC (`now()`); store and compare through the driver types.
- Everything must work on both dialects. `INSERT ... ON CONFLICT ... DO UPDATE ... WHERE` and `RowsAffected` are used for single-use claims (`UseNonce`); avoid dialect-specific functions.
- Multi-statement changes use a transaction (`RunInTx`). SQLite uses WAL, a busy timeout and a small pool (`MaxConns`); do not hold a connection across slow work.

## Encryption

Sensitive columns (settings values, secrets, TOTP seeds, tokens) go through the cipher with a context string that names the column and row (`settingContext(name)` style) so ciphertext cannot be moved between rows. A key rotation (`db rekey`) must keep working: new encrypted fields are added to the rekey logic and its tests.

## Body data

`BodyStore` (`sqlstore/body.go`) is the DEXA scans and scale readings, kept apart so the domain can live in a database of its own. Its methods take a user id as an opaque reference and never read `users`; the tables have **no foreign key** to it, so deleting a user does not delete their scans (`DeleteUserBodyData` does, called by the main backend, which also sweeps for orphans with `BodyUserIDs`).

- Every change bumps `body_user_state.data_version` in the same transaction (`bumpBodyVersion`); `users.data_version` only follows the user's settings, height and time zone. A new body write path must bump the counter, and a new body table must be in `Copy`.
- What identifies and orders a row stays in plain columns (indexes, paging and the upsert need them). The measured values and the notes are one JSON document in `data`, sealed with the **body cipher** (`s.bodyCipher`, derived from the master key under its own HKDF label), with a context that names the user and `row_uid` (`scaleContext`, `dexaContext`). `row_uid` is new on every write. Without an encryption key the document stays readable and is encrypted at the next start with a key.
- Because notes are inside the ciphertext, an upsert cannot leave a column out of the update: `keepNotes` reads the existing rows in the transaction. Rekey (`Rekey`), `encryptExisting` and `firstEncrypted` all cover body rows; keep them in step when you add an encrypted field.

## Settings and counters

`PutSetting` and `DeleteSetting` bump the settings generation in the same transaction; processes compare `SettingsGeneration` to know when to reload. Shared state tables (`used_nonces`, `auth_failures`) are purged by the backend's maintenance loop (they stay empty when Valkey holds that state, and so does `sessions`: `pkg/store/valkeystate` has the same methods with the same meaning, sliding-window counters as sorted sets, nonces as `SET NX PX` and sessions as a key with the session's life plus a per-user index, and must keep them in step; the server needs `maxmemory-policy noeviction`); so are finished jobs (`worker_jobs`, after the retention an administrator set) and workers not seen for a day (`worker_instances`).

## The job queue

`worker_jobs`, `worker_instances` and `worker_schedules` hold the queue the workers draw from (`jobs.go`). Every state change is one conditional `UPDATE ... WHERE id = ? AND state = ? [AND lease_token_hash = ?]` and `RowsAffected` says who won; do not read, decide and write in separate steps. Leasing orders by administrator first, then `users.job_priority` (higher first), then interactive, then oldest. `payload` and `result` are encrypted with a context that names the job (`jobPayloadContext`), the payload is cleared when a job ends, and both are in `rekey` and `encryptExisting`. A unique partial index (`dedupe_key` for queued and running jobs) is what keeps one active job per key. Test races with several goroutines and exactly one winner, and run the PostgreSQL tests too.

## Tests

`newStore` in `sqlstore_test.go` uses in-memory SQLite, and PostgreSQL (own schema per test) when `DEXAFIX_TEST_POSTGRES_DSN` is set. **Run both when you touch SQL**: `docker run -d --rm --name dexa-pg -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:18`. Test races for claims and counters (several goroutines, exactly one winner).
