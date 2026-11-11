# Limitations and security

- **Rate limiting is basic.** Failed password sign-ins are limited per login
  name and per address. The counts are kept in the database, so they survive a
  restart and are shared by all backend processes on it. On a public
  deployment, limit sign-in attempts at the reverse proxy too. See
  [Storage and rate limiting](07-accounts-and-sign-in.md#storage-and-rate-limiting).
- **Height and names are not encrypted.** Protect the database
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
- **Estimates are cached in memory** by the body service, per user, for the 32
  most recently used users, and recomputed when the data, the settings that
  matter or the date change. See [Estimate cache](09-estimates.md#estimate-cache).
- **The internal link carries sessions, passwords and the TLS private key.** In
  a [split deployment](15-split-deployment.md) the web process calls the api
  process over it. The service secret says which web process may call, but it
  does not encrypt the link. Keep the link on a private network: a Docker or
  Compose internal network, or the Kubernetes cluster network. Across hosts, use
  TLS through a service mesh or a tunnel. DexaFix warns at start when the
  backend URL is plain HTTP to an address that is not private. The api process
  never trusts `X-Dexafix-*` headers on the public listener. Personal access
  tokens and Basic authentication work only on the public API. Sealed cookies
  and keys stay in the api process; the single-use nonces are in the database.
- **Mutual TLS is available for the internal link, and recommended across
  hosts.** It encrypts the link and requires a client certificate in addition to
  the service secret. See [Mutual TLS](15-split-deployment.md#mutual-tls-between-web-and-api).
- **State is in the database, not in the process.** Failed-attempt lock-outs,
  single-use nonces (a sealed sign-in value can be spent once, on any backend)
  and the status of Strength Level syncs are stored in the database, so several
  backend processes on one database agree. A sync itself runs on the backend
  that started it; if that backend dies, the status is reported as interrupted
  after 2 minutes. What stays in memory is only a cache (the estimates) and the
  de-duplication of noisy audit events. See
  [Running more api processes](15-split-deployment.md#running-more-api-processes).
- **Load budgets are per listener.** The public API admits 32 requests at once,
  queues 64 and allows 20 requests per second per client. The internal listener
  admits 128 at once, queues 256 and allows 50 per second per client. Excess
  requests get 503 or 429 with `Retry-After`. The budgets keep a flood on the
  public API from starving the internal one. See
  [Split deployment](15-split-deployment.md#running-more-api-processes).
- **Outgoing connections** of the api process go only to the sign-in providers
  and the mail server an administrator configured, and are checked when they are
  made, on the address the name resolved to. Cloud metadata services and other
  link-local addresses (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`,
  `100.100.100.200`) are always refused, and so are redirects to them. Private
  networks (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`,
  `fc00::/7`) are refused unless an administrator turns on the setting for that
  kind of connection: **Allow sign-in providers on private network addresses** or
  **Allow the mail server on a private network address**. The two are separate,
  and turning one on does not open the other. A server on the same machine
  (`localhost`) is always allowed. A failed connection is reported to
  the administrator without what the other end answered; the details are in the
  server log. If the api process uses an HTTP proxy from `HTTPS_PROXY`, the check
  applies to the proxy, not to the address behind it. Everything else that talks
  to the outside is a [worker](16-workers.md).
- **Sensitive changes need a recent sign-in when there is no password.** A user
  who signs in with a provider or a passkey only, or any user while password
  sign-in is off, must have signed in within the last 10 minutes to make a
  personal access token, link another sign-in method or change the e-mail
  address. Users with a password confirm it instead.
- **Wrong passwords and codes are counted before they are checked,** per account
  (login name, second-step code, password confirmation), so guesses sent in
  parallel cannot all pass the limit. A correct answer clears the count. The
  per-address count only counts failures, so many people behind one proxy are not
  charged for right answers. Password checks (sign-in, confirmation and Basic
  credentials) share a few slots; when all are busy a request waits a moment and
  then gets a "busy" answer without counting as a failed attempt.
