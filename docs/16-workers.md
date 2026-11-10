# Workers

A worker is a process that talks to an outside service (Withings, Strength
Level) for the api process. Only workers reach the outside. The api process
keeps the database and decides what is stored. The web process does neither.

```text
api (database, queue)  <──  worker  ──>  Withings, Strength Level
        internal listener, worker secret
```

A worker has no database and no encryption key. It asks the api process for a
job, and a user's credentials reach it only inside the lease of that job. Every
call about a job carries the job's lease token. The api process hands each job
to exactly one worker, so you can run as many workers as you like.

## Single process and split deployment

- `dexafix serve` runs the built-in workers inside the same process. There is
  nothing to set up.
- In a [split deployment](15-split-deployment.md) you start workers yourself
  with `dexafix serve worker`. Without one, jobs stay queued: imports and
  syncs do not start.

## Start a worker

Create a worker secret. It must differ from the service secret, because the two
open different doors. `serve api` refuses to start when they are equal.

```bash
dexafix secrets worker-secret > worker.secret && chmod 600 worker.secret
```

Give the same file to the api process and to every worker:

```bash
dexafix serve api --worker-secret-file worker.secret ...
dexafix serve worker --backend-url http://api:8081 \
  --worker-secret-file worker.secret --providers withings
```

`--backend-url` is the internal address of the api process, the same one web
uses. Workers use the internal listener and, if you set it up,
[mutual TLS](15-split-deployment.md#mutual-tls-between-web-and-api) with the
`--backend-ca-file`, `--backend-client-cert-file`, `--backend-client-key-file`
and `--backend-server-name` flags. Over plain HTTP at an address that is not
private, the worker logs a warning, because the link carries credentials of
users' accounts at the providers.

The Compose deployment in `deploy/` turns mutual TLS on for the workers: its
`init` container makes the client certificate (the one web uses) and the worker
secret, and gives them to the worker containers only. See
[The recommended deployment](15-split-deployment.md#the-recommended-deployment-deploy).

| Flag                   | Environment variable          | Default | Description                                                              |
| ---------------------- | ----------------------------- | ------- | ------------------------------------------------------------------------ |
| `--backend-url`        | `DEXAFIX_BACKEND_URL`         | empty   | Required. Internal address of the api process.                           |
| `--worker-secret-file` | `DEXAFIX_WORKER_SECRET_FILE`  | empty   | Required. The secret shared with the api process, one per line.          |
| `--providers`          | `DEXAFIX_WORKER_PROVIDERS`    | empty   | Comma-separated: `strengthlevel`, `withings`. Empty runs all.            |
| `--concurrency`        | `DEXAFIX_WORKER_CONCURRENCY`  | `2`     | Jobs this process runs at once, from 1 to 64.                            |
| `--worker-id`          | `DEXAFIX_WORKER_ID`           | random  | Name of this worker on the Admin page.                                   |

`DEXAFIX_DSN_FILE`, `DEXAFIX_ENCRYPTION_KEY`, `DEXAFIX_ENCRYPTION_KEY_FILE` and
`DEXAFIX_SERVICE_SECRET_FILE` mean nothing to a worker. It logs a warning and
ignores them.

The `deploy/` Compose file runs one worker container per provider, so one that
hangs or is blocked cannot hold up the other. Scale one with
`docker compose up -d --scale worker-withings=2`.

## Settings on the Admin page

Which providers a worker runs is decided when it starts. Everything else is on
the **Workers** page of the Admin section, is stored in the database and
reaches every worker without a restart. Per kind of job (for example
*Withings import*):

- **On**: turn a kind off to stop new jobs from starting.
- **Jobs at once, in all workers** and **Attempts per job**.
- **First retry after** (doubled for each further attempt, at most one hour)
  and **Time limit of one attempt**.
- **Automatic run every (hours)**: `0` means no automatic runs.
- **Pause a user's automatic runs after (failures)**: after this many failed
  runs in a row the user's schedule for the kind is paused, with the reason
  shown to the user and the administrator. It starts again when the user fixes
  the connection (disconnect and connect again, or import by hand) or when an
  administrator resumes it.
- **Pause this kind after (failures in a row, all users)**: the circuit
  breaker. It turns the whole kind off until an administrator resumes it.
- Provider options, such as the pause between two writes to Strength Level.

Global settings: the **priority of new accounts** (1 to 100) and how many days
finished jobs are kept. An administrator can set the priority of each account
on the Users page. The Workers page also lists the running workers, with the version
and the time each last asked for work, and the queue, with the state, tries and
detail of each job.

The scheduler that starts automatic runs is part of the maintenance of the api
process (`--maintenance`), so only one process does it.

## When nothing happens

- Jobs stay *queued*: no worker runs that provider. Check the **All workers**
  list on the Admin page, `--backend-url`, and that both sides use the same
  worker secret.
- A worker stops with "the backend refused the worker secret": none of the
  lines in the api process's worker secret file equals the first line of the
  worker's. The file format is the one of the
  [service secret](15-split-deployment.md#service-secret): web sends the first
  line, the receiver accepts every line.
- A kind shows as paused: the circuit breaker or a user's pause fired. The
  reason is on the Workers page.
