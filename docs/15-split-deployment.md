# Split deployment

`dexa-fix serve` runs everything in one process. The same app can also run as
two processes:

- `dexa-fix serve web` is the frontend. It renders the pages, serves static
  files and owns the public listeners: plain HTTP, HTTPS and HTTP/3. It has no
  database and no encryption key.
- `dexa-fix serve api` is the main backend (`pkg/backend/main`). It owns the database, the encryption
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
dexa-fix secrets service-secret > service.secret && chmod 600 service.secret

dexa-fix serve api --listen 127.0.0.1:8082 --internal-listen 127.0.0.1:8081 \
  --service-secret-file service.secret

dexa-fix serve web --listen 127.0.0.1:8080 --backend-url http://127.0.0.1:8081 \
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
    image: ghcr.io/trueforge-org/dexa-fix:<tag>
    command: ["serve", "api"]
    volumes:
      - dexa-data:/data
    environment:
      DEXAFIX_DSN: file:/data/dexa-fix.db
      DEXAFIX_ENCRYPTION_KEY_FILE: /data/dexa-fix.key
      DEXAFIX_SERVICE_SECRET_FILE: /run/secrets/dexafix_service
    secrets:
      - dexafix_service
    expose:
      - "8080"
      - "8081"
    networks:
      - internal

  web:
    image: ghcr.io/trueforge-org/dexa-fix:<tag>
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

## Service secret

The web and api processes share a service secret. Web sends it with every
call to the internal listener, and the api process refuses calls without it.

Create the file with one command:

```bash
dexa-fix secrets service-secret > service.secret && chmod 600 service.secret
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
dexa-fix serve api --listen :8080 --internal-listen :8081 \
  --service-secret-file /run/secrets/dexafix_service

# Public-only api processes on the same database
dexa-fix serve api --listen :8080 --internal-listen "" --maintenance=false
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
  for example with `dexa-fix settings reset-network`, within about 5 seconds.
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
dexa-fix secrets mtls --dir ./mtls --name api
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
dexa-fix serve api \
  --internal-tls-cert-file mtls/api.pem \
  --internal-tls-key-file mtls/api.key \
  --internal-tls-client-ca-file mtls/ca.pem \
  --service-secret-file service.secret
```

On the web host:

```bash
dexa-fix serve web --backend-url https://api:8081 \
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

1. Create the new set: `dexa-fix secrets mtls --dir ./mtls-new --name api`.
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

The Compose example in `deploy/docker-compose.yaml` has a commented block for
mTLS at the end of the file. The image runs as uid 65532, which cannot write
into `./secrets`, so create the files as your own user:

```bash
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/secrets:/out" \
  ghcr.io/trueforge-org/dexa-fix:<tag> secrets mtls --dir /out/mtls --name api
chmod 444 secrets/mtls/*.key
```

The `--name api` matches the service name, which web reaches as `api:8081`.
The keys must be readable by uid 65532 inside the container, as the other
secrets are (`init-secrets.sh` sets 444). The `secrets/mtls` directory stays
private on the host, and the containers get the files through their mounts.

Then uncomment the block and add the new secret names to the `secrets` lists of
the api and web services.

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

Visitors see `dexa-fix is temporarily unavailable, try again in a moment` with
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
