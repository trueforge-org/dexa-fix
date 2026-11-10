# Body service

The body service holds the scale readings and the DEXA scans, computes the
[estimates](09-estimates.md) from them and decides what an import from Withings
stores. It knows nothing about people: no sign-in, no sessions, no e-mail. The
api process authenticates people and calls the body service for the data of one
user at a time.

You can ignore this page if you run `dexafix serve`, or `serve api` without
`--body-url`. Then the body service runs inside that process, on the same
database, and nothing needs to be set up. Run it as a process of its own when you
want a leak of one database or one host to give away only half of the data.

## What it separates

| | api process (main backend) | body service |
| --- | --- | --- |
| Holds | users, sessions, settings, audit log, provider tokens, the job queue | scale readings, DEXA scans, the estimate cache |
| Database | the main database | its own PostgreSQL schema and role, or its own SQLite file |
| Encryption key | its own | its own |
| Can reach | both | only its own database |

- The readings and scans are encrypted with a key that the api process does not
  hold (see [Keys](#keys)).
- With PostgreSQL, the body service connects as a role that sees the schema
  `body` and nothing else, and the api role has no rights in that schema. The
  deployment example sets this up.
- Heavy work (estimates over years of readings, imports) runs in the body
  service with its own limits, so it cannot slow down sign-in and the pages.

A compromised api process still reaches all data, because it is the one that asks
the body service for it. The separation protects against the leak of one
database, one backup or one host, not against a break-in to the api process.

## How a call is protected

Every call from the api process to the body service carries two things:

- the **body secret**, a file both processes share, like the
  [service secret](15-split-deployment.md#service-secret);
- a **token** that the api process signs for that one call. It names one user and
  one scope (`read`, `write` or `admin`), lives for 60 seconds at most and carries
  the user's time zone, height, method ranking and athlete default, which the
  estimates need.

The api process signs with a private key. The body service holds only the public
key, so it can check tokens but never make one. A token for user 1 opens nothing
of user 2, a `read` token cannot write, and only an `admin` token can remove a
user's data. The two processes need clocks within a few seconds of each other.

## Start it

The recommended way is the
[recommended deployment](15-split-deployment.md#the-recommended-deployment-deploy):
its `init` container (`dexafix secrets init`) creates everything below, including the
certificates, and its `db-setup` container (`dexafix db provision`) creates the PostgreSQL roles and schema. To run it by hand, make the secrets first. All of them are files:

```bash
dexafix secrets body-secret > body.secret && chmod 600 body.secret
dexafix secrets body-token-key > body-token.key && chmod 600 body-token.key
dexafix secrets body-token-public --key-file body-token.key > body-token.pub
```

`body-token.key` goes to the api process only. `body-token.pub` and
`body.secret` go to the body service, and `body.secret` to the api process too.
The body secret must differ from the service secret and from the worker secret:
`serve api` refuses to start otherwise. The key files carry a label
(`ed25519-private:` or `ed25519-public:`), and `serve body` refuses a private key
where the public one belongs.

Start the body service, with a database and an encryption key of its own:

```bash
dexafix serve body --listen :8082 \
  --db-driver postgres --dsn-file body.dsn \
  --encryption-key-file body.key \
  --body-secret-file body.secret \
  --body-token-public-key-file body-token.pub
```

Then point the api process at it:

```bash
dexafix serve api --internal-listen :8081 \
  --service-secret-file service.secret --worker-secret-file worker.secret \
  --db-driver postgres --dsn-file main.dsn --encryption-key-file main.key \
  --body-url http://body:8082 \
  --body-secret-file body.secret --body-token-key-file body-token.key
```

With `--body-url` the api process does not create or use the tables of the body
service in its own database. Keep the body service's port on a network that only
the api process can reach. All flags are in [Configuration](02-configuration.md).

### PostgreSQL role and schema

The Compose example creates them with `dexafix db provision` (the `db-setup`
container). For a database that already exists you can run that command with an
administrator's connection string, or run this once as the database owner, with
your own password:

```sql
CREATE ROLE dexafix_body LOGIN PASSWORD '<password>';
CREATE SCHEMA body AUTHORIZATION dexafix_body;
ALTER ROLE dexafix_body SET search_path = body;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
```

The body service's connection string then selects the schema, for example
`postgres://dexafix_body:<password>@db:5432/dexafix?search_path=body&sslmode=disable`.
The service migrates its own tables into that schema on start, and keeps its
migration count in its own table (`goose_body_version`).

With SQLite, give the body service a file of its own, for example
`--dsn file:/config/body.db`.

## Keys

In the combined process the key of the readings and scans is derived from the
encryption key with HKDF, under a label of its own, so one key file covers both
and `db rekey` rotates both. A value sealed for the readings does not open with
the key of the rest.

A body service in its own process has its own `--encryption-key-file` (or
`DEXAFIX_ENCRYPTION_KEY`), checked against its database like the main key (see
[Encryption](08-encryption.md)). Use the same file as the api process to get the
same key, or a different one to keep a leak of one key from opening the other. A
wrong key is refused at start with a message that says so. The body key is a
second key to back up: **without it the readings and scans cannot be read.**

Rotate it with the body service stopped, pointing `--dsn` and the key flags at
the body service's database and key:

```bash
DEXAFIX_ENCRYPTION_KEY=<new> DEXAFIX_ENCRYPTION_KEY_PREVIOUS=<old> \
  dexafix db rekey --body --dsn-file body.dsn --dry-run
```

Run it again without `--dry-run` to re-encrypt. It completes or changes nothing,
and can be repeated.

## Rotating the secret and the token key

- **Body secret.** Both files hold one secret per line. Add a new first line to the
  api process's file and keep the old line in the body service's file until all
  api processes use the new one: the api process sends the first line, the body
  service accepts every line. Then remove the old line.
- **Token key.** Make a new key pair. Add the new public key as another line in the
  body service's `body-token.pub`, restart it, switch the api process to the new
  private key, then remove the old public key line.

## Mutual TLS

The link can be wrapped in mutual TLS, on top of the secret and the tokens. Make a
set for this link, with a CA of its own, so that a certificate of the link between
web and api opens nothing here:

```bash
dexafix secrets mtls --link body --dir ./mtls-body --name body
```

It writes `ca.pem`, `body.pem`, `body.key`, `api-client.pem` and
`api-client.key`. Give the body service
`--body-tls-cert-file`, `--body-tls-key-file` and `--body-tls-client-ca-file`, and
the api process an `https://` `--body-url` with `--body-ca-file`,
`--body-client-cert-file` and `--body-client-key-file`. Either all three files are
given or none, and an `https://` address needs all of them.

## Deleting users

When an administrator deletes a user, the api process also removes the user's
readings and scans from the body service. If the body service cannot be reached
then, the data stays until the maintenance loop finds it: every six hours it
compares the users that have data with the users that exist and removes the data
of those that are gone. User ids are never reused, so this is safe.

## When the body service is down

Everything that needs readings, scans or estimates answers with a 503 page or
status: the Scale, DEXA and Estimates pages, the export, the readings and scans
endpoints of the [JSON API](11-api.md). A Withings import waits: the worker
reports its result again until the body service answers, and the job is not lost.
Signing in, settings, the admin pages and the rest keep working, and the Admin
page shows the two counts as "Unavailable".

## Management commands

The `db`, `user` and `settings` commands open the main database. When the body
service has its own database, give them `--without-body` (or
`DEXAFIX_WITHOUT_BODY=true`), or `db migrate up` creates empty body tables in the
main database. `db copy` is for databases that hold both. The body service
migrates its own database on start (`--auto-migrate`, on by default).

## Moving to this layout

A database from an earlier build is not migrated: the readings and scans are now
stored in a different form, and a body database of its own starts empty. Export
each user's data first and import it afterwards (see
[Accounts and sign-in](07-accounts-and-sign-in.md#your-data)).

## Limits

- A call is not atomic across the two services. An import is applied in steps
  that can be repeated, so a run that stops half way is finished by running it
  again.
- An export is built in memory in the body service and the api process.
- Backups of the two databases are separate: restore them from the same moment, or
  readings of users who were deleted in between wait for the next sweep.

## Troubleshooting

- **401 from the body service, in the api log as "refused a call".** The secrets
  differ, the public key does not belong to the private key, or the clocks differ
  by more than a few seconds. Check the files and the time.
- **"the body encryption key does not match this database".** Another key file
  than the one that made the readings. Restore the right one, or keep the old key
  as a previous key and run `db rekey --body`.
- **"the readings and scans are unavailable".** The api process cannot reach the
  body service: check `--body-url`, the network and that the service runs.
- **`serve api` says the secrets must differ.** The service, worker and body
  secrets each open a different door. Create a new one with
  `dexafix secrets body-secret`.
