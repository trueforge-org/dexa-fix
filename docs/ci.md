# CI and releases

## Badges (paste into README.md)

```markdown
[![CI](https://github.com/trueforge-org/dexafix/actions/workflows/ci.yaml/badge.svg)](https://github.com/trueforge-org/dexafix/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/trueforge-org/dexafix/graph/badge.svg)](https://codecov.io/gh/trueforge-org/dexafix)
[![Latest release](https://img.shields.io/github/v/release/trueforge-org/dexafix)](https://github.com/trueforge-org/dexafix/releases/latest)
[![License](https://img.shields.io/github/license/trueforge-org/dexafix)](LICENSE)
```

## CI and releases

`.github/workflows/ci.yaml` runs on pushes to `main`, pull requests, manually and weekly (Monday):

- **Lint**: gofmt, `go vet`, and a `go mod tidy` check.
- **Test (sqlite)** and **Test (postgres)**: `go test -race` with coverage. The Postgres run uses a
  service container and `DEXAFIX_TEST_POSTGRES_DSN`, so the Postgres migrations are exercised.
  Coverage goes to Codecov with the flags `unit` and `postgres`.
- **govulncheck**: known vulnerabilities in dependencies and the standard library. The version is pinned in `GOVULNCHECK_VERSION` at the top of the workflow and must be kept current: an old release analyses newer Go code with an old copy of `golang.org/x/tools` and can crash at random (exit status 2, `panic: unexpected expr`) instead of reporting. A real finding exits with status 3.
- **Build**: cross-compile for linux/amd64, linux/arm64, darwin/arm64, windows/amd64, plus
  `goreleaser check` and a snapshot release.

### Cutting a release

```sh
git tag v1.0.0
git push origin v1.0.0
```

`.github/workflows/release.yaml` runs GoReleaser and publishes:

- a GitHub release with `tar.gz` (zip on Windows) archives for linux/darwin/windows on amd64/arm64,
  `checksums.txt`, a keyless cosign signature of it (`checksums.txt.sigstore.json`), SBOMs, and a
  changelog grouped by conventional-commit prefix;
- build provenance attestations for the release artifacts.

Run the Release workflow manually (workflow_dispatch) for a dry run that builds a snapshot only.

Every third-party action is pinned to a commit SHA with its version in a comment, because the
release job holds write and signing permissions. Dependabot (github-actions and docker) proposes
the updates. The GoReleaser version is pinned in `.github/goreleaser/Dockerfile` (a file that is never built; it is there so
Dependabot's docker ecosystem proposes updates, and `release.yaml` reads the tag from it).

### Dependency updates

`.renovaterc.json5` is the Renovate configuration, the same as in `trueforge-org/forgetool`: it
extends the organisation preset `github>trueforge-org/renovate-config` (Go modules, GitHub Actions
and Docker images, with pinned digests, semantic commit messages, labels and schedules). One thing is
added here: a custom manager that keeps `GOVULNCHECK_VERSION` in `ci.yaml` current, because an old
govulncheck crashes at random on newer Go code.

Renovate only acts once the Renovate app is enabled for the repository. `.github/dependabot.yml` still
exists and covers the same ecosystems, so while both are on, each update is proposed twice; remove
the Dependabot file (and the Dependabot mentions above) when Renovate has taken over.

### Container

The release builds and publishes no container image. `Dockerfile.ci` is for CI, tests and
automation (the Compose example in `deploy/`, split-mode runs, agents). Build it from the repository:

```sh
CGO_ENABLED=0 go build -trimpath -o dexafix . && docker build -f Dockerfile.ci -t dexafix .
docker run -p 8080:8080 -v dexafix-data:/config DexaFix
```

The process runs as uid 568; a bind-mounted data directory must be writable by it
(`chown 568:568 ./config`).

The image is built `FROM scratch`: the binary embeds its time zones and CA roots, and there is no
shell, so `docker exec ... sh` does not work. Run one-off commands as a new container instead, for
example `docker run --rm -v dexafix-data:/config dexafix db migrate status`.

### Required configuration

- Secret `CODECOV_TOKEN` (from codecov.io after adding the repository). Without it uploads fail
  softly and CI still passes.
- Settings, Actions, General: workflow permissions must allow the release workflow to request
  `contents: write` (the workflow sets it itself).
- Branch protection on `main`: require the checks Lint, Test (sqlite), Test (postgres),
  govulncheck and the Build jobs.
