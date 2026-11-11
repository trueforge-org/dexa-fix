# Accounts and sign-in

Each user has an account and sees only their own data. The first account
created becomes the administrator. Passwords are hashed with bcrypt. They must
be 8 characters or more, and at most 72 bytes.

Creating an account asks for a username, an e-mail address and a password. The
e-mail address is the account's key, and it is what you sign in with. It is
compared case-insensitively and stored in lowercase. The username is unique too,
also ignoring case: 2 to 32 letters, digits, `.`, `_` or `-`, starting with a
letter or digit. A first and last name can be added on the Settings page. A new
account made by a provider takes its username from the provider's name field
(changed to those characters); when it is taken, `-2`, `-3` and so on is added.
Accounts that existed before usernames had to be unique keep their name; those
that only differed in case got their id added at the upgrade.

**New accounts are closed by default.** Only the very first account on an empty
server can be created without an invitation. After that nobody can create
their own account, with a password or on first sign-in through a provider,
until an administrator opens **New accounts** on the Admin page. Until then an
administrator adds people from the Users list. Existing accounts keep working.
This keeps a server that is reachable from the internet from handing out
accounts by accident.

## Your data

Under **Settings → Your data** every user can download two JSON files, which
serve a person's right of access and data portability (GDPR articles 15 and 20):

- **Settings and personal details**: username, first and last name, e-mail address, role, height,
  settings, permissions, sign-in methods (provider and account id), passkey
  names and dates (never the keys), access token names and prefixes, whether
  two-step verification is on (`two_factor.enabled`; never the secret or
  recovery codes), whether Withings and Strength Level are connected (and the
  Strength Level e-mail address), and the audit events about the account.
- **DEXA scans and scale readings**: everything the user measured, including
  notes.

Credentials are never included: the files say whether a password, token or
integration exists, not what it is. The same files are available to scripts at
`/api/v1/users/{id}/export/profile` and `/api/v1/users/{id}/export/data`.
Every export is itself recorded in the audit log. Deleting a user (Admin page)
removes all their measurements and settings.

## Audit log

Security-relevant events are recorded in the database: sign-ins (successful,
failed and blocked), sign-outs, account creation, profile, password, token and
sign-in method and two-step verification changes, imports, integration connects,
data deletions, exports,
and every administrator action (users, roles, permissions, providers, server
settings). Each event has a time, who did it, whom it concerned, a short detail
and the client's IP address. Details never hold passwords, tokens, e-mail
addresses or login names.

Administrators see the latest 100 events on the Admin page and can read the
whole log at `GET /api/v1/audit` (`limit`, `before_id` and `user_id` query
parameters). Everyone sees the latest events about their own account in
Settings, and at `GET /api/v1/users/{id}/audit`.

Events are deleted after the **Keep the audit log for N days** setting on the
Admin page (365 by default; `0` keeps them forever). When a user
is deleted, the IP addresses in the events about them are cleared and the
events no longer refer to them. Recording an event never fails the request
that caused it; a failed write is logged.

## Sign-in providers

Administrators add providers on the Admin page. There are three types:

- **OpenID Connect**, for example Google, with the issuer
  `https://accounts.google.com`.
- **Sign in with Apple**. Enter the Services ID as the client ID, then the team
  ID, the key ID, and the contents of the `.p8` key file as the secret. DexaFix
  generates Apple's short-lived client secret from these. Apple requires HTTPS.
- **OAuth 2.0**, with the authorisation, token and userinfo URLs.

Register this callback URL with the provider:

```text
https://your-host/auth/<short name>/callback
```

The Admin page shows the callback URL for each provider. Replace `<short name>`
with the provider's short name.

DexaFix connects to a provider from the api process. A provider on a private
network address (Authentik or Keycloak on your own LAN, say) is refused until
you turn on **Allow sign-in providers and the mail server on private network
addresses** under **Network and retention**. Cloud metadata and other
link-local addresses are always refused, and so are redirects to them. When a
provider cannot be reached, the Admin page says so and, for a refused address,
how to allow it; what the provider answered is only in the server log. See
[Outgoing connections](14-limitations-and-security.md#outgoing-connections).

Behind a reverse proxy, forward the `Host` header and set `X-Forwarded-Proto`.
The callback URL is built from them.

Users link a provider in Settings. With **Create accounts on first sign-in**
turned on, a login from an unknown account creates a new account. Accounts are
matched only by the provider's subject, never by e-mail. A new account takes
the provider's e-mail address on first sign-in, unless the provider marks it
unverified. If that address is already in use, sign-in is refused. The person
must then sign in to the existing account and link the provider in Settings.
Removing a provider also removes its links.

## Password sign-in

Password sign-in can be turned off on the Admin page. Providers and access
tokens keep working. The administrator who turns it off must have another way
to sign in first: a linked provider or a passkey.

To recover from a lockout, start the server with password sign-in forced on:

```bash
go run . serve --force-password-login
```

The same is set with `DEXAFIX_FORCE_PASSWORD_LOGIN=true`.

Users who forget their password can get a reset link. See
[Password reset](#password-reset).

## Password reset

There are two ways to set a new password without the old one. Both make a link
that works once.

**An administrator** opens the Users list on the Admin page and chooses **Reset
link** for the user. The link is shown once on the Admin page and is valid for
24 hours. Tick **E-mail it** to send it by e-mail instead, when e-mail is set
up.

**The user** can ask for the link when an SMTP server and a base URL are set on
the Admin page, under **E-mail** and **Network and retention**, and password
sign-in is on. The sign-in page then shows **Forgot your password?**. The user
enters their e-mail address, and the page always gives the same answer, whether
or not an account with a password uses that address. If one does, a link valid
for one hour is mailed in the background. The link is built from the base URL
only, never from the Host header. Accounts that sign in only through a provider
get no e-mail. At most 3 e-mails are sent per address and 10 per client address
in 15 minutes.

Using a link:

- The user chooses a new password of 8 to 72 bytes.
- With two-step verification on, the user also enters a code from the app, or a
  recovery code. A reset never bypasses two-step verification.
- The user is signed out everywhere: all sessions and access tokens are revoked.
- The link stops working once the password or the e-mail address changes.
- A user covered by the administrator's two-step policy who has no second step
  is sent to set one up after the reset.

The audit log records `password_reset_link_created`, `password_reset_requested`,
`password_reset_email_failed`, `password_reset_completed` and
`password_reset_invalid`. The e-mail settings have no flags or environment
variables; see [Settings on the Admin page](02-configuration.md#settings-on-the-admin-page).

## Two-step verification

Two-step verification adds a one-time code from an authenticator app to signing
in with a password. It protects password sign-in only. Providers (OpenID
Connect, Sign in with Apple and OAuth 2.0) do not ask for a second step, because
the provider is trusted to apply its own. Enforce a second factor at the
provider. Passkey sign-in does not ask for one either; see [Passkeys](#passkeys).

### Turning it on

Only accounts that sign in with a password can turn it on. Under **Settings →
Two-step verification**, confirm your current password and scan the QR code
with an authenticator app. You can type the key in instead. Standard apps work:
codes have 6 digits, change every 30 seconds and use SHA-1. Enter a code from
the app to confirm.

Ten recovery codes are then shown, once. Each has the form
`xxxx-xxxx-xxxx-xxxx` and works one time, in place of an app code. Store them
somewhere safe. To get new ones, confirm your password and a code in Settings;
the old codes stop working.

Turning it off needs your password and a code, or a recovery code. It is not
possible while an administrator requires two-step verification.

### Signing in

After the password, the sign-in page asks for the code on a second page. Enter
the 6-digit code, or a recovery code, in the same field. The password check is
kept in a signed cookie for 5 minutes. The cookie is not a session.

A code works once: a code that was already used is refused, even within its
30 seconds. Five wrong codes for an account within 15 minutes block that
account's second step. The per-address limit for failed sign-ins applies too.
A successful second step clears the count.

### Administrators

On the Admin page, **Two-step verification** chooses who must use it: **Nobody**
(the default), **Administrators** or **Everyone**. People covered by the policy
who have no second step are asked to set one up at their next password sign-in.
They scan the QR code, see their recovery codes once, and are then signed in.

In the Users list, **Remove two-step** turns off a user's two-step verification
and deletes their recovery codes. Use it when someone lost their device and
their recovery codes. If no administrator can sign in to do that, use the
command in [Commands](05-commands.md#account-commands).

An administrator who has two-step verification on is asked for a code again
before any Admin page (`/admin`). The code entered at sign-in counts, so there is
no second prompt straight after signing in. The step-up lasts 15 minutes, kept
in a signed cookie tied to the administrator's account. A recovery code works
too. The JSON API is not affected: it uses access tokens.

The audit log records `admin_verified` for each step-up, and `totp_enabled`,
`totp_disabled`, `totp_removed_by_admin`, `recovery_codes_regenerated`,
`recovery_code_used`, `second_factor_failed` and
`second_factor_throttled`. The last two are rate-limited, like failed sign-ins.

## Passkeys

A passkey (WebAuthn) signs you in without a password. It is a credential kept on
a device or a security key, and it is unlocked with a fingerprint, a face or a
PIN. A passkey replaces the password for that sign-in. It is not a second step
after a password: the device and its unlock already make two factors, so a
passkey sign-in is never asked for a two-step code, and the
[two-step policy](#administrators) does not apply to it.

### Adding and managing

Under **Settings → Passkeys**:

- **Add a passkey**: give it a name. With a password on the account, confirm
  the current password. Without one, the account needs a sign-in from the last
  10 minutes.
- **Rename** a passkey at any time.
- **Remove** a passkey: confirm your password. The last passkey cannot be
  removed while it is the only way to sign in. Add another sign-in method first.

### Signing in with a passkey

On the sign-in page, press **Sign in with a passkey**. Browsers that support
conditional UI also offer your passkey in the suggestions of the e-mail field.
Passkeys are discoverable, so no e-mail address has to be typed: the passkey
itself identifies the account.

### Requirements

- Browsers offer passkeys only on HTTPS, or on `localhost`. The Settings section
  says so when neither applies.
- A passkey belongs to the host name the browser used. When a base URL is set,
  that is its host name. The origin includes the port. If the host name changes,
  for example when you move to a new domain, existing passkeys stop working. Add
  them again.
- With the HTTPS listener (see [HTTPS, HTTP/2 and HTTP/3](03-reverse-proxy.md#https-http2-and-http3)),
  use its HTTPS address. Behind a proxy that terminates TLS, set an `https` base
  URL on the Admin page.

### Other places passkeys count

- A passkey sign-in is recorded with the method `passkey`, which the Users list
  on the Admin page shows in the **Sign-in** column.
- A passkey counts as a sign-in method. It satisfies the rule that you must link
  another sign-in method before you turn off password sign-in on the Admin page,
  and before you unlink your last provider.
- A passkey sign-in counts as the second step for the Admin pages, so no code is
  asked after it.
- Your data export lists passkey names and dates, never the keys. `db copy`
  copies passkeys too.

### Storage and security

- The server stores only the public key and metadata: name, dates and the sign
  counter, as JSON in the `passkeys` table. Public keys are not secret, so they
  are not encrypted. Each user has a random handle that identifies them to their
  passkeys. It says nothing about the person.
- The sign counter must not go backwards. If it does, the credential may have
  been copied: the sign-in is refused and recorded as `passkey_clone_warning`.
  Remove that passkey in Settings if you think it was copied.
- The state of a registration or a sign-in is kept in a signed cookie that works
  once and is valid for 5 minutes. Nothing is stored for visitors who are not
  signed in.
- Failed passkey sign-ins count against the per-address limit (see
  [Storage and rate limiting](#storage-and-rate-limiting)). They are recorded as
  `passkey_login_failed`, rate-limited like other failed sign-ins.

The audit log records `passkey_added`, `passkey_removed`, `passkey_login_failed`
and `passkey_clone_warning`.

Passkeys cannot recover an account. If you lose the device, sign in another way,
with a password or a provider, or ask an administrator. See
[Limitations and security](14-limitations-and-security.md).

## Storage and rate limiting

Some values are encrypted in the database. See [Encryption](08-encryption.md) for
which ones and how to manage the key.

Failed password sign-ins are counted in the database, so every backend process on it
shares the counts: 10 per login name and 60 per
client address in 15 minutes, after which sign-in answers 429. The address is the
visitor's: behind a reverse proxy, set **Trusted proxies** on the Admin page (see
[Running behind a reverse proxy](03-reverse-proxy.md)), or everyone
shares the proxy's address and its count. Limit sign-in attempts at the proxy as
well on a public deployment. Form posts are capped at 1 MiB. Wrong second-step
codes are counted separately, as described in
[Two-step verification](#two-step-verification).
