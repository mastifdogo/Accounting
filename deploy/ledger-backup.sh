#!/bin/sh
# Dumps the ledger database to $BACKUP_DIR (default /app/csv_data/backups)
# in pg_dump's custom format, then deletes dumps older than $BACKUP_KEEP_DAYS
# (default 30). Installed as /usr/local/bin/ledger-backup and run nightly by
# ledger-backup.timer with the environment from /etc/ledger/ledger.env.
set -eu
umask 027 # dumps contain all financial data and password hashes

: "${DATABASE_URL:?DATABASE_URL is not set}"
BACKUP_DIR="${BACKUP_DIR:-/app/csv_data/backups}"
BACKUP_KEEP_DAYS="${BACKUP_KEEP_DAYS:-30}"

mkdir -p "$BACKUP_DIR"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
final="$BACKUP_DIR/ledger-$stamp.dump"
tmp="$BACKUP_DIR/.ledger-$stamp.dump.tmp"
trap 'rm -f "$tmp"' EXIT

pg_dump --format=custom --compress=6 --no-owner --no-privileges --file="$tmp" "$DATABASE_URL"
# Verify the archive is readable before keeping it.
pg_restore --list "$tmp" >/dev/null
mv "$tmp" "$final"
echo "backup written: $final ($(wc -c <"$final") bytes)"

find "$BACKUP_DIR" -maxdepth 1 -name 'ledger-*.dump' -type f -mtime +"$BACKUP_KEEP_DAYS" -print -delete
