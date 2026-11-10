---
applyTo: ".github/workflows/**,.goreleaser.yaml,.github/goreleaser/**,codecov.yml,.renovaterc.json5"
paths:
  - ".github/workflows/**"
  - ".goreleaser.yaml"
  - ".github/goreleaser/**"
  - "codecov.yml"
  - ".renovaterc.json5"
description: "Rules for CI, release and dependency automation. Do not change them unless the task is about them."
---

# CI and release instructions

Do not change workflows, release configuration or dependency automation unless the task is specifically about them.

- `ci.yaml` runs: format (`gofmt`), `go vet`, `go mod tidy` (no diff), tests with `-race` on SQLite, the **web/backend split run** (`DEXAFIX_TEST_SPLIT=1 go test -race -count=1 ./pkg/backend/... ./pkg/frontend`), tests against PostgreSQL 18, `govulncheck`, a build matrix, and a goreleaser check. A new package or test mode must be reachable from these jobs.
- The release (`release.yaml`, on `v*` tags) runs goreleaser: one binary per platform, archives, checksums, SBOMs and signatures. It **builds and publishes no container image**: do not add `dockers`, `docker_manifests`, `docker_signs`, a registry login or `packages: write` back. The released image is built elsewhere, by ContainerForge (`apps/dexafix` in `trueforge-org/containerforge`) from the GitHub release, as `ghcr.io/trueforge-org/dexafix`; `Dockerfile.ci` exists for CI, tests and automation only. There is deliberately **one binary**; `serve web` and `serve api` are modes of it.
- Pin actions by commit SHA with a version comment, as the existing steps do. Do not widen `permissions:` beyond what a job needs.
- `go.mod` tracks the Go version CI uses; `go mod tidy` must leave no diff.
- Coverage settings are in `codecov.yml`.
