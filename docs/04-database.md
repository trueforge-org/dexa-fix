# Database

The store uses [bun](https://bun.uptrace.dev/), so SQLite and Postgres share one
query path. The schema is versioned with [goose](https://github.com/pressly/goose)
migrations, embedded per backend in `pkg/store/sqlstore/migrations/{sqlite,postgres}/`.

`serve` applies pending migrations on startup. It refuses to start if the
database schema is newer than the binary, so upgrade dexa-fix first.

A new SQLite database file is created readable by its owner only (mode 0600,
and SQLite gives its `-wal` and `-shm` files the same mode), because it holds
password hashes and health data. An existing file is left as it is, but a
warning is logged when other users can read it.

SQLite connections always enforce foreign keys and wait up to 5 seconds for
locks, and file databases run in WAL mode, so the DSN needs no pragmas. The
default DSN is `file:dexa-fix.db`.

The schema starts at a single `00001_init` migration for 1.0.0. Databases
created by pre-1.0 builds carry a higher schema version, so 1.0 refuses them
instead of guessing. Start from a fresh database.

The `db` commands (migrate, copy and rekey) are listed on the
[Commands](05-commands.md) page.

## Moving from SQLite to Postgres

Create an empty Postgres database, then copy and start the server:

```bash
export DEXAFIX_TO_DSN=postgres://user:pass@localhost/dexafix
go run . db copy --from-driver sqlite --from-dsn file:dexa-fix.db --to-driver postgres

export DEXAFIX_DSN=$DEXAFIX_TO_DSN
go run . serve --db-driver postgres
```

The target is migrated to the latest schema and must be empty. Ids are kept,
and the Postgres sequences are reset so new rows do not collide. The copy is a
single transaction, so a failure leaves the target untouched. The source is not
changed, but it must already be fully migrated (`db migrate up`).

`db copy` copies encrypted values unchanged, so the target database needs the
same encryption key as the source (or run `db rekey` on the target afterwards,
with the source key as a previous key). Give the server that starts on the target
the same `DEXAFIX_ENCRYPTION_KEY` or key file. See [Encryption](08-encryption.md).
