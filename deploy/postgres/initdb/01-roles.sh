#!/bin/sh
# Runs once, when the database is first created (the image runs every executable script in
# /docker-entrypoint-initdb.d then, as the superuser "postgres" with its password in PGPASSWORD).
# It creates the roles and the database so that the application never runs as a superuser, and so
# that a leak of one service cannot read the other's data:
#
#   postgres       the superuser: for administration only, no service uses it
#   dexafix        the api's role: owns the database "dexafix", has no rights in the schema "body"
#   dexafix_body   the body service's role: owns the schema "body" and can use nothing else
#
# The scans and readings live in "body", the users and everything else in "public". For a database
# that already exists, run the same statements by hand (docs/17-body-service.md).
#
# If this script fails the image stops and starts again over the half-made cluster, and then skips
# initialization: delete the volume (docker compose down -v) and start again.
#
# The passwords are the files deploy init made (docker compose run --rm init). The image's own
# ADDITIONAL_DBS setting only creates databases owned by the superuser and no roles, which is why
# this script does it.
set -eu

# The image's start script knows the superuser as a shell variable but does not export it.
SUPERUSER="${POSTGRES_USER:-postgres}"

psql -v ON_ERROR_STOP=1 \
  -v app_password="$(cat /run/secrets/db_password)" \
  -v body_password="$(cat /run/secrets/body_db_password)" \
  --username "$SUPERUSER" --dbname postgres <<'SQL'
CREATE ROLE dexafix LOGIN PASSWORD :'app_password';
CREATE DATABASE dexafix OWNER dexafix;
CREATE ROLE dexafix_body LOGIN PASSWORD :'body_password';
SQL

psql -v ON_ERROR_STOP=1 --username "$SUPERUSER" --dbname dexafix <<'SQL'
CREATE SCHEMA body AUTHORIZATION dexafix_body;
ALTER ROLE dexafix_body SET search_path = body;
-- Nobody but the owner of the database (the api's role) may use the schema "public": without this
-- every role may look at its tables' names and create objects there.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
SQL
