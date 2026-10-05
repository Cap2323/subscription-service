#!/bin/sh
# Set libpq PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSFILE/PGSSLMODE first.
# Run separately for each shard. Use a role that can bypass RLS for full backups.
set -eu
: "${PGDATABASE:?Set PGDATABASE}"
: "${BACKUP_FILE:?Set BACKUP_FILE to a new .dump path}"
if [ -e "$BACKUP_FILE" ]; then echo "Refusing to overwrite backup" >&2; exit 1; fi
umask 077
before=$(psql -X -At -v ON_ERROR_STOP=1 -c "SELECT fingerprint FROM public.routing_guard WHERE singleton AND NOT frozen AND pending_fingerprint=''")
if [ -z "$before" ]; then echo "Topology is not stable; refuse backup" >&2; exit 1; fi
pg_dump --format=custom --no-owner --file="$BACKUP_FILE"
after=$(psql -X -At -v ON_ERROR_STOP=1 -c "SELECT fingerprint FROM public.routing_guard WHERE singleton AND NOT frozen AND pending_fingerprint=''")
if [ "$before" != "$after" ]; then echo "Topology changed during backup; discard this archive" >&2; exit 1; fi
pg_restore --list "$BACKUP_FILE" > /dev/null
printf 'Archive created. A restore rehearsal is still required.
'
