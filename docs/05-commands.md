# Commands

## Server commands

| Command                   | Description                                                                 |
| ------------------------- | --------------------------------------------------------------------------- |
| `serve`                   | Run everything in one process: pages, API, database and keys.               |
| `serve web`               | Run the frontend of a [split deployment](15-split-deployment.md): pages, static files and the public HTTP, HTTPS and HTTP/3 listeners. No database. |
| `serve api`               | Run the backend of a split deployment: database, keys, public API, internal API and maintenance. |
| `serve worker`            | Run a [worker](16-workers.md): it talks to Withings and Strength Level for the api process. No database. |
| `secrets service-secret`  | Print a new service secret of 64 hexadecimal characters for `serve web` and `serve api`. |
| `secrets worker-secret`   | Print a new worker secret of 64 hexadecimal characters for `serve api` and `serve worker`. It must differ from the service secret. |
| `secrets mtls`            | Create certificates for [mTLS](15-split-deployment.md#mutual-tls-between-web-and-api).   |

Create the service secret file and keep it private:

```bash
go run . secrets service-secret > service.secret && chmod 600 service.secret
```

A group- or world-readable file only logs a warning. See
[Service secret](15-split-deployment.md#service-secret) for rotating it.

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

## Settings recovery

| Command                   | Description                                                          |
| ------------------------- | -------------------------------------------------------------------- |
| `settings reset-network`  | Clear the base URL, the trusted proxies and the PROXY protocol setting. |

Use it when a saved value leaves the Admin page unreachable: a base URL for an
address you no longer use, or a PROXY protocol setting that stops your balancer
from connecting. It works on the database directly, so it takes the same
database and encryption key settings as `serve`: `--db-driver`, `--dsn` (or
`DEXAFIX_DSN`, or `--dsn-file`) and the key flags. The change is not written to
the audit log. Restart the server afterwards, because a running server reads
these settings at start and when they are saved on the Admin page.

```bash
DEXAFIX_DSN=postgres://user:pass@localhost/dexafix go run . settings reset-network --db-driver postgres
```
