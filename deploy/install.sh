#!/bin/sh
# Installs (or upgrades) Ledger from an unpacked release directory:
#   sudo sh deploy/install.sh
# Creates the `ledger` system user, installs the binary, SQL files, helper
# scripts and systemd units, and creates /etc/ledger/ledger.env on first
# install. Existing configuration is never overwritten.
#
# Environment: LEDGER_UID (default 990) — fixed so the TrueNAS bind mount
# ownership can be mapped predictably (host uid = 100000 + LEDGER_UID for an
# unprivileged Proxmox container). CSV_DIR (default /app/csv_data).
set -eu

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
src=$(cd "$(dirname "$0")/.." && pwd)
LEDGER_UID="${LEDGER_UID:-990}"
CSV_DIR="${CSV_DIR:-/app/csv_data}"

[ -x "$src/bin/ledger" ] || { echo "$src/bin/ledger not found; run 'make build' or unpack a release" >&2; exit 1; }

echo "==> user"
if ! id ledger >/dev/null 2>&1; then
	groupadd --system --gid "$LEDGER_UID" ledger
	useradd --system --uid "$LEDGER_UID" --gid ledger --home-dir /nonexistent --no-create-home \
		--shell /usr/sbin/nologin --comment "Ledger service" ledger
fi
echo "    ledger uid=$(id -u ledger) gid=$(id -g ledger)"

echo "==> files"
install -m 0755 "$src/bin/ledger" /usr/local/bin/ledger
install -m 0755 "$src/deploy/ledger-backup.sh" /usr/local/bin/ledger-backup
install -m 0755 "$src/deploy/ledger-user.sh" /usr/local/sbin/ledger-user
install -d -m 0755 /usr/local/share/ledger
install -m 0644 "$src/db/schema.sql" "$src/db/grants.sql" /usr/local/share/ledger/
install -m 0755 "$src/deploy/setup-db.sh" /usr/local/share/ledger/setup-db.sh
install -m 0644 "$src/deploy/postgresql-lowmem.conf" /usr/local/share/ledger/

install -d -m 0750 -o root -g ledger /etc/ledger
if [ ! -f /etc/ledger/ledger.env ]; then
	install -m 0640 -o root -g ledger "$src/deploy/ledger.env.example" /etc/ledger/ledger.env
	echo "    created /etc/ledger/ledger.env"
else
	echo "    kept existing /etc/ledger/ledger.env"
fi

if command -v systemctl >/dev/null 2>&1; then
	echo "==> systemd units"
	install -m 0644 "$src/deploy/ledger.service" "$src/deploy/ledger-backup.service" \
		"$src/deploy/ledger-backup.timer" /etc/systemd/system/
	systemctl daemon-reload 2>/dev/null || echo "    (systemd not running; run systemctl daemon-reload later)"
fi

echo "==> CSV directory $CSV_DIR"
if [ -d "$CSV_DIR" ]; then
	if runuser -u ledger -- test -w "$CSV_DIR"; then
		echo "    writable by ledger"
	else
		echo "    WARNING: $CSV_DIR is not writable by uid $(id -u ledger)."
		echo "    For an unprivileged container, on the Proxmox host run:"
		echo "      chown $((100000 + $(id -u ledger))):$((100000 + $(id -g ledger))) <host path of the bind mount>"
	fi
else
	echo "    WARNING: $CSV_DIR does not exist (bind mount not configured yet?)"
fi

cat <<NEXT

Installed. Next steps (first install):
  1. PostgreSQL:   apt install postgresql
                   cp /usr/local/share/ledger/postgresql-lowmem.conf /etc/postgresql/*/main/conf.d/ledger.conf
                   systemctl restart postgresql
  2. Database:     sh /usr/local/share/ledger/setup-db.sh
  3. First user:   ledger-user add <your-name>
  4. Start:        systemctl enable --now ledger ledger-backup.timer
  5. Browse to:    http://<container-ip>:8080/

Upgrade: run this script again from the new release, then: systemctl restart ledger
NEXT
