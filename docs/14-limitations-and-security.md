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
- **Behind a proxy**, set **Trusted proxies** on the Admin page, or everyone
  shares the proxy's address and the forwarding headers are ignored. Trust only
  ranges that nothing else can send from. See
  [Running behind a reverse proxy](03-reverse-proxy.md).
- **Self-signed HTTPS certificates** are trusted by no browser or app until
  someone adds them, and browsers use HTTP/3 only with a trusted certificate.
  Use an uploaded certificate for public access. The private key of an uploaded
  or generated certificate is stored encrypted. There is no redirect from HTTP
  to HTTPS yet. See [HTTPS, HTTP/2 and HTTP/3](03-reverse-proxy.md#https-http2-and-http3).
- **Password reset links are bearer credentials.** Anyone who reads the mailbox,
  or a reset link shown on the Admin page, can set a new password. Use STARTTLS
  or TLS for the SMTP server; the `none` option is only for a server on
  localhost. See [Password reset](07-accounts-and-sign-in.md#password-reset).
- **Estimates are cached in memory** per user, for the 32 most recently used
  users, and recomputed when the data, the settings that matter or the date
  change. See [Estimate cache](09-estimates.md#estimate-cache).
