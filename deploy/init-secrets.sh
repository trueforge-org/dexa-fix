#!/bin/sh
# Creates the secrets docker-compose.yaml needs, in ./secrets next to this script. It never
# overwrites a file that exists: losing the encryption key makes the stored secrets unreadable.
set -eu
cd "$(dirname "$0")"

IMAGE="${DEXAFIX_IMAGE:-ghcr.io/trueforge-org/dexa-fix}:${DEXAFIX_VERSION:-latest}"

random_hex() { # 64 hex characters = 256 bits
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

umask 077
mkdir -p secrets
chmod 700 secrets

create() { # create NAME COMMAND...
  name="$1"; shift
  if [ -e "secrets/$name" ]; then
    echo "kept    secrets/$name (exists)"
    return
  fi
  "$@" > "secrets/$name"
  # The containers run as an unprivileged user (65532) and a mounted file keeps the host's
  # permissions, so it must be readable by others. The directory above stays private to you.
  chmod 444 "secrets/$name"
  echo "created secrets/$name"
}

create db_password random_hex
# The password is hex, so it is safe inside the URL.
create dsn sh -c 'printf "postgres://dexafix:%s@db:5432/dexafix?sslmode=disable" "$(cat secrets/db_password)"'
create encryption_key random_hex
create service_secret random_hex
create worker_secret random_hex # differs from service_secret: they open different doors

echo
echo "Back up secrets/encryption_key together with the database."
echo "Next: docker compose up -d   (image: $IMAGE)"
