# Commands

## Database commands

| Command                  | Description                                                   |
| ------------------------ | ------------------------------------------------------------- |
| `db migrate [up]`        | Apply all pending migrations. This is the default action.     |
| `db migrate down --yes`  | Roll back the latest migration. Drops tables and their data.  |
| `db migrate to N`        | Migrate up or down to version N. `0` empties the schema. Going down needs `--yes`. |
| `db migrate status`      | List every migration and whether it is applied.               |
| `db migrate version`     | Print the current schema version.                             |
| `db copy`                | Copy all data to another database. See below.                 |
| `db rekey [--dry-run]`   | Re-encrypt all encrypted values with the current key. See [Encryption](08-encryption.md). |

`db migrate` takes `--db-driver` and `--dsn`, with the same environment
variables as `serve`. `db copy` takes `--from-driver`, `--from-dsn`,
`--to-driver` and `--to-dsn`, with the variables `DEXAFIX_FROM_DSN`,
`DEXAFIX_TO_DRIVER` and `DEXAFIX_TO_DSN` (and `*_FILE` variants for the
connection strings). `--from-dsn` falls back to `DEXAFIX_DSN`.

Rolling back drops the tables and columns of the migrations it undoes, together
with their data. `db migrate down` and `db migrate to N` for an N below the
current version therefore refuse to run without `--yes`; without it they print
which migrations would be undone. Make a backup first.
`db rekey` takes the database and encryption key flags of `serve`.

```bash
go run . db migrate status
DEXAFIX_DSN=postgres://user:pass@localhost/dexafix go run . db migrate up --db-driver postgres
go run . db migrate down --yes
```

## Account commands

| Command                   | Description                                                          |
| ------------------------- | -------------------------------------------------------------------- |
| `user reset-2fa <e-mail>` | Remove two-step verification and the recovery codes of that account. |

`user reset-2fa` is for lockouts, when nobody who can sign in can turn it off
from the Admin page. It works on the database directly, so it takes the same
database and encryption key settings as `serve`: `--db-driver`, `--dsn` (or
`DEXAFIX_DSN`, or `--dsn-file`) and the key flags. The server does not have to
be stopped, but the change is not written to the audit log. If an administrator
requires two-step verification, the user sets it up again at the next sign-in.

```bash
DEXAFIX_DSN=postgres://user:pass@localhost/dexafix go run . user reset-2fa you@example.com --db-driver postgres
```
