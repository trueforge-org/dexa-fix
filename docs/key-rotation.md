# Encryption key check and key rotation (design, not implemented)

Status: planned after 1.0.0. This note records the problem, the intended design and the
open questions, so the work can be picked up without redoing the analysis.

## Where we are

Secrets in the database are encrypted with AES-256-GCM under a single 32-byte key (see
`pkg/store/sqlstore/crypto.go`). The key comes from `DEXAFIX_ENCRYPTION_KEY`, or from a file
(`--encryption-key-file`, default `dexa-fix.key` in the working directory) that `serve` creates
with mode 0600 when it is missing.

Encrypted today: user e-mail addresses, integration data (Withings tokens, the Strength Level
login), server settings (the cookie key, the Withings application secret) and the client secrets of
sign-in providers. A stored value looks like `enc:v1:<nonce+ciphertext>`; the associated data ties
a value to its row, so ciphertext copied to another row fails to open. A separate key derived from
the same master key (HKDF) produces the HMAC used to look users up by e-mail (`email_lookup`).

## Problems

1. **A missing or mistyped key is not noticed.** If the key file is not found (wrong working
   directory, a volume that did not mount), `serve` generates a new random key and carries on. New
   secrets are then written under the new key while the old rows stay unreadable. Startup only
   fails if some row happens to be read during start (the e-mail backfill does that when users
   exist), and by then a second key file exists on disk.
2. **There is no way to change the key.** The master key is also the root of the e-mail lookup, so
   a new key invalidates every `email_lookup` value as well as every ciphertext. A leaked or
   suspected-leaked key can only be handled by hand.
3. **The ciphertext does not say which key made it**, so a rotation could not tell old from new
   values, and a half-finished one could not be resumed.

## Part 1: key check (small, do first)

Goal: refuse to start with the wrong key, and refuse to invent a new key when data already depends
on the old one.

- Store a *check value* in `app_settings` under `crypto.check`: the HMAC-SHA256 of a fixed label
  (for example `dexa-fix key check v1`) under a key derived from the master key with HKDF and its
  own `info` string. It reveals nothing about the key and cannot be mistaken for a ciphertext.
- On `Open` with a cipher: if the setting exists and does not match, fail with a message that
  names the likely causes ("the key file is missing, the wrong file, or `DEXAFIX_ENCRYPTION_KEY`
  differs from the one that created this database"). If it does not exist yet (a new database, or
  one created before this check), write it, but only when no `enc:` value exists in the database.
  If encrypted values exist without a check value, verify by opening one of them first.
- `resolveEncryptionKey` (cmd) must not create a key file when the database already contains
  encrypted values. It needs a cheap query for that, for example
  `SELECT 1 FROM app_settings WHERE value LIKE 'enc:%' LIMIT 1` plus the same for
  `user_integrations`. Creating a key stays automatic for a fresh database, so the first run keeps
  working without steps.
- Tests: wrong key refuses; right key passes; fresh database gets a check value; deleting the key
  file next to an existing encrypted database fails instead of creating a new key.

## Part 2: rotation

### Stored format

Add a key id to the prefix: `enc:v2:<kid>:<payload>`, where `kid` is the first 8 hex characters of
`SHA-256(master key)` (enough to tell keys apart; it is not secret and reveals nothing useful).
`v1` values stay readable and count as "key unknown, try the current key". The store holds one
*current* key and any number of *previous* keys, all by id.

### Configuration

- `DEXAFIX_ENCRYPTION_KEY` / the key file stay the current key.
- `DEXAFIX_ENCRYPTION_KEY_PREVIOUS` (comma separated) and `--encryption-key-previous-file`
  (one key per line) supply keys that may still decrypt old values. Like the current key they are
  never accepted as flag values.
- Reads: pick the key by `kid`; for `v1` values try the current key, then each previous one.
- Writes: always the current key.

### The `db rekey` command

`dexa-fix db rekey` re-encrypts everything under the current key. It needs the old key(s) through
the previous-key settings, so the procedure is:

1. Back up the database and the key file.
2. Generate the new key and make it current; move the old key to the previous list.
3. Stop other writers, or accept that rows written meanwhile are already under the new key (they
   are, so this is safe, just not required).
4. Run `dexa-fix db rekey`. In one transaction it
   - opens every encrypted value with whichever key made it and seals it with the current key
     (users' e-mail, integrations, settings, provider secrets),
   - recomputes `email_lookup` for every user from the decrypted address under the new lookup key,
   - writes the new check value.
   Postgres and SQLite both run it as one transaction, so it either completes or leaves the
   database untouched. A dry run (`--dry-run`) reports how many values would change and fails on
   any value no key can open.
5. Remove the old key from the configuration once the command reports zero values under old keys
   (`db rekey --verify` re-reads everything and exits non-zero otherwise).

Because the e-mail lookup key is derived from the master key, rotation rewrites `email_lookup`.
Until `db rekey` has run, sign-in by e-mail would not find users, so the command must be run
before users are let back in. `serve` should refuse to start (not warn) when the current key
differs from the one that wrote the check value and no previous key accounts for it, which is
Part 1 doing its job.

### Cookie and session keys

The cookie key (`auth.cookie_key` in `app_settings`) is stored encrypted and not derived from the
master key, so `rekey` re-encrypts it but does not change it. Session tokens are stored as hashes
and are unaffected.

## Open questions

- Should `db rekey` be allowed while `serve` runs? The transaction makes it safe for the data, but
  the running process keeps its old cipher in memory. Simplest: document "stop the server".
- Do we want to rotate to a key from a KMS later? If so, the key id and the previous-key list are
  already the right shape; only the source of keys changes.
- `db copy` copies ciphertext as it is. Copying to a database that will use a different key needs
  `rekey` afterwards, or a `--rekey` option on copy that decrypts and re-encrypts while copying.

## Test plan

- Unit: seal/open with `v1` and `v2` values; open with a previous key; unknown `kid` fails with a
  clear error.
- Store: `rekey` on a database filled with every kind of encrypted value, then reading back through
  a store that only knows the new key; lookup by e-mail works after; an interrupted run (cancel the
  context mid-way) leaves the old state intact; dry run changes nothing.
- cmd: the key file is never created when the database already has encrypted values; rotation
  procedure end to end on a temp SQLite file.
- Postgres: the same rekey test under `DEXAFIX_TEST_POSTGRES_DSN` in CI.
