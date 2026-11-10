# Configuration

Each `serve` flag has an environment variable. A flag on the command line
overrides the variable. The same holds for `serve web` and `serve api`, the two
processes of a [split deployment](15-split-deployment.md). `serve body` is the
[body service](17-body-service.md), a third kind of process.

| Flag                       | Environment variable             | Default                                                               | Description                                            |
| -------------------------- | -------------------------------- | --------------------------------------------------------------------- | ------------------------------------------------------ |
| `--listen`                 | `DEXAFIX_LISTEN`                 | `:8080`                                                               | Address to listen on for plain HTTP. For `serve api`, the public API instead. |
| `--db-driver`              | `DEXAFIX_DB_DRIVER`              | `sqlite`                                                              | `sqlite` or `postgres`.                                |
| `--dsn`                    | `DEXAFIX_DSN`                    | `file:dexafix.db`                                                    | Database connection string. A string with a password is refused on the command line: use the variable or `--dsn-file`. |
| `--dsn-file`               | `DEXAFIX_DSN_FILE`               | empty                                                                 | File holding the connection string. |
| `--auto-migrate`            | `DEXAFIX_AUTO_MIGRATE`           | `true`                                                                | Apply pending migrations on startup; when `false`, `serve` fails if any are pending. |
| `--force-password-login`   | `DEXAFIX_FORCE_PASSWORD_LOGIN`   | `false`                                                               | Keep password sign-in on, to recover from a lockout.   |
| `--encryption-key-file`    | `DEXAFIX_ENCRYPTION_KEY_FILE`    | `dexafix.key`                                                        | Key file for encrypted values. Created if missing, but only for a database without encrypted values. |
| (none)                     | `DEXAFIX_ENCRYPTION_KEY`         | unset                                                                 | Key itself, overrides the key file. No flag.           |
| `--encryption-key-previous-file` | `DEXAFIX_ENCRYPTION_KEY_PREVIOUS_FILE` | empty                                                     | File with earlier keys, one per line, for a key rotation. |
| (none)                     | `DEXAFIX_ENCRYPTION_KEY_PREVIOUS` | unset                                                                | Earlier keys, comma separated, for a key rotation. No flag. |
| `--backend-url`            | `DEXAFIX_BACKEND_URL`            | empty                                                                 | `serve web` only. Required: the internal address of the api process, such as `http://api:8081`. |
| `--service-secret-file`    | `DEXAFIX_SERVICE_SECRET_FILE`    | empty                                                                 | `serve web`, and `serve api` when the internal listener is on. Required there. The shared secret, see [Split deployment](15-split-deployment.md#service-secret). |
| `--worker-secret-file`     | `DEXAFIX_WORKER_SECRET_FILE`     | empty                                                                 | `serve api` when the internal listener is on, and `serve worker`. Secret shared by the api process and its [workers](16-workers.md); must differ from the service secret. |
| `--internal-listen`        | `DEXAFIX_INTERNAL_LISTEN`        | `:8081`                                                               | `serve api` only. Internal API for the web process; keep it on a private network. Empty turns it off. |
| `--maintenance`            | `DEXAFIX_MAINTENANCE`            | `true`                                                                | `serve api` only. Runs the audit log purge and certificate renewal. Turn it off on all but one api process. |
| `--api-upstream`           | `DEXAFIX_API_UPSTREAM`           | empty                                                                 | `serve web` only. Passes `/api/` on to the api process's public listener, for setups without a proxy that routes by path. Off by default. |
| `--internal-tls-cert-file` | `DEXAFIX_INTERNAL_TLS_CERT_FILE` | empty                                                                 | `serve api` only. Server certificate of the internal listener. Set it with `--internal-tls-key-file` and `--internal-tls-client-ca-file`, or none. See [Mutual TLS](15-split-deployment.md#mutual-tls-between-web-and-api). |
| `--internal-tls-key-file`  | `DEXAFIX_INTERNAL_TLS_KEY_FILE`  | empty                                                                 | `serve api` only. Private key of the certificate above. |
| `--internal-tls-client-ca-file` | `DEXAFIX_INTERNAL_TLS_CLIENT_CA_FILE` | empty                                                        | `serve api` only. CA that issues the certificates of the web process. The internal listener then requires a client certificate from it. |
| `--backend-ca-file`        | `DEXAFIX_BACKEND_CA_FILE`        | empty                                                                 | `serve web` only. CA that must have issued the certificate of the api process. Use with an `https://` `--backend-url`. See [Mutual TLS](15-split-deployment.md#mutual-tls-between-web-and-api). |
| `--backend-client-cert-file` | `DEXAFIX_BACKEND_CLIENT_CERT_FILE` | empty                                                           | `serve web` only. Client certificate that web presents to the api process. |
| `--backend-client-key-file` | `DEXAFIX_BACKEND_CLIENT_KEY_FILE` | empty                                                            | `serve web` only. Private key of the client certificate above. |
| `--backend-server-name`    | `DEXAFIX_BACKEND_SERVER_NAME`    | empty                                                                 | `serve web` only. Name the api process's certificate must carry, if it is not the host of `--backend-url`. |
| `--body-url`               | `DEXAFIX_BODY_URL`               | empty                                                                 | `serve api` only. Address of a [body service](17-body-service.md) in its own process, such as `http://body:8082`. Empty runs the body service inside the api process. |
| `--body-secret-file`       | `DEXAFIX_BODY_SECRET_FILE`       | empty                                                                 | `serve api` with `--body-url`, and `serve body`. Secret they share, one per line (the api process sends the first, the body service accepts every line). Must differ from the service and worker secrets. |
| `--body-token-key-file`    | `DEXAFIX_BODY_TOKEN_KEY_FILE`    | empty                                                                 | `serve api` with `--body-url`. Private key that signs the tokens for the body service (`secrets body-token-key`). |
| `--body-ca-file`           | `DEXAFIX_BODY_CA_FILE`           | empty                                                                 | `serve api` only. CA that must have issued the body service's certificate. Use with an `https://` `--body-url`, together with the two files below. |
| `--body-client-cert-file`  | `DEXAFIX_BODY_CLIENT_CERT_FILE`  | empty                                                                 | `serve api` only. Client certificate that the api process presents to the body service. |
| `--body-client-key-file`   | `DEXAFIX_BODY_CLIENT_KEY_FILE`   | empty                                                                 | `serve api` only. Private key of the client certificate above. |
| `--body-server-name`       | `DEXAFIX_BODY_SERVER_NAME`       | empty                                                                 | `serve api` only. Name the body service's certificate must carry, if it is not the host of `--body-url`. |
| `--listen` (`serve body`)  | `DEXAFIX_BODY_LISTEN`            | `:8082`                                                               | `serve body` only. Address of the body service; keep it on a private network. |
| `--health-listen`          | `DEXAFIX_HEALTH_LISTEN`          | empty (off)                                                           | `serve api`, `serve body` and `serve worker`. A listener that answers `/healthz` and `/readyz` only, for a container health check, such as `127.0.0.1:8083`. See [Status and health checks](15-split-deployment.md#status-and-health-checks). |
| `--valkey-url`             | `DEXAFIX_VALKEY_URL`             | empty (off)                                                           | `serve api` only. Keep the failed sign-in counters, single-use values and sessions in Valkey (`redis://host:6379/0`, `rediss://` for TLS) instead of the database. A password in the URL is refused on the command line: use the file. See [Valkey](15-split-deployment.md#valkey). |
| `--valkey-url-file`        | `DEXAFIX_VALKEY_URL_FILE`        | empty                                                                 | `serve api` only. File with the Valkey URL, which may hold the password: `redis://:password@host:6379/0`. |
| `--body-token-public-key-file` | `DEXAFIX_BODY_TOKEN_PUBLIC_KEY_FILE` | empty                                                           | `serve body` only. Required. Public key(s) of the api process's token key, one per line (`secrets body-token-public`). |
| `--body-tls-cert-file`     | `DEXAFIX_BODY_TLS_CERT_FILE`     | empty                                                                 | `serve body` only. Serve over mutual TLS: certificate of the service. Set it with `--body-tls-key-file` and `--body-tls-client-ca-file`, or none. |
| `--body-tls-key-file`      | `DEXAFIX_BODY_TLS_KEY_FILE`      | empty                                                                 | `serve body` only. Private key of the certificate above. |
| `--body-tls-client-ca-file` | `DEXAFIX_BODY_TLS_CLIENT_CA_FILE` | empty                                                                | `serve body` only. CA that issues the api process's client certificate. |
| `--without-body`           | `DEXAFIX_WITHOUT_BODY`           | `false`                                                               | `serve api` (needs `--body-url`) and the `db`, `user` and `settings` commands. This database does not hold the scale readings and DEXA scans because a body service of its own does; keeps the commands from creating their tables here. |

`serve body` takes the database and key flags too, for the database and key of the
body service. It has no `--maintenance`, and its `--auto-migrate` migrates the
body database only.

`serve web` has no database and no encryption key, so it has no database or key
flags. If `DEXAFIX_DSN_FILE` or `DEXAFIX_ENCRYPTION_KEY` is set for it, it
logs a warning and ignores the variable. The database and key flags belong to
`serve api`.

`DEXAFIX_FORCE_PASSWORD_LOGIN` counts as set only when it is exactly `true`.
`1` or `TRUE` leaves it off.

## Settings on the Admin page

Some settings are not flags. They are stored in the database, encrypted like
the other settings, and take effect at once, without a restart. Administrators
change them on the [Admin page](06-using-the-app.md):

- **Network and retention**
  - Base URL, such as `https://dexa.example.com`. Empty by default.
  - Trusted proxies. Empty by default, which trusts nobody. See [Running behind a reverse proxy](03-reverse-proxy.md).
  - Accept the PROXY protocol from the trusted proxies. Off by default. See [PROXY protocol](03-reverse-proxy.md#proxy-protocol).
  - Keep the audit log for N days. 365 by default; `0` keeps it forever.
  - Remind me to rotate the encryption key after N days. Empty or `0` never reminds. Rotation stays manual.
- **E-mail**: SMTP server, port (587 by default), encryption (`starttls` by default, `tls`, or `none` for a server on localhost only), user name, password (stored encrypted, never shown again) and sender address. See [Password reset](07-accounts-and-sign-in.md#password-reset).
- **HTTPS, HTTP/2 and HTTP/3**: certificate, HTTPS port (8443 by default) and HTTP/3. Plain HTTP stays on `--listen`. See [HTTPS, HTTP/2 and HTTP/3](03-reverse-proxy.md#https-http2-and-http3).

There are no flags or environment variables for these settings, and none for
SMTP. If `DEXAFIX_TRUSTED_PROXIES`, `DEXAFIX_BASE_URL` or
`DEXAFIX_AUDIT_RETENTION_DAYS` is still set, the server logs a warning that it
is ignored. If a wrong base URL or PROXY protocol setting locks you out, see
[Settings recovery](05-commands.md#settings-recovery).

The remaining flags are the ones the server needs before it can open the
database: the listen address, the database, the encryption key and
`--auto-migrate`.

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
(`--dsn-file`, `--from-dsn-file`, `--to-dsn-file`, `--admin-dsn-file`, or the matching
`DEXAFIX_*_FILE` variables), which works well with Docker and Kubernetes
secrets. A plain `--dsn file:path.db` without a password is fine. Passing a
password in a flag makes the command fail with a message that says so. The
encryption key has no flag either (see [Encryption](08-encryption.md)).

`dexafix info` prints information about the binary.
