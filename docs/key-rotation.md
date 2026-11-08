# Encryption key check and key rotation

Status: implemented. This note records the problem, the design and the decisions taken while
implementing it. User-facing instructions are in the README, section "Encryption".

## Where we were

Secrets in the database are encrypted with AES-256-GCM (see `pkg/store/sqlstore/crypto.go`). The key
comes from `DEXAFIX_ENCRYPTION_KEY`, or from a file (`--encryption-key-file`, default `dexa-fix.key`
in the working directory) that `serve` creates with mode 0600 when it is missing.

Encrypted: user e-mail addresses, integration data (provider tokens, the Strength Level login),
server settings (the cookie key and other secrets) and the client secrets of sign-in providers. The
associated data ties a value to its row, so ciphertext copied to another row fails to open. A key
derived from the master key (HKDF) produces the HMAC used to look users up by e-mail
(`email_lookup`).

Problems this design solved:

1. A missing key file was replaced by a new key, and startup then failed for any database that had
   run before, leaving the generated file behind.
2. There was no way to change the key. The master key is also the root of the e-mail lookup.
3. The ciphertext did not say which key made it, so a rotation could not tell old from new values
   and a half-finished one could not be resumed.

## Key check

- `app_settings` row `crypto.check` holds the HMAC-SHA256 of the label `dexa-fix key check v1` under
  a key derived from the master key with HKDF (info `dexa-fix key check`), hex encoded. It reveals
  nothing about the key and is not a ciphertext, so encryption and `rekey` skip that row.
- On `Open` with a cipher (not with `WithoutMigrate`), `verifyKey` runs before existing values are
  encrypted:
  - Check value present and equal to the current key's: fine. Equal to a previous key's: start, log
    a warning that `db rekey` is due. Equal to none: fail with a message naming the likely causes
    (missing or wrong key file, a different `DEXAFIX_ENCRYPTION_KEY`).
  - Check value absent: if the database holds an encrypted value, open one of them (a wrong key
    fails with the same message; a value under a previous key starts without writing a check). If it
    holds none (a new database, or one that predates encryption with plaintext values), write the
    check. Plaintext values are then encrypted as before, so nothing changes for those databases.
- `resolveEncryptionKey` (cmd) takes a `hasData` function. When the key file is missing and the
  database holds encrypted values (`sqlstore.HasEncryptedValues`: a `LIKE 'enc:%'` probe on
  settings, integrations, user e-mail and provider secrets; missing tables count as none) it
  returns an error and creates nothing. A new database still gets a key automatically.
- `db copy` deletes the target's own check row and takes the source's with the ciphertext, so the
  check always matches the values it sits next to. The emptiness check of the target ignores it.

## Rotation

### Stored format

`enc:v2:<kid>:<payload>`, with `kid` the first 8 hex characters of `SHA-256(master key)`. The AES
key for v2 values is derived from the master key with HKDF-SHA256 (info `dexa-fix aes-256-gcm v2`),
so the master key is never used directly as a cipher key. `enc:v1:<payload>` (AES under the master
key itself) stays readable: it carries no key id, so the current key is tried first, then each
previous one. New values are always written as v2 under the current key. The cipher holds one
current key and any number of previous keys, looked up by id.

### Configuration

- `DEXAFIX_ENCRYPTION_KEY` / the key file stay the current key.
- `DEXAFIX_ENCRYPTION_KEY_PREVIOUS` (comma separated) and `--encryption-key-previous-file`
  (`DEXAFIX_ENCRYPTION_KEY_PREVIOUS_FILE`; one key per line, blank lines and `#` comments ignored)
  supply keys that may still open old values. Like the current key they are never accepted as flag
  values. Both `serve` and `db rekey` take them.

### `dexa-fix db rekey`

Stop the server, back up the database and key, make the new key current and list the old one as
previous, then run `db rekey`. In one transaction (SQLite and Postgres alike) it

- opens every encrypted value with whichever key made it and seals it with the current key: user
  e-mail, integrations, settings (including the cookie key), provider client secrets. Values that
  are already `v2` under the current key are skipped, so the command is idempotent and a repeat
  after a failure picks up where the database stands,
- recomputes `email_lookup` for every user from the decrypted address under the current lookup key,
- writes the new check value.

Any value that no configured key can open fails the whole run and nothing is changed. `--dry-run`
does the same work without writing and reports the counts per kind (it fails on unreadable values
too). There is no separate `--verify`: a dry run that reports zero values to change is that check.

### E-mail lookup across a rotation

The lookup key derives from the master key, so a rotation changes every `email_lookup`. Decision:
keep it simple and correct. `db rekey` recomputes the lookups in the same transaction as the
re-encryption. A server that starts between the key change and `db rekey` also recomputes them on
startup (the existing e-mail backfill runs on every `Open`), so e-mail sign-in keeps working in that
window too. The cost is that the lookup key is not independent of the rotation; a stable, separate
lookup secret would avoid the rewrite but add a second secret to manage.

### Cookie and session keys

`auth.cookie_key` is stored encrypted and not derived from the master key, so `rekey` re-encrypts it
but does not change it. Sessions stay valid. Session tokens are stored as hashes and are unaffected.

## Open questions

- `db rekey` while `serve` runs: the transaction keeps the data safe, but the running process keeps
  its old cipher in memory. Documented as "stop the server".
- Rotating to a key from a KMS later: the key id and the previous-key list are already the right
  shape; only the source of keys changes.
- `db copy --rekey` (decrypt and re-encrypt while copying) is not implemented; run `db rekey` on the
  target with the source key as a previous key.

## Tests

- `pkg/store/sqlstore/rekey_test.go`: seal/open with v1 and v2, previous keys, unknown key id; wrong
  key refused, right key accepted, check value written for fresh databases and for databases that
  lack one; `HasEncryptedValues`; `Rekey` across all kinds of encrypted values, read back through a
  store that only knows the new key, e-mail lookup afterwards, idempotence, dry run changes nothing,
  a failing value or cancelled context leaves the database untouched.
- `cmd/keys_test.go`: no key file created when the database holds encrypted values; fresh and
  plaintext databases still get one; previous key parsing; the same-directory warning detection;
  `db rekey` end to end on a temporary SQLite file.
- Not covered: Postgres. The rekey code uses only portable bun queries and a transaction, but it has
  not been run against Postgres; add it under `DEXAFIX_TEST_POSTGRES_DSN` in CI.
