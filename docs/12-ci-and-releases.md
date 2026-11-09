# CI and releases

GitHub Actions runs lint (gofmt, `go vet`, `go mod tidy`), the tests with the
race detector and coverage on SQLite and on Postgres, `govulncheck`, and a
cross-compile for linux/amd64, linux/arm64, darwin/arm64 and windows/amd64. It
runs on pushes to `main`, on pull requests, when started by hand, and weekly on
Monday. Coverage is reported to Codecov. Pushing a tag such as `v1.0.0` publishes
a GitHub release (archives, checksums, a cosign signature, SBOMs, build
provenance, a grouped changelog) and a multi-arch container image at
`ghcr.io/trueforge-org/dexa-fix`. See [ci.md](ci.md) for the details,
the secrets to configure and how to cut a release.

```bash
docker run -p 8080:8080 -v dexa-fix-data:/data ghcr.io/trueforge-org/dexa-fix:latest
```
