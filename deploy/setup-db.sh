#!/bin/sh
# Creates the ledger database and its two roles, applies the schema and the
# application grants. Safe to re-run: existing roles, database and schema are
# left alone (grants are re-applied).
#
#   ledger_owner  NOLOGIN; owns every table, trigger and function
#   ledger_app    LOGIN; the service connects as this role. It can read and
#                 append, but cannot alter tables or disable triggers.
#
# Run as root on the database host (uses the local `postgres` superuser):
#   sh setup-db.sh
#
# Environment (all optional):
#   DB_NAME=ledger  OWNER_ROLE=ledger_owner  APP_ROLE=ledger_app
#   APP_PASSWORD=...      default: generated
#   SQL_DIR=...           default: /usr/local/share/ledger (or ../db next to this script)
#   ENV_FILE=...          default: /etc/ledger/ledger.env; DATABASE_URL is written there
#   DB_HOST=127.0.0.1 DB_PORT=5432   used in the written DATABASE_URL
#   PSQL="runuser -u postgres -- psql"   how to reach PostgreSQL as a superuser
set -eu

DB_NAME="${DB_NAME:-ledger}"
OWNER_ROLE="${OWNER_ROLE:-ledger_owner}"
APP_ROLE="${APP_ROLE:-ledger_app}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-5432}"
ENV_FILE="${ENV_FILE:-/etc/ledger/ledger.env}"
PSQL="${PSQL:-runuser -u postgres -- psql}"

for name in "$DB_NAME" "$OWNER_ROLE" "$APP_ROLE"; do
	case "$name" in
	'' | [!a-z_]* | *[!a-z0-9_]*) echo "invalid name '$name': use lowercase letters, digits and _" >&2; exit 1 ;;
	esac
done

here=$(cd "$(dirname "$0")" && pwd)
if [ -z "${SQL_DIR:-}" ]; then
	if [ -f /usr/local/share/ledger/schema.sql ]; then SQL_DIR=/usr/local/share/ledger; else SQL_DIR="$here/../db"; fi
fi
[ -f "$SQL_DIR/schema.sql" ] && [ -f "$SQL_DIR/grants.sql" ] || { echo "schema.sql/grants.sql not found in $SQL_DIR" >&2; exit 1; }

generated=""
if [ -z "${APP_PASSWORD:-}" ]; then
	# URL- and shell-safe random password.
	APP_PASSWORD=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
	generated=1
fi

psql_su() { $PSQL -v ON_ERROR_STOP=1 -X -q "$@"; }

echo "==> roles"
role_exists=$(echo "SELECT 1 FROM pg_roles WHERE rolname = '$APP_ROLE'" | psql_su -tA -d postgres)
psql_su -d postgres -v owner="$OWNER_ROLE" -v app="$APP_ROLE" -v pw="$APP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I NOLOGIN', :'owner')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'owner') \gexec
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'app', :'pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app') \gexec
SQL
if [ -n "$role_exists" ] && [ -n "$generated" ]; then
	echo "    $APP_ROLE already existed; its password was NOT changed."
	echo "    Set APP_PASSWORD=... to (re)set it, or keep the DATABASE_URL you already have."
	keep_url=1
elif [ -n "$role_exists" ]; then
	echo "SELECT format('ALTER ROLE %I PASSWORD %L', :'app', :'pw') \gexec" |
		psql_su -d postgres -v app="$APP_ROLE" -v pw="$APP_PASSWORD" >/dev/null
fi

echo "==> database"
psql_su -d postgres -v db="$DB_NAME" -v owner="$OWNER_ROLE" <<'SQL'
SELECT format('CREATE DATABASE %I OWNER %I ENCODING ''UTF8'' TEMPLATE template0', :'db', :'owner')
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'db') \gexec
SQL
psql_su -d "$DB_NAME" -v db="$DB_NAME" -v owner="$OWNER_ROLE" -v app="$APP_ROLE" <<'SQL'
SELECT format('REVOKE ALL ON DATABASE %I FROM PUBLIC', :'db') \gexec
SELECT format('GRANT CONNECT ON DATABASE %I TO %I', :'db', :'app') \gexec
SELECT format('ALTER SCHEMA public OWNER TO %I', :'owner') \gexec
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
SQL

echo "==> schema"
has_schema=$(echo "SELECT to_regclass('public.journal_entries') IS NOT NULL" | psql_su -tA -d "$DB_NAME")
if [ "$has_schema" = "t" ]; then
	echo "    already present; not re-applied"
else
	# Objects are created by (and owned by) the owner role.
	{ echo "SET ROLE $OWNER_ROLE;"; cat "$SQL_DIR/schema.sql"; } | psql_su -d "$DB_NAME"
fi

echo "==> grants for $APP_ROLE"
{ echo "SET ROLE $OWNER_ROLE;"; cat "$SQL_DIR/grants.sql"; } | psql_su -d "$DB_NAME" -v app_role="$APP_ROLE"

url="postgres://$APP_ROLE:$APP_PASSWORD@$DB_HOST:$DB_PORT/$DB_NAME?sslmode=disable"
if [ -z "${keep_url:-}" ]; then
	if [ -f "$ENV_FILE" ]; then
		tmp=$(mktemp "$ENV_FILE.XXXXXX")
		grep -v '^DATABASE_URL=' "$ENV_FILE" >"$tmp" || true
		printf 'DATABASE_URL=%s\n' "$url" >>"$tmp"
		chmod --reference="$ENV_FILE" "$tmp" 2>/dev/null || chmod 0640 "$tmp"
		chown --reference="$ENV_FILE" "$tmp" 2>/dev/null || true
		mv "$tmp" "$ENV_FILE"
		echo "==> DATABASE_URL written to $ENV_FILE"
	else
		echo "==> add this to your environment file:"
		echo "DATABASE_URL=$url"
	fi
fi
echo "done."
