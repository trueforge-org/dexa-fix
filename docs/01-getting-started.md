# Getting started

## Quick start

Requires the Go version in `go.mod`.

```bash
go run . serve
```

Open <http://localhost:8080>, create an account and enter some data. The first
account becomes the administrator. By default the SQLite database is
`dexafix.db` in the current directory.

The first run also creates the encryption key file `dexafix.key` in the current
directory, unless `DEXAFIX_ENCRYPTION_KEY` is set. Back up the key file too. The
server does not start without it, because the encrypted values cannot be read.
See [Encryption](08-encryption.md) for where to keep it.

To build a binary:

```bash
go build -o dexafix .
./dexafix serve
```

The server speaks plain HTTP on the `--listen` port. To serve HTTPS, with HTTP/2
and optionally HTTP/3, turn it on under **HTTPS, HTTP/2 and HTTP/3** on the Admin
page. See [Running behind a reverse proxy](03-reverse-proxy.md#https-http2-and-http3).
Settings such as the base URL, trusted proxies and the e-mail server are also
on the Admin page, not in flags: see [Configuration](02-configuration.md#settings-on-the-admin-page).

`.goreleaser.yaml` builds release binaries for linux, darwin and windows on
amd64 and arm64.
