# Documentation

The [README](../README.md) gives an overview. These pages go into more detail,
in reading order:

1. [Getting started](01-getting-started.md): run the app from source.
2. [Configuration](02-configuration.md): flags, environment variables, the settings on the Admin page and how secrets are passed.
3. [Running behind a reverse proxy](03-reverse-proxy.md): trusted proxies, the base URL, the PROXY protocol, and HTTPS with HTTP/2 and HTTP/3.
4. [Database](04-database.md): SQLite and Postgres, schema migrations and moving between them.
5. [Commands](05-commands.md): the `db` commands for migrations, copying and rekeying, and recovery commands for accounts and settings.
6. [Using the app](06-using-the-app.md): the pages of the web interface.
7. [Accounts and sign-in](07-accounts-and-sign-in.md): sign-in, password reset, two-step verification, passkeys, providers, the audit log and your data.
8. [Encryption](08-encryption.md): what is encrypted, how the key is kept and how to rotate it.
9. [Estimates](09-estimates.md): how the daily estimates are calculated and cached.
10. [Withings import](10-withings-import.md): linking a Withings account and importing readings.
11. [JSON API](11-api.md): routes, authentication and the import endpoint.
12. [CI and releases](12-ci-and-releases.md): what CI checks and how releases are published.
13. [Development](13-development.md): tests, project layout, schema changes and adding an importer.
14. [Limitations and security](14-limitations-and-security.md): read before you expose the app.
15. [Split deployment](15-split-deployment.md): running the frontend (`serve web`) and the backend (`serve api`) as two processes, with the service secret and troubleshooting.
16. [Workers](16-workers.md): the processes that talk to Withings and Strength Level, the worker secret and the Workers page of the Admin section.
17. [Body service](17-body-service.md): the readings, scans and estimates as a process of their own, with a database, key and secret of their own.

Other pages:

- [Key rotation](key-rotation.md): design note on the key check and why rotation stays manual.
- [CI details](ci.md): the README badges, the secrets to configure and how to cut a release.
