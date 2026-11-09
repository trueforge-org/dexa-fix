---
applyTo: "deploy/**,Dockerfile,.dockerignore"
paths:
  - "deploy/**"
  - "Dockerfile"
  - ".dockerignore"
description: "Rules for the Docker Compose advisory, the secrets script and the image: networks, secrets, hardening, and how to test it."
---

# Deployment instructions

`deploy/docker-compose.yaml` is the recommended deployment: PostgreSQL, the main backend (`serve api`), the web frontend (`serve web`) and one worker container per provider (`serve worker --providers ...`). `deploy/init-secrets.sh` creates the secrets. The `Dockerfile` builds from the prebuilt binary (goreleaser); the final image is `scratch`.

## Rules

- **Networks:** the database is only on an internal network shared with the backend; the web and backend share another internal network; only `web` is on a network that publishes ports. The internal API port (8081) is never published. The workers are on the `link` network (to reach the api) and on `egress`, a network with outbound access and no published ports: they are the only containers that talk to Withings and Strength Level, so `api` needs no internet. A worker has no database access and no encryption key.
- **Two different secrets:** `service_secret` (web ↔ api) and `worker_secret` (workers ↔ api) open different doors on the api's internal listener; `serve api` refuses to start if they are equal. Only the containers that need a secret get it.
- **Secrets** are Compose `secrets:` mounted at `/run/secrets/...` and passed by `*_FILE` variables, never as plain environment values or command-line arguments. `init-secrets.sh` never overwrites an existing file (losing the encryption key makes stored secrets unreadable) and uses `0700` for the directory and `0444` for the files so the unprivileged user (65532) in the container can read them. Do not commit `deploy/secrets`.
- **Hardening:** `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges`, an unprivileged user (the image sets it). Do not add `privileged`, host networking or extra capabilities.
- **Public API:** off by default in the example (`--listen=`); document the way to turn it on instead of publishing it silently.
- Pin versions in examples with a variable (`DEXAFIX_VERSION`), and keep both containers on the same version.
- The container healthcheck of the app images is not possible (no shell in `scratch`); rely on `/healthz` and `/readyz` of the web process from outside.
- Keep the `Dockerfile` defaults consistent with the flags (`DEXAFIX_LISTEN`, `DEXAFIX_INTERNAL_LISTEN`, `DEXAFIX_DSN`, `DEXAFIX_ENCRYPTION_KEY_FILE`) and `EXPOSE` the ports.

## Test what you change

- `docker compose -f deploy/docker-compose.yaml config -q` must pass (set `DEXAFIX_IMAGE` and `DEXAFIX_VERSION` if needed; create temporary empty secret files in a scratch copy, never in the repository).
- Run it for real. If the Docker daemon runs on another host than your shell (a dev container with Docker Desktop), bind-mounted files are not visible to it: put the secrets in a volume and start the same containers with `docker run` using the same image, networks, environment and options as the compose file, then check sign-up, pages, `/readyz`, the isolation between networks, and stopping and starting the backend.
- The image needs the binary in the build context: `CGO_ENABLED=0 go build -trimpath -o dexa-fix . && docker build -t dexa-fix-test .`. Clean up every container, network, volume and image you created.
