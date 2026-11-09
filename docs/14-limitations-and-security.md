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
- **Passkeys need HTTPS** (or `localhost`) in the browser, and belong to the host
  name the browser used, with the port in the origin. Changing the host name, for
  example moving to a new domain, makes existing passkeys unusable: each user
  adds them again. Passkeys cannot recover an account. A lost device needs another
  sign-in method or an administrator. See [Passkeys](07-accounts-and-sign-in.md#passkeys).
- **Password reset links are bearer credentials.** Anyone who reads the mailbox,
  or a reset link shown on the Admin page, can set a new password. Use STARTTLS
  or TLS for the SMTP server; the `none` option is only for a server on
  localhost. See [Password reset](07-accounts-and-sign-in.md#password-reset).
- **Estimates are cached in memory** per user, for the 32 most recently used
  users, and recomputed when the data, the settings that matter or the date
  change. See [Estimate cache](09-estimates.md#estimate-cache).
- **The internal link carries sessions, passwords and the TLS private key.** In
  a [split deployment](15-split-deployment.md) the web process calls the api
  process over it. The service secret says which web process may call, but it
  does not encrypt the link. Keep the link on a private network: a Docker or
  Compose internal network, or the Kubernetes cluster network. Across hosts, use
  TLS through a service mesh or a tunnel. dexa-fix warns at start when the
  backend URL is plain HTTP to an address that is not private. The api process
  never trusts `X-Dexafix-*` headers on the public listener. Personal access
  tokens and Basic authentication work only on the public API. Sealed cookies,
  keys and single-use nonces stay in the api process.
- **Memory is per api process.** Failed-login throttling, single-use nonces and
  Strength Level sync progress are held in memory by each api process, so
  several api processes do not share them. Limit sign-in attempts at the reverse
  proxy as well.
- **Load budgets are per listener.** The public API admits 32 requests at once,
  queues 64 and allows 20 requests per second per client. The internal listener
  admits 128 at once, queues 256 and allows 50 per second per client. Excess
  requests get 503 or 429 with `Retry-After`. The budgets keep a flood on the
  public API from starving the internal one. See
  [Split deployment](15-split-deployment.md#running-more-api-processes).
