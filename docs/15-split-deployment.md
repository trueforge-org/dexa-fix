# Split deployment

`dexafix serve` runs everything in one process. The same app can also run as
two processes:

- `dexafix serve web` is the frontend. It renders the pages, serves static
  files and owns the public listeners: plain HTTP, HTTPS and HTTP/3. It has no
  database and no encryption key.
- `dexafix serve api` is the main backend (`pkg/backend/main`). It owns the database, the encryption
  keys and every security decision. It also runs maintenance: the audit log
  purge and the renewal of the self-signed certificate.

In the source the two halves are separate packages: `pkg/frontend` cannot
import anything under `pkg/backend/`, the store or a database driver (a test
checks it), and `pkg/shared` holds the contract between them.

Splitting them has two uses. A flood of requests to the frontend cannot starve
the backend, because each process has its own load budget. And the process
that faces visitors holds neither the database nor the keys.

## Topology

```text
                      visitors
                         |
                         v
        reverse proxy (path split, see 03-reverse-proxy.md)
              |                              |
        /api/ |                              | everything else
              v                              v
   api :8080 (public API)           web :8080 (pages, static files)
                                             |
                                             | internal link, /internal/v1
                                             | service secret, private network
                                             v
                                    api :8081 (internal API)
```

- web talks to the api process only over the internal listener. Keep that
  listener on a private network.
- Any number of api processes may share one database, and exactly one of them
  runs the maintenance (`--maintenance=false` on the others). The URL web uses
  for the backend may point at a load balancer in front of them. See
  [Running more api processes](#running-more-api-processes).
- Workers (`serve worker`) call the same internal listener with the worker
  secret and reach the outside services for the api process. See
  [Workers](16-workers.md).
- The scale readings, DEXA scans and estimates are held by the
  [body service](17-body-service.md), which runs inside the api process unless
  you start `serve body` and give `serve api` a `--body-url`.
- The HTTPS and HTTP/3 listeners belong to web. See
  [TLS and HTTP/3](#tls-and-http3).

## Quick start

Two processes on one host, for trying it out. Create a service secret, start
the api process, then the web process:

```bash
dexafix secrets service-secret > service.secret && chmod 600 service.secret

dexafix serve api --listen 127.0.0.1:8082 --internal-listen 127.0.0.1:8081 \
  --service-secret-file service.secret

dexafix serve web --listen 127.0.0.1:8080 --backend-url http://127.0.0.1:8081 \
  --service-secret-file service.secret --api-upstream http://127.0.0.1:8082
```

Start the api process first. Otherwise web waits for it, see
[Troubleshooting](#troubleshooting).

Here no reverse proxy routes `/api/`, so web passes it on with
`--api-upstream`. The api process must then trust web as a proxy. On the Admin
page, set **Trusted proxies** to `loopback`. Open <http://localhost:8080> and
create the first account.

## Compose example

Both services use the same image and the same tag. The api process has the
database and the key. Only web publishes ports.

```yaml
services:
  api:
    image: ghcr.io/trueforge-org/dexafix:<tag>
    command: ["serve", "api"]
    volumes:
      - dexa-data:/config
    environment:
      DEXAFIX_DSN: file:/config/dexafix.db
      DEXAFIX_ENCRYPTION_KEY_FILE: /config/dexafix.key
      DEXAFIX_SERVICE_SECRET_FILE: /run/secrets/dexafix_service
    secrets:
      - dexafix_service
    expose:
      - "8080"
      - "8081"
    networks:
      - internal

  web:
    image: ghcr.io/trueforge-org/dexafix:<tag>
    command: ["serve", "web"]
    environment:
      DEXAFIX_BACKEND_URL: http://api:8081
      DEXAFIX_SERVICE_SECRET_FILE: /run/secrets/dexafix_service
    secrets:
      - dexafix_service
    ports:
      - "8080:8080"
      - "8443:8443"
      - "8443:8443/udp"
    networks:
      - internal
      - default

networks:
  internal:
    internal: true

secrets:
  dexafix_service:
    file: ./service.secret

volumes:
  dexa-data:
```

This example runs the body service inside the api container. The files in
`deploy/` run it as a container of its own, with a database role, an encryption
key and a secret of its own: see [Body service](17-body-service.md).

The image's default command is `serve`, which runs one process. The split
needs the `command` line for each service.

This example publishes only web, so the public API is not reachable from
outside yet. Choose one:

- Put a reverse proxy on the `internal` network, and route `/api/` to
  `api:8080` and everything else to `web:8080`. See
  [Two processes: path split](03-reverse-proxy.md#two-processes-path-split).
- Set `DEXAFIX_API_UPSTREAM: http://api:8080` on web. Then list the web
  container's address among the trusted proxies on the Admin page. The
  `private` keyword covers Docker's bridge ranges.

## The recommended deployment: `deploy/`

`deploy/docker-compose.yaml` runs everything as containers: PostgreSQL, Valkey,
the api process, the body service, the web process and one worker container per
provider. Every link between them is protected by a secret **and** by mutual TLS.
Two short-lived containers prepare the rest: `init`, which is `dexafix secrets
init` and makes every password, key, secret and certificate, and `db-setup`,
which is `dexafix db provision` and makes the database roles and the schema.

```bash
cd deploy
mkdir -m 700 secrets && sudo chown 568:568 secrets
docker compose up -d
```

Open <http://localhost:8080> and create the first account: it becomes the
administrator. No file has to exist before `up` except that folder, because the
containers read their secrets from volumes that `init` fills. `init` and
`db-setup` run again at every `docker compose up -d`.

### Who `init` runs as

`init` runs as the unprivileged user `568:568`, the user of all the other
containers, with no capabilities. That keeps a root process out of the
deployment, but it means the folders `init` writes must already belong to that
user:

| Folder | Must be | Who makes it so |
| --- | --- | --- |
| `./secrets` on the host (mounted at `/secrets/host`) | Owned by `568:568`, mode `700` | You, once: `mkdir -m 700 secrets && sudo chown 568:568 secrets`. A folder that Docker makes by itself belongs to root. |
| The volumes `secrets-api`, `-web`, `-worker`, `-body`, `-db`, `-dbsetup`, `-valkey` | Owned by `568:568` | The image: a new volume copies the owner of its mount point. A volume that an earlier version made may belong to root. |

If an owner or a mode would have to change and `init` is not allowed to change
it, `init` stops with an error that says what to do, and nothing else starts:

```text
open /secrets/host/.service_secret.123: permission denied

init runs as 568:568 and may not change the owner or the permissions of this folder. Do one of these, once:
  - Run init as root this one time. ...
        docker compose run --rm --user 0:0 init
  - Or prepare the folder on the host yourself:
        sudo chown -R 568:568 ./secrets && sudo chmod 700 ./secrets
```

You can also skip the preparation and run `init` as root the first time. As
root it gives `./secrets`, the files in it and the volumes to `568:568` (it has
the two capabilities it needs for that, `CHOWN` and `DAC_OVERRIDE`, and
nothing else). From then on nothing has to change, so the plain `up` runs it as
`568:568` again:

```bash
cd deploy
docker compose run --rm --user 0:0 init   # once: fixes the owner of ./secrets and the volumes
docker compose up -d                      # every time after that
```

Another user id than `568` works too (set `user:` and `--chown=` of `init` to
it, and run the other containers as it), but then the same rule holds.

What `init` makes, and where it keeps it:

| Where | What | When it changes |
| --- | --- | --- |
| `./secrets/` on the host | The database passwords, the connection strings, the Valkey password and the two encryption keys (`encryption_key`, `body_encryption_key`) | Never: a file that exists is kept |
| Volume `secrets-api` | The api's connection string and encryption key, the service, worker and body secrets, the body token signing key, the Valkey URL, the api's certificates | The secrets and the key are kept; the certificates are new at every run |
| Volumes `secrets-web`, `secrets-worker`, `secrets-body` | Only what that container needs: its shared secret and its certificates (the body service also gets its connection string and encryption key, and the public half of the signing key) | As above |
| Volumes `secrets-db`, `secrets-dbsetup`, `secrets-valkey` | The superuser's password (the database), the administrator's connection string and the two role passwords (`db-setup`), the Valkey password | They follow the files in `./secrets` |

**Back up `./secrets/encryption_key` and `./secrets/body_encryption_key`
together with the database.** Without an encryption key the encrypted values
cannot be read again. They are on the host, so `docker compose down -v` does not
delete them. `./secrets` is private to `568:568` (mode `700`); read it with `sudo`.

The certificates are valid for 397 days and are replaced at every `docker
compose up`, so run it at least once a year. The CA's private key is thrown away
as soon as the certificates are made. The containers read changed certificate
files again within 30 seconds and need no restart. While they do, a process may
be briefly ahead of the others: expect one `bad certificate` line in the api's
log and a `could not refresh the settings` warning in web's, which heal by
themselves. The web process and the workers share one client certificate: it
proves the link, and the service or worker secret proves the role. The link to
the body service has a CA of its own, so a certificate of one link opens nothing
of the other.

The database image is `ghcr.io/trueforge-org/postgresql` (ContainerForge). It
runs as uid 568 and keeps its data in `/config/18`, and so do the other images
of this stack: DexaFix runs as uid 568 too and keeps its data in `/config`. The
`db-setup` container runs [`dexafix db provision`](05-commands.md) against it,
so the application never runs as the superuser `postgres`. The api's role
`dexafix` owns the database; the body service's role `dexafix_body` owns the
schema `body` and can use nothing else. The image can make no roles from
environment variables (its `ADDITIONAL_DBS` setting creates databases owned by
the superuser, and nothing else), and it reads the superuser's password from the
environment only, so the Compose file exports it from the secret file in a small
`entrypoint`. `db provision` can be repeated: what exists is kept and the
passwords are set again, so a failed start is mended by the next `up`, and a
password you changed in `./secrets` takes effect there too.

The Docker daemon may run on another host than your shell (Docker Desktop with a
dev container). Then a bind mount such as `./secrets` is not visible to it: use a
normal host, or put `./secrets` in a volume as the test setup in
`deploy.instructions.md` does.

### Resource limits

Every container in the Compose file has a ceiling on memory, CPU and processes,
so that one runaway process (a flood of requests, a very large import, a leak)
cannot starve the others on the same host. The defaults are generous for a
household or a small team:

| Container | Memory | CPUs | Processes | Why |
| --- | --- | --- | --- | --- |
| `api` | 512 MB | 2 | 256 | Sign-in work (bcrypt) and the pages' data |
| `body` | 768 MB | 2 | 256 | The estimates: CPU heavy, and it keeps the results of up to 32 users in memory (about 160 MB at most) |
| `web` | 256 MB | 1 | 256 | Pages and static files |
| `worker-withings`, `worker-strengthlevel` | 256 MB | 0.5 | 128 | Waiting for jobs and talking to the provider |
| `db` | 1 GB | 2 | 512 | PostgreSQL, with up to 20 connections each from `api` and `body` |
| `valkey` | 512 MB | 1 | 128 | At most 256 MB of data (`--maxmemory`), more while it rewrites its file |
| `init`, `db-setup` | 128 MB | 0.5 | 64 | They run for a moment and exit |

The Go containers also get `GOMEMLIMIT` at about 80% of their memory limit, so
the garbage collector works harder before the kernel kills the process. Go also
reads the CPU limit, so a container limited to one CPU does not start a thread
pool for sixteen.

Change a limit in a `docker-compose.override.yaml` next to the file (Compose
merges it), and raise `GOMEMLIMIT` with it:

```yaml
services:
  body:
    deploy:
      resources:
        limits: {memory: 2G}
    environment:
      GOMEMLIMIT: 1600MiB
```

`docker stats` shows the use against the limit. A container that is killed for
using too much memory shows `OOMKilled: true` in `docker inspect`; raise its
limit then. Check the PostgreSQL side too: each of `api` and `body` opens up to
20 connections, so the server's `max_connections` (100 by default) must cover
every `api` and `body` container you run, plus some for yourself.

## Status and health checks

**The status page.** `/status` on the web process shows the health of each part:
the web process itself, the api process, the database, the body service and the
workers of each provider. It is the same for everybody, signed in or not. It
shows a state per part and nothing else: no versions, host names, worker names
or reasons. It answers 503 while a part is down, so a monitor needs no parsing,
and 200 otherwise. A cache cannot mix visitors up (every response is
`no-store`, and the list is identical for all).

| State | Meaning |
| --- | --- |
| Operational | It works. |
| Down | It does not answer. The api process cannot be reached, the database or the body service does not answer within 2 seconds, or workers of a provider registered in the last day but none has asked for work in the last 90 seconds. |
| Not in use | No worker of that provider has registered in the last day. That is not a fault: imports of that provider simply wait until one starts. |

A worker gets a new random name every time it starts, so workers of replaced
containers stay on record for a day. They count for nothing once one worker of
the provider is running. Give a worker a fixed `--worker-id` if you want it to
keep its name.

The api process keeps its answer for 5 seconds, so a flood of requests to
`/status` costs one round of checks per 5 seconds.

Administrators also get `/admin/status` (linked from `/status` and from the
Admin page): the workers by name, version and last request, and the reason a
part is down. It needs the same sign-in and second step as the rest of the Admin
pages.

**Health checks for containers.** The images are `scratch`: no shell, no curl.
Three processes have an optional listener that answers `/healthz` and `/readyz`
only:

```bash
dexafix serve api --health-listen 127.0.0.1:8083 ...
dexafix serve body --health-listen 127.0.0.1:8083 ...
dexafix serve worker --health-listen 127.0.0.1:8083 ...
```

- `/healthz` answers `ok` while the process runs.
- `/readyz` answers `ok` when it can do its work, and 503 `not ready` otherwise.
  For the api process and the body service that means the database answers. A
  worker is ready once it has registered and has heard from the api process in
  the last 90 seconds. The reason is only in the log, at the debug level.

The listener needs no secret or certificate, which is why it is a separate one,
and it answers nothing else. Bind it to the loopback address. The web process
needs no extra listener: its own `/healthz` (the process runs) and `/readyz`
(it can reach the api process) are on its normal listener.

### Profiling

To see where an api, body or worker process spends its time, start it with
`--pprof` (or `DEXAFIX_PPROF=true`) together with a loopback
`--health-listen`:

```bash
dexafix serve api --health-listen 127.0.0.1:8083 --pprof ...
go tool pprof http://127.0.0.1:8083/debug/pprof/profile?seconds=30
```

The profiling pages are served on the health listener only, and the process
refuses to start when `--pprof` is set without a loopback `--health-listen`,
because a profile shows memory and stacks. In a container, run `go tool pprof`
from the host through `docker exec` or a published loopback port. The web
process has no health listener and no profiling pages. `--pprof` is off by
default.

`dexafix healthcheck <url>` makes one request and exits 0 for a 2xx answer, so
the container can check itself:

```yaml
healthcheck:
  test: ["CMD", "/dexafix", "healthcheck", "http://127.0.0.1:8083/readyz"]
  interval: 10s
  timeout: 5s
  retries: 5
  start_period: 20s
```

`deploy/docker-compose.yaml` does this for every container and starts them in
order: the database, the body service, the api process, then the web process and
the workers, each when the one before is healthy. The api process does not wait
for the body service in its own `/readyz`: when the body service is down the
pages say so (503) and everything else keeps working, and `/status` shows it.

## Valkey

By default the failed sign-in counters, the single-use values (second-step
codes, password reset links, and the like) and the sessions are rows in the
database. Every wrong password is a write and every page of a signed-in visitor
is a session lookup, so a flood of either is a flood of database work. With
[Valkey](https://valkey.io) (or any Redis-compatible server) the api process keeps
them there instead and the database sees none of it. It is optional and only
`serve api` takes it:

```bash
echo 'redis://:a-long-password@valkey:6379/0' > valkey.url && chmod 600 valkey.url
dexafix serve api --valkey-url-file valkey.url ...
```

`--valkey-url` (`DEXAFIX_VALKEY_URL`) takes the URL without a password; a
password on the command line is refused, as for the database. `rediss://` turns
on TLS. At start the api process connects and checks that the server answers;
if it does not, it stops.

What it holds, and what it does not:

- **Held in Valkey:** the failed-attempt counters (one sorted set per login name
  and per client address, the same 15-minute sliding window as in the database)
  the single-use values (one key each, with a life until the value would have
  expired anyway, plus a minute for clock differences) and the sessions (one key
  each, which expires with the session, and an index of every user's sessions so
  that they can all be ended at once). A session holds only who signed in and
  how: the person's name and role are read from the database on every request,
  so a role change or a deleted account takes effect at once.
- **Sessions end on purpose, not by a cache.** Signing out, a changed password,
  a password set by an administrator, a reset link and a deleted account each
  end the sessions in Valkey themselves. For the password cases that happens
  before the password is written, so if Valkey cannot be reached the password is
  not changed and the old sessions are not left alive.
- **Not held in Valkey:** the job queue and everything else. The estimate cache
  and the sign-in provider discovery cache are in each process's memory, as
  before.
- **The three are never mixed.** With Valkey the tables `auth_failures`,
  `used_nonces` and `sessions` stay empty. Several api processes must all use
  it, or none. Turning it on or off forgets who failed lately and **signs
  everybody out**.

**It fails closed.** While Valkey does not answer, every address counts as locked
out, every single-use value as spent and no session can be read, so nobody can
sign in or stays signed in (their sessions are there again when it is back). Pages that need
no sign-in keep working, and `/status` shows Valkey as down. This is the same
choice as when the database does not answer: a limit that gives way when its
store is unreachable is no limit.

**Run it with `maxmemory-policy noeviction`.** A counter that was evicted would
let an attacker try again, a nonce that was evicted would let a one-time value
be used twice, and a session that was evicted would sign somebody out. A full server then refuses writes, which fails closed. Keep the
append-only file on a volume so a restart keeps the counters.

The Compose example (`deploy/docker-compose.yaml`) has it: `init` makes the
password (`./secrets/valkey_password`, kept like the other passwords) and the URL
for the api container. The `valkey` container is on a network of its own that
only the api container shares, and the api container waits for it to be healthy.
To run the example without Valkey, remove the `valkey` service, the
`DEXAFIX_VALKEY_URL_FILE` line of `api` and the `state` network; the same things
are then rows in the database.

## Service secret

The web and api processes share a service secret. Web sends it with every
call to the internal listener, and the api process refuses calls without it.

Create the file with one command:

```bash
dexafix secrets service-secret > service.secret && chmod 600 service.secret
```

The command prints one secret of 64 hexadecimal characters. The file holds one
secret per line. Blank lines and lines starting with `#` are ignored. Each
secret must be at least 32 characters.

- Web sends the **first** line.
- The api process accepts **every** line.

A group- or world-readable file only logs a warning, because container secret
mounts are often mode 0444.

### Rotating the secret

To rotate, put the new secret on the first line and keep the old one below it:

```text
# service.secret
<new secret>
<old secret>
```

1. Edit `service.secret` as above.
2. Restart the api process first, so that it accepts both secrets.
3. Restart the web process, so that it sends the new secret.
4. Remove the old line, and restart the api process again.

## Running more api processes

The api process has two listeners, and each can run in its own process:

- `--listen`: the public API, `/api/v1`, behind the reverse proxy.
- `--internal-listen`: the internal API for web. Keep it on a private network.

Any number of processes may serve the internal listener, behind a load
balancer if you like. Exactly one process runs the maintenance (audit purge,
cleanup of nonces and lock-outs, the self-signed certificate); turn it off with
`--maintenance=false` on all the others. Public-only processes turn the internal
listener off as well:

```bash
# The one api process for web: internal listener and maintenance on
dexafix serve api --listen :8080 --internal-listen :8081 \
  --service-secret-file /run/secrets/dexafix_service

# Public-only api processes on the same database
dexafix serve api --listen :8080 --internal-listen "" --maintenance=false
```

A public-only process needs no service secret, because it has no internal
listener.

Some limits apply:

- **Load budgets are per listener.** The public listener admits up to 32
  requests at once (never more than half of the database connections: 2 with
  SQLite, 10 with Postgres), queues 64 and allows 20 requests per second per
  client. The internal listener admits 128, queues 256 and allows 50 requests
  per second per client. So a flood on the public API cannot starve web, and
  it always leaves database connections for it. Excess requests get 503 or 429
  with `Retry-After`. Password checks for Basic-auth API requests are limited
  to a quarter of the CPUs (at least one) at a time, so a flood of
  credentials cannot use up all the CPU.
- **Settings reload.** A process reloads settings that another process changed,
  for example with `dexafix settings reset-network`, within about 5 seconds.
- **State is shared through the database.** Failed-login lock-outs, single-use
  nonces and the Strength Level sync status are stored in the database, so all
  api processes agree. Only caches (estimates) and the de-duplication of noisy
  audit events are per process. A Strength Level sync runs on the backend that
  started it. Only the maintenance process generates or renews a self-signed
  certificate; the others load the stored one. See
  [Limitations and security](14-limitations-and-security.md).
- **Database.** SQLite works for two api processes on one host, in WAL mode,
  but it still serialises writes. Use Postgres when isolation or several
  processes matter.

## TLS and HTTP/3

Web serves HTTPS and HTTP/3. The certificate and its key are stored in the api
process, and you set them on the Admin page as usual. Web fetches them over the
internal link. Web follows changes to the settings about every 5 seconds, and at
once after a save on the Admin page. Web's HTTPS port must be published, for
example `8443:8443` and `8443:8443/udp` in the Compose example.

The private key crosses the internal link, so the internal link must stay on a
private network. See [HTTPS, HTTP/2 and HTTP/3](03-reverse-proxy.md#https-http2-and-http3).

If web terminates TLS itself, a proxy that only passes TCP cannot split the
paths. Set `DEXAFIX_API_UPSTREAM` on web, as in the Compose example, or
terminate TLS at the proxy and split the paths there.

## Mutual TLS between web and api

Mutual TLS (mTLS) makes both ends of the internal link prove who they are with
a certificate. Use it for any link that leaves one host or private network, and
as defence in depth on one host. The service secret stays required.

With mTLS the internal listener speaks TLS 1.3 only and refuses plain HTTP. The
api process accepts only a client certificate issued by a private CA, and web
checks the api process's certificate against the same CA.

### Create the certificates

```bash
dexafix secrets mtls --dir ./mtls --name api
```

`--name` is each host name or IP address that web uses to reach the api
process. Repeat it for each name. `localhost`, `127.0.0.1` and `::1` are always
included, and at least one `--name` is required. The command writes:

- `ca.pem`: the CA certificate. Both processes trust it.
- `api.pem` and `api.key`: the certificate and key of the api process.
- `web.pem` and `web.key`: the certificate and key of the web process.

Keys are mode 0600, certificates 0644 and the directory 0700. The command never
overwrites a file: if any of the five exists, it writes nothing. The CA's
private key is not kept, so no further certificates can be issued from this CA.
Leaf certificates are valid for 2 years and the CA for 10 years. To renew,
create a new set and follow [Rotating the certificates](#rotating-the-certificates).

### Start both processes

On the api host:

```bash
dexafix serve api \
  --internal-tls-cert-file mtls/api.pem \
  --internal-tls-key-file mtls/api.key \
  --internal-tls-client-ca-file mtls/ca.pem \
  --service-secret-file service.secret
```

On the web host:

```bash
dexafix serve web --backend-url https://api:8081 \
  --backend-ca-file mtls/ca.pem \
  --backend-client-cert-file mtls/web.pem \
  --backend-client-key-file mtls/web.key \
  --service-secret-file service.secret
```

Each flag has an environment variable, see [Configuration](02-configuration.md).

- The api process takes all three `--internal-tls-*` options or none. It needs
  `--internal-listen` to be on.
- Web needs an `https://` URL in `--backend-url` together with all three
  `--backend-*-file` options. The files with an `http://` URL are an error.
- If the name in the backend's certificate is not the host of `--backend-url`,
  set `--backend-server-name` to the name in the certificate.

### Rotating the certificates

Both processes read the certificate files again when they change. They check
every 30 seconds. A broken or half-written file is ignored, and the last good
one stays in use, so rotating needs no restart. Write the new file next to the
old one and `mv` it into place.

To rotate without a gap, for the CA or a leaf certificate:

1. Create the new set: `dexafix secrets mtls --dir ./mtls-new --name api`.
2. Concatenate the old and the new `ca.pem` into one file, and put that file in
   the CA file of both processes.
3. Wait at least 30 seconds.
4. Put the new `api.pem` and `api.key` in the api process, and the new
   `web.pem` and `web.key` in the web process.
5. Wait at least 30 seconds, then put only the new `ca.pem` in the CA file of
   both processes. The old certificates stop working here.

### What it protects

- A client without a certificate from the CA cannot reach the internal listener,
  even with the service secret.
- Web checks the name and the server-auth key usage of the api process's
  certificate. An impostor on the network cannot receive the service secret.
- The link is encrypted with TLS 1.3, so sessions, passwords and the TLS private
  key do not cross it in clear.

### What it does not protect

- A stolen web host. Its client key and the service secret let an attacker act
  as web, and web can ask the api process for the TLS private key of the HTTPS
  listener.
- The public listener. mTLS does not apply to it: keep it behind the reverse
  proxy.
- Revocation. There is no certificate revocation list. To cut off a leaked
  client key, rotate the CA as above and drop the old one.
- Key files on disk. Whoever can read `web.key` and `service.secret` is web.

### Compose

`deploy/docker-compose.yaml` has mutual TLS on by default for both internal
links (web and workers to api, and api to the body service): the `init`
container makes the certificates, see
[The recommended deployment](#the-recommended-deployment-deploy). Nothing more
is needed. To make a set by hand for another setup, use `dexafix secrets mtls`
as above, or `dexafix secrets init` for everything at once.

## Troubleshooting

### The backend refused the service secret

Web stops at start with `the backend refused the service secret: use the same
--service-secret-file as the api process`. The api process answered 401.

- The two processes read different files, or the api process does not have
  the first line of the web process's file. Give both the same file, or finish
  the rotation in the order above.

### Web waits for the api process

At start, web logs `waiting for the backend` and retries for up to 60 seconds.
If the api process does not answer in that time, web stops with `the backend
is not available`.

- Start the api process first, or let the container restart.
- Check `--backend-url`, such as `http://api:8081`, from the web host or
  container. The name must resolve, and the api process must not run with
  `--internal-listen ""`.

### The 503 page

Visitors see `dexafix is temporarily unavailable, try again in a moment` with
`Retry-After: 5`. Web could not get an answer from the api process: it is down,
unreachable, or shedding load on its internal listener. Web logs
`backend request failed`.

A plain `Service Unavailable` with `Retry-After: 2` means web itself is at its
limit on requests in progress. That is normal under a flood. If it lasts, limit
the traffic at the reverse proxy.

A `429 Too Many Requests` comes from web's per-client rate limit, or from its
limit on sign-in attempts. Wait for `Retry-After`.

### Versions differ

Web logs `the frontend and the backend run different versions: deploy both
from the same release` at start. Deploy both processes from the same image tag.
