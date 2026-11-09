# Documentation

The [README](../README.md) gives an overview. These pages go into more detail,
in reading order:

1. [Getting started](01-getting-started.md): run the app from source.
2. [Configuration](02-configuration.md): flags, environment variables and how secrets are passed.
3. [Running behind a reverse proxy](03-reverse-proxy.md): HTTPS, trusted proxies and the base URL.
4. [Database](04-database.md): SQLite and Postgres, schema migrations and moving between them.
5. [Commands](05-commands.md): the `db` commands for migrations, copying and rekeying.
6. [Using the app](06-using-the-app.md): the pages of the web interface.
7. [Accounts and sign-in](07-accounts-and-sign-in.md): sign-in, two-step verification, providers, the audit log and your data.
8. [Encryption](08-encryption.md): what is encrypted, how the key is kept and how to rotate it.
9. [Estimates](09-estimates.md): how the daily estimates are calculated and cached.
10. [Withings import](10-withings-import.md): linking a Withings account and importing readings.
11. [JSON API](11-api.md): routes, authentication and the import endpoint.
12. [CI and releases](12-ci-and-releases.md): what CI checks and how releases are published.
13. [Development](13-development.md): tests, project layout, schema changes and adding an importer.
14. [Limitations and security](14-limitations-and-security.md): read before you expose the app.

Other pages:

- [Key rotation](key-rotation.md): design note on the key check and why rotation stays manual.
- [CI details](ci.md): the README badges, the secrets to configure and how to cut a release.
