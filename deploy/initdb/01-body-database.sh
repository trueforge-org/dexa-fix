#!/bin/sh
# Runs once, when PostgreSQL creates its data directory (the official image runs every script in
# /docker-entrypoint-initdb.d then). It gives the body service a role and a schema of its own, so a
# leak of one cannot read the other:
#
#   dexafix_body   may use the schema "body" and nothing else
#   dexafix        (the api) has no rights in the schema "body"
#
# The scans and readings live in "body", the users and everything else in "public". For a database
# that already exists, run the same statements by hand (docs/17-body-service.md).
set -eu

psql -v ON_ERROR_STOP=1 -v body_password="$(cat /run/secrets/body_db_password)" \
  --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE dexafix_body LOGIN PASSWORD :'body_password';
CREATE SCHEMA body AUTHORIZATION dexafix_body;
ALTER ROLE dexafix_body SET search_path = body;
-- Nobody but the owner of the database (the api's role) may use the schema "public": without this
-- every role may look at its tables' names and create objects there.
REVOKE ALL ON SCHEMA public FROM PUBLIC;
SQL
