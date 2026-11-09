# Configuration

Each `serve` flag has an environment variable. A flag on the command line
overrides the variable.

| Flag                       | Environment variable             | Default                                                               | Description                                            |
| -------------------------- | -------------------------------- | --------------------------------------------------------------------- | ------------------------------------------------------ |
| `--listen`                 | `DEXAFIX_LISTEN`                 | `:8080`                                                               | Address to listen on.                                  |
| `--db-driver`              | `DEXAFIX_DB_DRIVER`              | `sqlite`                                                              | `sqlite` or `postgres`.                                |
| `--dsn`                    | `DEXAFIX_DSN`                    | `file:dexa-fix.db`                                                    | Database connection string. A string with a password is refused on the command line: use the variable or `--dsn-file`. |
| `--dsn-file`               | `DEXAFIX_DSN_FILE`               | empty                                                                 | File holding the connection string. |
| `--auto-migrate`            | `DEXAFIX_AUTO_MIGRATE`           | `true`                                                                | Apply pending migrations on startup; when `false`, `serve` fails if any are pending. |
| `--trusted-proxies`        | `DEXAFIX_TRUSTED_PROXIES`        | empty                                                                 | Reverse proxies whose `X-Forwarded-*` headers are believed. See [Running behind a reverse proxy](03-reverse-proxy.md). |
| `--base-url`               | `DEXAFIX_BASE_URL`               | empty                                                                 | Public address, such as `https://dexa.example.com`. |
| `--audit-retention-days`   | `DEXAFIX_AUDIT_RETENTION_DAYS`   | `365`                                                                 | Delete audit log events, and the IP addresses in them, after this many days. `0` keeps them forever. |
| `--key-rotation-reminder-days` | `DEXAFIX_KEY_ROTATION_REMINDER_DAYS` | `0` | Show a reminder on the Admin page when the encryption key is older than this many days. `0` never reminds. Rotation stays manual. |
| `--force-password-login`   | `DEXAFIX_FORCE_PASSWORD_LOGIN`   | `false`                                                               | Keep password sign-in on, to recover from a lockout.   |
| `--encryption-key-file`    | `DEXAFIX_ENCRYPTION_KEY_FILE`    | `dexa-fix.key`                                                        | Key file for encrypted values. Created if missing, but only for a database without encrypted values. |
| (none)                     | `DEXAFIX_ENCRYPTION_KEY`         | unset                                                                 | Key itself, overrides the key file. No flag.           |
| `--encryption-key-previous-file` | `DEXAFIX_ENCRYPTION_KEY_PREVIOUS_FILE` | empty                                                     | File with earlier keys, one per line, for a key rotation. |
| (none)                     | `DEXAFIX_ENCRYPTION_KEY_PREVIOUS` | unset                                                                | Earlier keys, comma separated, for a key rotation. No flag. |

Two variables are read strictly:

- `DEXAFIX_FORCE_PASSWORD_LOGIN` counts as set only when it is exactly `true`.
  `1` or `TRUE` leaves it off.
- An invalid `DEXAFIX_AUDIT_RETENTION_DAYS` (not a whole number, or negative) is
  ignored without a warning, and `365` is used.

Other environment variables:

- `LOGLEVEL` sets the log level: `trace`, `debug`, `info`, `warn`, `error`,
  `fatal` or `panic`. The default is `info`.
- `DEBUG` turns off coloured console output when set to any value. It does not
  change the log level.

Postgres example:

```bash
export DEXAFIX_DSN=postgres://user:pass@localhost/dexafix
go run . serve --db-driver postgres
```

**Secrets are never accepted as command-line flags**, because other users of the
machine can read them with `ps` and shells keep them in their history. A
connection string with a password comes from the environment or from a file
(`--dsn-file`, `--from-dsn-file`, `--to-dsn-file`, or the matching
`DEXAFIX_*_FILE` variables), which works well with Docker and Kubernetes
secrets. A plain `--dsn file:path.db` without a password is fine. Passing a
password in a flag makes the command fail with a message that says so. The
encryption key has no flag either (see [Encryption](08-encryption.md)).

`dexa-fix info` prints information about the binary.
