#!/bin/sh
# A subshell avoids changing the official entrypoint's shell options when sourced.
(
set -eu
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=ON_ERROR_STOP=1 \
 --set=db_name="$POSTGRES_DB" --set=app_password="$PG_APP_PASSWORD" --set=migrator_password="$PG_MIGRATOR_PASSWORD" <<'SQL'
CREATE ROLE subscriptions_app LOGIN PASSWORD :'app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE subscriptions_migrator LOGIN PASSWORD :'migrator_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db_name" TO subscriptions_app, subscriptions_migrator;
GRANT USAGE, CREATE ON SCHEMA public TO subscriptions_migrator;
SQL
)
