#!/bin/sh
# Target MUST be a new, isolated database with the app/migrator roles provisioned.
set -eu
: "${PGDATABASE:?Set an isolated target PGDATABASE}"
: "${BACKUP_FILE:?Set BACKUP_FILE}"
: "${CONFIRM_RESTORE:?Set CONFIRM_RESTORE to the exact target database name}"
if [ "$PGDATABASE" != "$CONFIRM_RESTORE" ]; then echo "Database confirmation mismatch" >&2; exit 1; fi
pg_restore --exit-on-error --no-owner --dbname="$PGDATABASE" "$BACKUP_FILE"
