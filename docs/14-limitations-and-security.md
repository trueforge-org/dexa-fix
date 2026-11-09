# Limitations and security

- **Rate limiting is basic and in memory.** Failed password sign-ins are limited
  per login name and per address, but the counts reset when the server restarts
  and are not shared between several servers. On a public deployment, limit
  sign-in attempts at the reverse proxy too. See
  [Storage and rate limiting](07-accounts-and-sign-in.md#storage-and-rate-limiting).
- **Measurements, height and names are not encrypted.** Protect the database
  file or Postgres database and its backups with disk or database encryption.
  Losing the encryption key makes the encrypted values unreadable. See
  [Encryption](08-encryption.md).
- **Behind a proxy**, set `--trusted-proxies`, or everyone shares the proxy's
  address and the forwarding headers are ignored. See
  [Running behind a reverse proxy](03-reverse-proxy.md).
- **Estimates are cached in memory** per user, for the 32 most recently used
  users, and recomputed when the data, the settings that matter or the date
  change. See [Estimate cache](09-estimates.md#estimate-cache).
