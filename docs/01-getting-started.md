# Getting started

## Quick start

Requires the Go version in `go.mod`.

```bash
go run . serve
```

Open <http://localhost:8080>, create an account and enter some data. The first
account becomes the administrator. By default the SQLite database is
`dexa-fix.db` in the current directory.

The first run also creates the encryption key file `dexa-fix.key` in the current
directory, unless `DEXAFIX_ENCRYPTION_KEY` is set. Back up the key file too. The
server does not start without it, because the encrypted values cannot be read.
See [Encryption](08-encryption.md) for where to keep it.

To build a binary:

```bash
go build -o dexa-fix .
./dexa-fix serve
```

`.goreleaser.yaml` builds release binaries for linux, darwin and windows on
amd64 and arm64.
