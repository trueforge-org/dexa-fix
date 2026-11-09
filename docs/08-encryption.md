# Encryption

These values are encrypted with AES-256-GCM before they are stored:

- each user's Withings tokens and Strength Level login
- sign-in provider secrets, including the Apple key
- Withings application credentials and every other server setting on the Admin
  page, including the SMTP password and the private key of an uploaded HTTPS
  certificate
- e-mail addresses
- each user's two-step verification secret, the key of their authenticator app

Each value is tied to its row, so copying it to another row makes it unreadable.
Each e-mail address also gets a keyed hash, derived from the encryption key. The
hash makes addresses searchable and unique without decrypting them. Passwords,
access tokens, password reset links and two-step verification recovery codes are
stored only as hashes. Names are not encrypted, but addresses are. Measurements and height are
not encrypted by the application either. For those, and for names, use disk or
database encryption.

The key comes from one of these, in this order:

1. The environment variable `DEXAFIX_ENCRYPTION_KEY`: 32 bytes as 64
   hexadecimal characters or as base64. There is no command-line flag for it.
2. The key file set with `--encryption-key-file` or
   `DEXAFIX_ENCRYPTION_KEY_FILE`. The default is `dexa-fix.key` in the working
   directory.

If the variable is not set and the key file does not exist, dexa-fix creates
the file with a new random key and mode 0600, but only for a new database or one
that holds no encrypted values yet. If the database already holds encrypted
values, a missing key file is an error and no file is created, so a key file
that did not mount (or a wrong working directory) is reported instead of being
replaced by a key that cannot read anything.

In containers, put the key file on a persistent volume, or set
`DEXAFIX_ENCRYPTION_KEY`. Values stored before encryption are encrypted
automatically at startup.

The database remembers which key it belongs to (a check value in the
`crypto.check` setting, which reveals nothing about the key). Starting with a
different key fails with a message that says so, instead of failing later at a
random value.

For SQLite, `serve` logs a warning when the key file is in the same directory as
the database file, which is the case for the container image defaults
(`/data/dexa-fix.db` and `/data/dexa-fix.key`). A leak of that directory (a
backup, a volume snapshot) then exposes both the encrypted values and the key.
Prefer `DEXAFIX_ENCRYPTION_KEY` from a secret store, or a key file on a separate
secret mount.

Back up the key separately from the database. Without it, the encrypted values
cannot be read, and the server does not start, because a server setting can no
longer be read. Keeping the key next to database backups defeats the purpose.

## Rotating the key

Stored values look like `enc:v2:<key id>:<data>`. The key id is the first eight
hexadecimal characters of the SHA-256 of the key, so values written by different
keys can be told apart. Values from before (`enc:v1:`) stay readable. To change
the key:

1. Stop the server and back up the database and the key file.
2. Generate a new key, for example `openssl rand -hex 32`.
3. Configure the new key as the current one and the old key as a previous one:
   `DEXAFIX_ENCRYPTION_KEY=<new>` and `DEXAFIX_ENCRYPTION_KEY_PREVIOUS=<old>`, or
   a new `--encryption-key-file` plus `--encryption-key-previous-file` with the
   old key on a line of its own. Several previous keys are allowed.
4. Run `dexa-fix db rekey --dry-run` to see how many values would change. It
   fails if any value cannot be opened by one of the keys.
5. Run `dexa-fix db rekey`. In a single transaction it re-encrypts the e-mail
   addresses, integrations, server settings (including the cookie key),
   sign-in provider secrets and two-step verification secrets with the current
   key, recomputes the e-mail lookup hashes (they derive from the key) and
   stores the new check value. It prints a count for each kind, such as
   `two-step secrets re-encrypted: 2`. It either completes or changes nothing,
   and can be repeated: values already under the current key are skipped.
6. Start the server with only the new key. Remove the old key from the
   configuration once `db rekey` reports no values left to re-encrypt.

The server also starts between steps 3 and 5. It reads old values with the
previous key, writes new ones with the current key, and logs a warning until
`db rekey` has run. Run `db rekey` while the server is stopped: a running server
keeps its keys in memory. The cookie key itself does not change, so sessions
stay valid. `db rekey` was tested on SQLite; on Postgres it is the same code in
one transaction.

Rotation is manual on purpose: the key belongs to the operator, `db rekey` is
offline and the AES-GCM usage limits are far out of reach (see
[key-rotation.md](key-rotation.md)). Rotate when the key may have
leaked, when someone with access leaves, or when your policy asks for it. The
Admin page shows the key id and since when it is in use (at least). To get a
reminder there, set **Remind me to rotate the encryption key after (days)** under
**Network and retention** on the Admin page (default empty, no reminder). The page also
says when `db rekey` is due after a key change.
