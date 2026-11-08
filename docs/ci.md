# CI and releases

## Badges (paste into README.md)

```markdown
[![CI](https://github.com/trueforge-org/dexa-fix/actions/workflows/ci.yaml/badge.svg)](https://github.com/trueforge-org/dexa-fix/actions/workflows/ci.yaml)
[![codecov](https://codecov.io/gh/trueforge-org/dexa-fix/graph/badge.svg)](https://codecov.io/gh/trueforge-org/dexa-fix)
[![Go Report Card](https://goreportcard.com/badge/github.com/trueforge-org/dexa-fix)](https://goreportcard.com/report/github.com/trueforge-org/dexa-fix)
[![Latest release](https://img.shields.io/github/v/release/trueforge-org/dexa-fix)](https://github.com/trueforge-org/dexa-fix/releases/latest)
[![License](https://img.shields.io/github/license/trueforge-org/dexa-fix)](LICENSE)
```

## CI and releases

`.github/workflows/ci.yaml` runs on pushes to `main`, pull requests, manually and weekly (Monday):

- **Lint**: gofmt, `go vet`, and a `go mod tidy` check.
- **Test (sqlite)** and **Test (postgres)**: `go test -race` with coverage. The Postgres run uses a
  service container and `DEXAFIX_TEST_POSTGRES_DSN`, so the Postgres migrations are exercised.
  Coverage goes to Codecov with the flags `unit` and `postgres`.
- **govulncheck**: known vulnerabilities in dependencies and the standard library.
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
- a multi-arch container image `ghcr.io/trueforge-org/dexa-fix` tagged `X.Y.Z`, `X.Y` and `latest`
  (pre-releases only get the full version tag), keyless-signed with cosign (the images and the
  manifest), on a base image pinned by digest;
- build provenance attestations for the release artifacts.

Run the Release workflow manually (workflow_dispatch) for a dry run that builds a snapshot only.

Every third-party action is pinned to a commit SHA with its version in a comment, because the
release job holds write and signing permissions. Dependabot (github-actions and docker) proposes
the updates. The GoReleaser version in `release.yaml` is pinned too and is bumped by hand.

### Container

```sh
docker run -p 8080:8080 -v dexa-fix-data:/data ghcr.io/trueforge-org/dexa-fix:latest
```

The process runs as uid 65532; a bind-mounted data directory must be writable by it
(`chown 65532:65532 ./data`).

### Required configuration

- Secret `CODECOV_TOKEN` (from codecov.io after adding the repository). Without it uploads fail
  softly and CI still passes.
- Settings, Actions, General: workflow permissions must allow the release workflow to request
  `contents: write`/`packages: write` (the workflow sets them itself).
- After the first image push, make the ghcr.io package public and link it to the repository.
- Branch protection on `main`: require the checks Lint, Test (sqlite), Test (postgres),
  govulncheck and the Build jobs.
