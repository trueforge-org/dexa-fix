# dexa-fix

[![CI](https://github.com/trueforge-org/dexa-fix/actions/workflows/ci.yaml/badge.svg)](https://github.com/trueforge-org/dexa-fix/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/trueforge-org/dexa-fix/graph/badge.svg)](https://codecov.io/gh/trueforge-org/dexa-fix)
[![Go Report Card](https://goreportcard.com/badge/github.com/trueforge-org/dexa-fix)](https://goreportcard.com/report/github.com/trueforge-org/dexa-fix)
[![Latest release](https://img.shields.io/github/v/release/trueforge-org/dexa-fix)](https://github.com/trueforge-org/dexa-fix/releases/latest)
[![License](https://img.shields.io/github/license/trueforge-org/dexa-fix)](LICENSE)

Corrects smart-scale results using DEXA scan results. A Go web app that stores
per-user DEXA scans and scale readings, and shows one estimated value per day.

Each user has an account and sees only their own data. Users sign in with a
password or with an external provider. Read
[Limitations and security](#limitations-and-security) before you expose it.

## How it works

A smart scale is convenient but biased. A DEXA scan is accurate but rare. The
app uses the difference between the two (the offset) to correct the scale
between scans.

- Stored readings are never changed. Everything below is calculated from them
  and kept in a memory cache until the data or the date changes (see
  [Estimate cache](#estimate-cache)).
- First the app builds its own per-day series: the most accurate reading of
  each day, averaged over a centred 7-day window of real readings, with the
  gaps filled in by straight lines.
- The offset is `DEXA - the actual reading` on the day of each DEXA scan. The
  reading is the scale's own value that day, not the smoothed series.
- On a day with a DEXA value, that value is used.
- On every other day, the series is corrected by the nearby offset.

Example: one DEXA scan on 2026-03-01 with body fat 18.0 %, while the scale says
20.0 % every day. The reading on the scan day is 20.0 %, so the offset is
-2.0 percentage points.

| Date       | Scale (%) | Estimate (%) | Origin                   |
| ---------- | --------- | ------------ | ------------------------ |
| 2026-03-01 | 20.0      | 18.0         | `dexa`                   |
| 2026-03-02 | 20.0      | 18.0         | `measured`               |
| 2026-03-05 | none      | 18.0         | `interpolated` or `held` |

Fat mass and fat-free mass are then calculated from the body fat % and the
weight on every day with a weight, whether or not a calibration applies. The
scale's own values are used only when they cannot be calculated.

The full rules are in [Estimation details](#estimation-details).

## Features

- Accounts with password sign-in, plus OpenID Connect, Sign in with Apple and
  OAuth 2.0 providers. See [Accounts and sign-in](#accounts-and-sign-in).
- Per-user DEXA scans and scale readings, entered in the browser or by JSON API.
- Each reading records its measurement method (smart scale, caliper, BodPod,
  hydrostatic, tape, visual AI) and whether athlete mode was on.
- One estimate per day for weight, body fat, fat mass, fat-free mass, lean
  mass, hydration, bone mass, visceral fat index, BMI, fat mass index (FMI),
  lean mass index (LMI) and the android/gynoid (A/G) fat ratio.
- Optional Withings import, so scale readings do not need typing in.
- SQLite (default) or Postgres. Schema changes are versioned migrations, applied
  on startup. See [Database](#database).

## Quick start

Requires the Go version in `go.mod`.

```bash
go run . serve
```

Open <http://localhost:8080>, create an account and enter some data. The first
account becomes the administrator. By default the SQLite database is
`dexa-fix.db` in the current directory.

The first run also creates the encryption key file `dexa-fix.key` in the current
directory, unless `DEXAFIX_ENCRYPTION_KEY` is set. Back up the key file too. The
server does not start without it, because the encrypted values cannot be read.
See [Encryption](#encryption) for where to keep it.

To build a binary:

```bash
go build -o dexa-fix .
./dexa-fix serve
```

`.goreleaser.yaml` builds release binaries for linux, darwin and windows on
amd64 and arm64.

## Configuration

Each `serve` flag has an environment variable. A flag on the command line
overrides the variable.

| Flag                       | Environment variable             | Default                                                               | Description                                            |
| -------------------------- | -------------------------------- | --------------------------------------------------------------------- | ------------------------------------------------------ |
| `--listen`                 | `DEXAFIX_LISTEN`                 | `:8080`                                                               | Address to listen on.                                  |
| `--db-driver`              | `DEXAFIX_DB_DRIVER`              | `sqlite`                                                              | `sqlite` or `postgres`.                                |
| `--dsn`                    | `DEXAFIX_DSN`                    | `file:dexa-fix.db`                                                    | Database connection string. A string with a password is refused on the command line: use the variable or `--dsn-file`. |
| `--dsn-file`               | `DEXAFIX_DSN_FILE`               | empty                                                                 | File holding the connection string. |
| `--auto-migrate`            | `DEXAFIX_AUTO_MIGRATE`           | `true`                                                                | Apply pending migrations on startup; when `false`, `serve` fails if any are pending. |
| `--trusted-proxies`        | `DEXAFIX_TRUSTED_PROXIES`        | empty                                                                 | Reverse proxies whose `X-Forwarded-*` headers are believed. See [Running behind a reverse proxy](#running-behind-a-reverse-proxy). |
| `--base-url`               | `DEXAFIX_BASE_URL`               | empty                                                                 | Public address, such as `https://dexa.example.com`. |
| `--audit-retention-days`   | `DEXAFIX_AUDIT_RETENTION_DAYS`   | `365`                                                                 | Delete audit log events, and the IP addresses in them, after this many days. `0` keeps them forever. |
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
encryption key has no flag either (see [Encryption](#encryption)).

`dexa-fix info` prints information about the binary.

## Running behind a reverse proxy

dexa-fix speaks plain HTTP. For HTTPS, put a reverse proxy such as nginx, Caddy
or Traefik in front of it, and bind dexa-fix to a private address so that
nobody can bypass the proxy: `--listen 127.0.0.1:8080`.

A proxy hides two things from the app: who the visitor is (every connection
comes from the proxy) and whether they used HTTPS. The proxy passes both on in
the `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` headers. Any
visitor can send those headers too, so dexa-fix only believes them on
connections from the proxies you list in `--trusted-proxies`. From everyone
else they are ignored, and a warning is logged once.

| Setting             | What it does                                                                                                                              |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `--trusted-proxies` | Comma-separated addresses or CIDR ranges of your proxies, such as `127.0.0.1,10.0.0.0/8`. The keyword `loopback` stands for `127.0.0.0/8` and `::1`; `private` adds the RFC 1918 ranges and `fc00::/7`. Empty trusts nobody. |
| `--base-url`        | The public address, scheme and host only (no path). Optional when the proxy forwards `Host` and `X-Forwarded-Proto` correctly; set it for a fixed, predictable address. |

What changes when the proxy is trusted:

- **Sign-in limits and the audit log** use the visitor's address. Without this,
  everyone shares the proxy's address: one person's failed sign-ins would block
  everybody, and the audit log would show only the proxy.
- **Cookies are marked `Secure`** and `Strict-Transport-Security` is sent, as
  soon as the visitor used HTTPS (as reported by the proxy), or the base URL is
  `https`.
- **Sign-in provider callback URLs** (the ones to register at your identity
  provider, shown on the Admin page) are built from the base URL, or else from
  `X-Forwarded-Proto` and the host.
- **Form posts pass the same-origin check.** The browser's `Origin` has to match
  the host it visited, which is why the proxy must pass the original host, either
  as `Host` or as `X-Forwarded-Host`, or you set `--base-url`.

For the address, the app reads `X-Forwarded-For` from the right: it skips every
address that belongs to a trusted proxy, and the first address after that is
the visitor. Anything the visitor wrote further left is never used. Make sure
the proxy appends to this header instead of passing along what the client sent.
Without `X-Forwarded-For`, `X-Real-IP` is used.

Examples, assuming dexa-fix listens on `127.0.0.1:8080` on the same machine:

```text
# nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}

# Caddy: forwards the headers by default
dexa.example.com {
    reverse_proxy 127.0.0.1:8080
}

# Traefik (docker labels)
traefik.http.routers.dexa.rule=Host(`dexa.example.com`)
traefik.http.routers.dexa.tls=true
traefik.http.services.dexa.loadbalancer.server.port=8080
```

```sh
dexa-fix serve --listen 127.0.0.1:8080 --trusted-proxies loopback --base-url https://dexa.example.com
```

In Docker, the proxy reaches the app from the network's bridge address, so use
the network's range, or `private`. Only trust ranges that nothing but your
proxies can send from.

dexa-fix must be served from the root of its host. A sub-path such as
`example.com/dexa/` is not supported. If you use Withings, the redirect URL on the Admin page
has to be the public callback address, for example
`https://dexa.example.com/integrations/withings/callback`.

## Database

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

| Command                  | Description                                                   |
| ------------------------ | ------------------------------------------------------------- |
| `db migrate [up]`        | Apply all pending migrations. This is the default action.     |
| `db migrate down --yes`  | Roll back the latest migration. Drops tables and their data.  |
| `db migrate to N`        | Migrate up or down to version N. `0` empties the schema. Going down needs `--yes`. |
| `db migrate status`      | List every migration and whether it is applied.               |
| `db migrate version`     | Print the current schema version.                             |
| `db copy`                | Copy all data to another database. See below.                 |
| `db rekey [--dry-run]`   | Re-encrypt all encrypted values with the current key. See [Encryption](#encryption). |

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

### Moving from SQLite to Postgres

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
the same `DEXAFIX_ENCRYPTION_KEY` or key file. See [Encryption](#encryption).

## Using the app

Settings and Admin are in a menu under your name in the header, which also has
**Sign out**.

- `/`: the sign-in page. Sign in with an e-mail address and password, create an
  account, or use a provider button. Names are display names. They need not be
  unique and have 1 to 64 characters.
- `/u/{id}`: redirects to the estimates page.
- `/u/{id}/estimates`: one row per day with the estimated values, an overview
  with the latest values and a trend chart, the calibrations in use, and the
  Withings buttons.
- `/u/{id}/dexa`: add and delete DEXA scans. A scan needs a date and at least
  one measurement.
- `/u/{id}/scale`: add and delete readings. Times are in your time zone (Settings). Choose the
  measurement method in the drop-down and tick **Athlete mode** if it applies.
- `/u/{id}/settings`: the athlete-mode default and the accuracy rank of each
  method. The ranking is a drag-to-reorder list, with up and down buttons for
  keyboard use. Without JavaScript it has rank number inputs instead. A Withings
  section connects, imports from and disconnects the user's Withings account. It
  says so when Withings credentials are missing, either on the Admin page or in
  the environment. Also:
  - **Profile**: name, e-mail, time zone and height in cm. The e-mail is
    required, because it is how you sign in; changing it asks for your current
    password. The time zone (an IANA name such as `Europe/Amsterdam`, default
    `UTC`) decides which calendar day a scale reading belongs to, how the date
    and time of a reading are read and shown, what "today" is and which day a
    Withings import covers. DEXA scans are plain dates and are not moved.
  - **Access tokens**: create and revoke personal access tokens for the API.
  - **Linked accounts**: link or unlink sign-in providers.
  - **Password**: set or change the account's password. Changing it signs out
    the other sessions and revokes all access tokens.
- `/admin`: administrators only. Administrators can add users and open or
  close new accounts. The page shows the server status and has these
  sections:
  - **Users**: grant or remove administrator, set a password, or delete a user
    with all their data. The last administrator cannot be removed.
  - **Password sign-in**: turn it on or off.
  - **Sign-in providers**: add, edit and remove providers.
  - **Withings application**: the client ID, client secret and redirect URL.
    Values saved here override the environment variables.

Numbers may use a decimal comma. Notes are limited to 500 characters.

While a scale reading or DEXA scan is being entered, an empty field shows a gray
value when it can be calculated from what was typed. Weight, body fat %, fat
mass and fat-free mass calculate from each other: any two give the rest. On the
DEXA form, lean mass + bone mass gives fat-free mass. A typed value is never
replaced. Gray values are saved with the form unless a value is typed instead.
This needs JavaScript; without it the fields stay empty.

On the estimates page, a `DEXA` badge marks a value from a DEXA scan, and a
`corr.` badge marks a corrected value. Faded values are interpolated or held,
and a gray `est` tag in front of the date marks a row with a value
interpolated between two readings of that field. Hovering a value shows its
origin, the method and mode it is based on, and the raw reading. The
calibrations section lists each scan's offsets (DEXA minus the reading on the
scan day) per method and mode. Days with only a DEXA scan appear too. The
daily table shows the same period as the chart (30 days, 90 days, 1 year or
all time, 90 days by default). The Columns menu above the daily table shows
and hides columns, and the choice is remembered per account.

## Accounts and sign-in

Each user has an account and sees only their own data. The first account
created becomes the administrator. Passwords are hashed with bcrypt. They must
be 8 characters or more, and at most 72 bytes.

Creating an account asks for a name, an e-mail address and a password. The
e-mail address is the account's unique key, and it is what you sign in with. It
is compared case-insensitively and stored in lowercase. Names are display names
and need not be unique.

**New accounts are closed by default.** Only the very first account on an empty
server can be created without an invitation. After that nobody can create
their own account, with a password or on first sign-in through a provider,
until an administrator opens **New accounts** on the Admin page. Until then an
administrator adds people from the Users list. Existing accounts keep working.
This keeps a server that is reachable from the internet from handing out
accounts by accident.

### Your data

Under **Settings → Your data** every user can download two JSON files, which
serve a person's right of access and data portability (GDPR articles 15 and 20):

- **Settings and personal details**: name, e-mail address, role, height,
  settings, permissions, sign-in methods (provider and account id), access
  token names and prefixes, whether Withings and Strength Level are connected
  (and the Strength Level e-mail address), and the audit events about the
  account.
- **DEXA scans and scale readings**: everything the user measured, including
  notes.

Credentials are never included: the files say whether a password, token or
integration exists, not what it is. The same files are available to scripts at
`/api/v1/users/{id}/export/profile` and `/api/v1/users/{id}/export/data`.
Every export is itself recorded in the audit log. Deleting a user (Admin page)
removes all their measurements and settings.

### Audit log

Security-relevant events are recorded in the database: sign-ins (successful,
failed and blocked), sign-outs, account creation, profile, password, token and
sign-in method changes, imports, integration connects, data deletions, exports,
and every administrator action (users, roles, permissions, providers, server
settings). Each event has a time, who did it, whom it concerned, a short detail
and the client's IP address. Details never hold passwords, tokens, e-mail
addresses or login names.

Administrators see the latest 100 events on the Admin page and can read the
whole log at `GET /api/v1/audit` (`limit`, `before_id` and `user_id` query
parameters). Everyone sees the latest events about their own account in
Settings, and at `GET /api/v1/users/{id}/audit`.

Events are deleted after `--audit-retention-days` (365 by default). When a user
is deleted, the IP addresses in the events about them are cleared and the
events no longer refer to them. Recording an event never fails the request
that caused it; a failed write is logged.

### Sign-in providers

Administrators add providers on the Admin page. There are three types:

- **OpenID Connect**, for example Google, with the issuer
  `https://accounts.google.com`.
- **Sign in with Apple**. Enter the Services ID as the client ID, then the team
  ID, the key ID, and the contents of the `.p8` key file as the secret. dexa-fix
  generates Apple's short-lived client secret from these. Apple requires HTTPS.
- **OAuth 2.0**, with the authorisation, token and userinfo URLs.

Register this callback URL with the provider:

```text
https://your-host/auth/<short name>/callback
```

The Admin page shows the callback URL for each provider. Replace `<short name>`
with the provider's short name.

Behind a reverse proxy, forward the `Host` header and set `X-Forwarded-Proto`.
The callback URL is built from them.

Users link a provider in Settings. With **Create accounts on first sign-in**
turned on, a login from an unknown account creates a new account. Accounts are
matched only by the provider's subject, never by e-mail. A new account takes
the provider's e-mail address on first sign-in, unless the provider marks it
unverified. If that address is already in use, sign-in is refused. The person
must then sign in to the existing account and link the provider in Settings.
Removing a provider also removes its links.

### Password sign-in

Password sign-in can be turned off on the Admin page. Providers and access
tokens keep working. The administrator who turns it off must have a linked
provider first.

To recover from a lockout, start the server with password sign-in forced on:

```bash
go run . serve --force-password-login
```

The same is set with `DEXAFIX_FORCE_PASSWORD_LOGIN=true`.

### Encryption

These values are encrypted with AES-256-GCM before they are stored:

- each user's Withings tokens and Strength Level login
- sign-in provider secrets, including the Apple key
- Withings application credentials and other server settings
- e-mail addresses

Each value is tied to its row, so copying it to another row makes it unreadable.
Each e-mail address also gets a keyed hash, derived from the encryption key. The
hash makes addresses searchable and unique without decrypting them. Passwords
and access tokens were already stored only as hashes. Names are not encrypted,
but addresses are. Measurements and height are not encrypted by the application
either. For those, and for names, use disk or database encryption.

The key comes from one of these, in this order:

1. The environment variable `DEXAFIX_ENCRYPTION_KEY`: 32 bytes as 64
   hexadecimal characters or as base64. There is no command-line flag for it.
2. The key file set with `--encryption-key-file` or
   `DEXAFIX_ENCRYPTION_KEY_FILE`. The default is `dexa-fix.key` in the working
   directory.

If the variable is not set and the key file does not exist, dexa-fix creates
the file with a new random key and mode 0600, but only for a new database or one
that holds no encrypted values yet. If the database already holds encrypted
values, a missing key file is an error and no file is created, so a key file
that did not mount (or a wrong working directory) is reported instead of being
replaced by a key that cannot read anything.

In containers, put the key file on a persistent volume, or set
`DEXAFIX_ENCRYPTION_KEY`. Values stored before encryption are encrypted
automatically at startup.

The database remembers which key it belongs to (a check value in the
`crypto.check` setting, which reveals nothing about the key). Starting with a
different key fails with a message that says so, instead of failing later at a
random value.

For SQLite, `serve` logs a warning when the key file is in the same directory as
the database file, which is the case for the container image defaults
(`/data/dexa-fix.db` and `/data/dexa-fix.key`). A leak of that directory (a
backup, a volume snapshot) then exposes both the encrypted values and the key.
Prefer `DEXAFIX_ENCRYPTION_KEY` from a secret store, or a key file on a separate
secret mount.

Back up the key separately from the database. Without it, the encrypted values
cannot be read, and the server does not start, because a server setting can no
longer be read. Keeping the key next to database backups defeats the purpose.

#### Rotating the key

Stored values look like `enc:v2:<key id>:<data>`. The key id is the first eight
hexadecimal characters of the SHA-256 of the key, so values written by different
keys can be told apart. Values from before (`enc:v1:`) stay readable. To change
the key:

1. Stop the server and back up the database and the key file.
2. Generate a new key, for example `openssl rand -hex 32`.
3. Configure the new key as the current one and the old key as a previous one:
   `DEXAFIX_ENCRYPTION_KEY=<new>` and `DEXAFIX_ENCRYPTION_KEY_PREVIOUS=<old>`, or
   a new `--encryption-key-file` plus `--encryption-key-previous-file` with the
   old key on a line of its own. Several previous keys are allowed.
4. Run `dexa-fix db rekey --dry-run` to see how many values would change. It
   fails if any value cannot be opened by one of the keys.
5. Run `dexa-fix db rekey`. In a single transaction it re-encrypts the e-mail
   addresses, integrations, server settings (including the cookie key) and
   sign-in provider secrets with the current key, recomputes the e-mail lookup
   hashes (they derive from the key) and stores the new check value. It either
   completes or changes nothing, and can be repeated: values already under the
   current key are skipped.
6. Start the server with only the new key. Remove the old key from the
   configuration once `db rekey` reports no values left to re-encrypt.

The server also starts between steps 3 and 5. It reads old values with the
previous key, writes new ones with the current key, and logs a warning until
`db rekey` has run. Run `db rekey` while the server is stopped: a running server
keeps its keys in memory. The cookie key itself does not change, so sessions
stay valid. `db rekey` was tested on SQLite; on Postgres it is the same code in
one transaction.

### Storage and rate limiting

Some values are encrypted in the database. See [Encryption](#encryption) for
which ones and how to manage the key.

Failed password sign-ins are counted in memory: 10 per login name and 60 per
client address in 15 minutes, after which sign-in answers 429. The address is the
visitor's: behind a reverse proxy, set `--trusted-proxies` (see
[Running behind a reverse proxy](#running-behind-a-reverse-proxy)), or everyone
shares the proxy's address and its count. Limit sign-in attempts at the proxy as
well on a public deployment. Form posts are capped at 1 MiB.

## Withings import

Each user links their own Withings account. Imported measurements are stored
as scale readings with the source `withings`.

### Setup

1. In the Withings developer dashboard, create an application and register the
   callback URL, for example
   `https://dexa.example.com/integrations/withings/callback`.
2. As an administrator, enter the application's client id and secret in the
   Withings application section of the Admin page. They are stored encrypted in
   the database. The redirect URL defaults to this server's own callback
   address, so set it explicitly only if the address Withings must use differs
   (for example behind a proxy without `--base-url`). Connecting is possible
   only when all three are set; until then the Settings page shows Withings as
   not set up. Withings is configured only on the Admin page: the old
   `DEXAFIX_WITHINGS_*` variables and flags no longer exist and are ignored with
   a warning.
3. On the estimates or Settings page, choose **Connect Withings** and grant
   access. Finish within 10 minutes, the lifetime of the connection state.
4. Choose **Import from Withings**.

**Disconnect Withings** forgets the user's tokens. Imported readings are kept.

Imported: weight, fat ratio, fat mass, fat-free mass, muscle mass, hydration,
bone mass and visceral fat. Measurements Withings marks as possibly belonging
to someone else (for example a guest) are ignored. Days are calendar days in the
user's time zone.

### Bulk import

**Import from Withings** fetches the whole history. Each day is handled by what
is already stored:

| Day has                | What happens                                                                                          |
| ---------------------- | ----------------------------------------------------------------------------------------------------- |
| No readings            | Imported first.                                                                                       |
| Withings readings only | Changed readings are updated, identical ones are left alone, new ones are added. Nothing deleted.     |
| Manual readings        | New Withings readings are skipped and the day is listed in the result. Earlier imports are refreshed. |

A manual entry is a deliberate value. Averaging it with a Withings reading of
the same weigh-in would distort the day, so bulk import leaves such days alone.

### Re-import one day

Use **↻** on a row of the estimates page, or `?day=` on the API, to re-import
one day. It replaces that day's Withings data:

- Adds the day's Withings readings, even when manual readings exist.
- Updates changed readings.
- Removes stored Withings readings that Withings no longer returns, for
  example after a weigh-in was deleted in the Withings app.
- Never touches manual readings.

## Estimation details

Estimates are computed from the stored data. The result is cached per user and
recalculated when the data or the date changes (see
[Estimate cache](#estimate-cache)). Stored readings are never changed. Each
field is handled on its own.

1. **Daily average.** Readings are averaged per day (a calendar day in the user's time zone), for each field and each
   group. A group is a measurement method plus athlete mode. Missing values are
   ignored.
2. **Most accurate reading.** Each day uses the group whose method ranks
   highest for that field. The default order, most accurate first, is
   hydrostatic, BodPod, caliper, smart scale, tape, visual AI, typed into an
   app (`manual-entry`). Change it on the
   settings page. A method without a value for the field is skipped, so a
   caliper reading with only body fat does not hide the scale weight. If the
   normal and athlete group of one method both have a value, the one matching
   the user's athlete-mode default wins.
3. **Rolling average.** Each day is averaged over the real readings within 3
   days before and after it (a 7-day window, shorter at the ends of the series).
   Only real readings count, never days filled in by step 4. If the window has
   at least 3 days with a real reading, the day's value is that average. This
   also applies to a day without a reading of its own, and its origin stays
   `interpolated` or `held`. Otherwise a day with a reading keeps that reading.
   Peaks are not capped.
4. **Missing days.** A day still without a value from step 3 gets a value on a
   straight line between the nearest days that have one (origin `interpolated`).
   After the last such day, its value is repeated up to today (origin `held`).
   The held value can therefore be the last average, not the last raw reading.
   This is the per-day series.
5. **Calibration.** On each DEXA day inside the series, the offset (DEXA minus
   the reading on that day) is recorded for body fat % and bone mass. Every
   group (method and athlete mode) with a reading of that field on the scan day
   gets its own offset. Several readings of one group on one day are averaged
   first. A group without a reading on the scan day gets no offset from that
   scan. A scan with fat mass and weight but no body fat % gets its body fat %
   calculated as fat mass / weight x 100, so it still calibrates body fat %.
   Weight is measured by DEXA but is never corrected. Hydration and visceral
   fat index are never corrected either, because DEXA does not measure them
   here. A scan with no reading of a group on that day cannot calibrate that
   group.
6. **Estimate.** For each field and day:
   - A DEXA value is used as it is (origin `dexa`).
   - Before the first calibration of the day's group the series is corrected
     with that first calibration's offset.
   - Between two calibrations of the group the offset is blended with a
     smoothstep curve, `w(t) = t²(3 − 2t)`, where `t` is 0 at the earlier scan
     and 1 at the later one: `offset = (1 − w) × earlier + w × later`. The first
     half stays near the earlier scan, and the second half moves towards the
     later one.
   - After the last calibration the last offset is kept.
   - A window that mixes methods applies each reading's own offset: the offsets
     of the groups behind the window's real readings are averaged, weighted by
     how many readings each group has in the window. A group without a
     calibration counts as 0.
   - Corrected values have `corrected: true`. Body fat % is kept within 0 to 100
     and bone mass is kept at 0 or more, after the offset is applied.
   - Fat mass and fat-free mass are calculated on every day with a weight,
     whether or not a calibration applies. When body fat % is known, fat mass =
     weight x body fat % / 100. When it is not, the fat mass already on that day
     is kept. Fat-free mass = weight - fat mass, so the two always add up to the
     weight. The scale's own fat mass and fat-free mass are used only when they
     cannot be calculated, for example on a day with no weight. A DEXA fat mass
     or fat-free mass value on its own day is used as it is, and the other is
     calculated from it where needed. They are marked `corrected: true` when
     the value they come from is corrected or from DEXA. They are unmarked only
     for a group with no calibration at all. Lean mass = fat-free mass - bone
     mass, calculated on every day with both. On a day missing either one there
     is no lean mass. A DEXA lean mass value on its own day is used as it is
     (origin `dexa`). Lean mass is marked `corrected: true` when the fat-free
     mass or bone mass it comes from is corrected or from DEXA.
   - BMI = weight (kg) / height (m)², FMI = fat mass (kg) / height (m)² and
     LMI = lean mass (kg) / height (m)², where lean mass is the fat-free mass
     minus the bone mass. Height comes from the profile in cm. Without a height,
     these three are left out. They are marked `corrected: true` when the value
     they come from is corrected or from DEXA. The A/G ratio = android fat % /
     gynoid fat %, from DEXA scans that have both values (origin `dexa` on the
     scan day). After a scan, the latest ratio is repeated (origin `held`)
     until the next scan with a ratio. There is no value before the first such
     scan.

### Estimate cache

Computing the estimates walks a user's whole history, so the result is kept in
memory per user (for the 32 most recently used users). It is reused until
something it depends on changes, and there is no list of places that have to
remember to clear it: every user row carries a `data_version` counter in the
database, and each write that affects estimates increments it in the same
transaction. That covers adding or deleting scans and readings (web, API,
imports), the athlete default and method ranking, and the height. Because the
counter lives in the database, a change made by another process or server
sharing a Postgres database is seen at once as well. The cache is also tied to
the date, since the series runs up to today, and starts empty after a restart or
upgrade.

`GET /api/v1/users/{id}/estimates` sends an `ETag`. A client that sends it back
in `If-None-Match` gets `304 Not Modified` while nothing changed, without the
server computing or sending anything, which suits dashboards that poll.

## JSON API

All routes are under `/api/v1`. Bodies are JSON, and unknown request fields are
rejected. Errors have the form `{"error": "..."}`.

Every request must be authenticated, except `GET /healthz` and static files.
Responses are 401 when the request is not authenticated, and 403 when the
user's role is not enough. See [Authentication](#authentication).

| Method | Path                            | Description                                                                                                                                             |
| ------ | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET    | `/users`                        | List users. Administrators only.                                                                                                                        |
| POST   | `/users`                        | Create a user. Administrators only. Body: `{"name": "...", "email": "...", "password": "..."}`. `email` is required and unique. `password` is optional. |
| GET    | `/users/{id}/dexa-scans`        | List DEXA scans.                                                                                                                                        |
| POST   | `/users/{id}/dexa-scans`        | Add a scan. `measured_at` is required (RFC 3339).                                                                                                       |
| GET    | `/users/{id}/scale-readings`    | List scale readings.                                                                                                                                    |
| POST   | `/users/{id}/scale-readings`    | Add a batch: a JSON array of up to 5000 readings.                                                                                                       |
| GET    | `/users/{id}/settings`          | Athlete-mode default and method ranking.                                                                                                                |
| PUT    | `/users/{id}/settings`          | Replace them. Body: `{"athlete_default": true, "method_ranking": [...]}`.                                                                               |
| GET    | `/users/{id}/export/profile`    | Download settings and personal details (JSON file). See [Your data](#your-data).                                                                        |
| GET    | `/users/{id}/export/data`       | Download all scans and readings (JSON file).                                                                                                            |
| GET    | `/users/{id}/audit`             | Events about your account, newest first.                                                                                                                |
| GET    | `/audit`                        | The whole audit log. Administrators only.                                                                                                               |
| GET    | `/users/{id}/estimates`         | Daily estimates and calibrations.                                                                                                                       |
| POST   | `/users/{id}/import/{provider}` | Import from a provider. Optional `since` and `day` query parameters.                                                                                    |

Scans and readings cannot be deleted through the API, and access tokens cannot
be created or revoked through it. Use the web interface for those. Changing a
password revokes all of the user's access tokens, and so does an administrator
setting it, so create new tokens afterwards if you use the API.

The two list endpoints (`dexa-scans` and `scale-readings`) return the newest
first and are paged. Query parameters: `limit` (default 1000, at most 10000),
`before` (the cursor from the previous page), `since` and `until` (RFC 3339 or
`YYYY-MM-DD`, inclusive; a plain date as `until` covers that whole day). When
there are older items, the response has an `X-Next-Cursor` header; pass its
value as `before` to get the next page. The body stays a plain JSON array. The
scale page in the web interface shows 100 readings at a time. The export
endpoints always return everything.

Batch readings with an `external_id` are upserted on `source` and
`external_id`, so repeated imports do not duplicate. Readings without a
`source` get `manual`.

Readings also take `method` (`scale` by default, or `hydrostatic`, `bodpod`,
`caliper`, `tape`, `visual-ai`, `manual-entry`) and `athlete_mode`. When `athlete_mode` is left
out, the user's default applies. A re-import never changes the method or athlete
mode of a stored reading. In `method_ranking`, methods you leave out follow in
the default order.

```bash
export DEXAFIX_TOKEN=dxf_...   # personal access token from Settings

curl -s -X POST localhost:8080/api/v1/users \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name": "Alice", "email": "alice@example.com"}'

curl -s -X POST localhost:8080/api/v1/users/1/dexa-scans \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"measured_at": "2026-03-01T08:00:00Z", "weight_kg": 80.0}'

curl -s -X POST localhost:8080/api/v1/users/1/scale-readings \
  -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '[{"measured_at": "2026-03-01T07:00:00Z", "weight_kg": 82.0}]'

curl -s -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  localhost:8080/api/v1/users/1/estimates
```

### Authentication

Send one of these with each request:

- `Authorization: Bearer <personal access token>`. Tokens start with `dxf_`.
  Create them in Settings. A token is shown once and stored only as a hash.
  It works for the API only.
- `Authorization: Bearer <ID token>` from an OpenID Connect or Sign in with
  Apple provider, for an account linked to that provider. The token must be a
  JWT signed by the provider and issued for this application's client ID.
  Opaque access tokens are not accepted.
- HTTP Basic with the e-mail address and password, while password sign-in is
  on:

  ```bash
  curl -s -u you@example.com:password localhost:8080/api/v1/users/1/estimates
  ```

- The browser session cookie, set when you sign in on the website.

A user can reach only their own `/users/{id}/...` routes. Other ids answer 404.
`GET /users` and `POST /users` are for administrators only.

### Import endpoint

```bash
curl -s -X POST -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  'localhost:8080/api/v1/users/1/import/withings?since=2026-01-01T00:00:00Z'
curl -s -X POST -H "Authorization: Bearer $DEXAFIX_TOKEN" \
  'localhost:8080/api/v1/users/1/import/withings?day=2026-03-01'
```

- `since` is an RFC 3339 timestamp. It limits a bulk import to measurements at
  or after that time.
- `day` is a date such as `2026-03-01`. It re-imports that day. When `day` is
  set, `since` is ignored.

A successful import returns counts:

```json
{"provider":"withings","imported":3,"updated":0,"unchanged":12,"removed":0,"skipped":0}
```

`skipped_days` (a list of `YYYY-MM-DD`) appears only when `skipped` is above
zero.

| Status | Meaning                                         |
| ------ | ----------------------------------------------- |
| 400    | `since` or `day` is malformed.                  |
| 404    | Unknown provider or user.                       |
| 409    | The user has not connected the provider.        |
| 501    | The server has no credentials for the provider. |

### Other routes

- `GET /healthz` returns `ok`.
- `GET /u/{id}/integrations/{provider}/connect` starts OAuth. The provider then
  calls `GET /integrations/{provider}/callback`. The callback is not per user,
  because Withings accepts one redirect URL. A short-lived cookie set by
  `connect` identifies the user. Both return 404 when the provider is unknown
  or not configured.
- `POST /u/{id}/integrations/{provider}/disconnect` forgets the user's tokens.

## CI and releases

GitHub Actions runs lint (gofmt, `go vet`, `go mod tidy`), the tests with the
race detector and coverage on SQLite and on Postgres, `govulncheck`, and a
cross-compile for linux/amd64, linux/arm64, darwin/arm64 and windows/amd64. It
runs on pushes to `main`, on pull requests, when started by hand, and weekly on
Monday. Coverage is reported to Codecov. Pushing a tag such as `v1.0.0` publishes
a GitHub release (archives, checksums, a cosign signature, SBOMs, build
provenance, a grouped changelog) and a multi-arch container image at
`ghcr.io/trueforge-org/dexa-fix`. See [docs/ci.md](docs/ci.md) for the details,
the secrets to configure and how to cut a release.

```bash
docker run -p 8080:8080 -v dexa-fix-data:/data ghcr.io/trueforge-org/dexa-fix:latest
```

## Development

```bash
go test ./...
```

The tests use in-memory SQLite. To run the store tests against Postgres too,
set `DEXAFIX_TEST_POSTGRES_DSN`. Each test gets its own schema. Postgres 18 is
the supported version, so test against that:

```bash
docker run -d --rm --name dexa-pg -e POSTGRES_PASSWORD=x -p 5432:5432 postgres:18
DEXAFIX_TEST_POSTGRES_DSN='postgres://postgres:x@localhost:5432/postgres?sslmode=disable' \
  go test ./pkg/store/sqlstore/
```

### Layout

- `main.go`: entry point. Sets up logging and prints the version.
- `cmd/`: `serve` runs the web app, `db` migrates and copies the database,
  `info` prints binary information.
- `pkg/estimate/`: estimation logic.
- `pkg/strengthlevel/`: the Strength Level entries built from the estimates
  (`strengthlevel.go`) and the sync client (`client/`).
- `pkg/importer/`: the importer interfaces and the registry.
- `pkg/importer/withings/`: OAuth and API client (`client.go`), the rules for
  what to write (`plan.go`), and the importer (`withings.go`).
- `pkg/models/`: domain types.
- `pkg/store/`: the `Store` interface. `sqlstore/` implements it with bun for
  SQLite and Postgres. Its goose migrations live under `migrations/`.
- `pkg/web/`: HTML handlers and templates, the JSON API (`api.go`), the
  integration routes (`handlers_import.go`), routes in `server.go`, and the
  in-memory estimate cache (`estcache.go`).

The layout and conventions follow trueforge-org/forgetool. `dexa-fix info`
reuses its `pkg/info`.

### Schema changes

Every schema change adds a migration with the same version number to both
`pkg/store/sqlstore/migrations/sqlite/` and `migrations/postgres/`, for example
`00002_add_notes.sql`. Each file has `-- +goose Up` and `-- +goose Down`
sections. `TestMigrationParity` fails if the two directories differ.

### Adding an importer

An importer writes readings from an external service. It implements:

```go
type Importer interface {
    Name() string
    Import(ctx context.Context, st store.Store, userID int64, o Options) (Result, error)
}
```

`Name` is the provider name used in URLs and as the readings' `Source`. Write
through `st.UpsertScaleReadings` with a stable `ExternalID`, so repeated
imports are idempotent. If the user must link an account, also implement
`importer.OAuthProvider`. Its OAuth routes are generic over the provider name.

Register the importer in `cmd/serve.go`:

```go
srv, err := web.New(st, importer.NewRegistry(wi, myImporter))
```

The import API works for any registered provider. The estimates page has
buttons only for Withings (`providerWithings` in `pkg/web/handlers_estimates.go`
and `pkg/web/templates/estimates.html`), so a new provider needs its own
buttons there.

## Limitations and security

- **Rate limiting is basic and in memory.** Failed password sign-ins are limited
  per login name and per address, but the counts reset when the server restarts
  and are not shared between several servers. On a public deployment, limit
  sign-in attempts at the reverse proxy too. See
  [Storage and rate limiting](#storage-and-rate-limiting).
- **Measurements, height and names are not encrypted.** Protect the database
  file or Postgres database and its backups with disk or database encryption.
  Losing the encryption key makes the encrypted values unreadable. See
  [Encryption](#encryption).
- **Behind a proxy**, set `--trusted-proxies`, or everyone shares the proxy's
  address and the forwarding headers are ignored. See
  [Running behind a reverse proxy](#running-behind-a-reverse-proxy).
- **Estimates are cached in memory** per user, for the 32 most recently used
  users, and recomputed when the data, the settings that matter or the date
  change. See [Estimate cache](#estimate-cache).
