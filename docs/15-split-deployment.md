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
- Exactly one api process serves the internal listener. Any number of api
  processes may serve the public API only, on the same database. See
  [Running more api processes](#running-more-api-processes).
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

Run exactly one process with the internal listener, and it also runs the
maintenance. Public-only processes turn both off:

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
- **Memory is per process.** Failed-login throttling, single-use nonces and the
  Strength Level sync progress are held in memory in each api process. Each
  process counts on its own. See [Limitations and security](14-limitations-and-security.md).
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
