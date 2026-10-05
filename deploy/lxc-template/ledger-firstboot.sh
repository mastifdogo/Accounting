#!/bin/sh
# First boot of a container created from the Ledger LXC template.
# Installed as /usr/local/sbin/ledger-firstboot and run once by
# ledger-firstboot.service, before ledger.service. It gives every container
# its own secrets:
#   - a fresh PostgreSQL cluster
#   - the ledger_owner/ledger_app roles, with a random app password written to
#     /etc/ledger/ledger.env
#   - an `admin` login with a random password in /root/ledger-admin-password
# Safe to re-run: it stops at once if /var/lib/ledger/provisioned exists.
set -eu

STATE=/var/lib/ledger/provisioned
ADMIN_USER="${LEDGER_ADMIN_USER:-admin}"
PW_FILE=/root/ledger-admin-password
CSV_DIR=/app/csv_data

log() { echo "ledger-firstboot: $*"; }

[ -f "$STATE" ] && { log "already provisioned"; exit 0; }

# SSH host keys: Proxmox normally generates them when the container is
# created; cover containers created any other way.
if command -v ssh-keygen >/dev/null 2>&1 && ! ls /etc/ssh/ssh_host_*_key >/dev/null 2>&1; then
	log "generating SSH host keys"
	ssh-keygen -A
fi

# PostgreSQL: one cluster per container (the template ships none).
ver=$(find /usr/lib/postgresql -mindepth 1 -maxdepth 1 -printf '%f\n' | sort -V | tail -n1)
[ -n "$ver" ] || { log "PostgreSQL is not installed"; exit 1; }
if [ ! -d "/etc/postgresql/$ver/main" ]; then
	log "creating PostgreSQL $ver cluster"
	pg_createcluster --locale C.UTF-8 "$ver" main >/dev/null
fi
install -m 0644 /usr/local/share/ledger/postgresql-lowmem.conf "/etc/postgresql/$ver/main/conf.d/ledger.conf"
if [ -d /run/systemd/system ]; then
	systemctl start "postgresql@$ver-main"
else
	pg_ctlcluster "$ver" main start # no systemd (e.g. building/testing in a chroot)
fi
for _ in $(seq 1 30); do
	pg_isready -q && break
	sleep 1
done
pg_isready -q || { log "PostgreSQL did not start"; exit 1; }

log "creating the ledger database"
sh /usr/local/share/ledger/setup-db.sh

if ledger-user list | awk 'NR > 1 { print $1 }' | grep -qx "$ADMIN_USER"; then
	log "user $ADMIN_USER already exists"
else
	log "creating login user $ADMIN_USER"
	pw=$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')
	(umask 077 && printf '%s\n' "$pw" >"$PW_FILE")
	ledger-user add "$ADMIN_USER" --password-stdin <"$PW_FILE"
fi

if ! runuser -u ledger -- sh -c "mkdir -p -m 0750 $CSV_DIR/backups && touch $CSV_DIR/.write-test && rm $CSV_DIR/.write-test" 2>/dev/null; then
	log "WARNING: $CSV_DIR is not writable by the ledger user (UID $(id -u ledger));"
	log "         exports, uploads and backups will fail until it is (see docs/DEPLOYMENT.md, step 3)."
fi

cat >/etc/motd <<MOTD

  Ledger double-entry accounting
  ------------------------------
  Web UI:     http://<this container's IP>:8080/   (hostname -I)
  Login:      $ADMIN_USER / password in $PW_FILE (change it under Settings)
  Users:      ledger-user add|passwd|disable|enable|list
  Logs:       journalctl -u ledger -f
  CSV files:  $CSV_DIR   (bind-mount your TrueNAS share here)

MOTD

mkdir -p "$(dirname "$STATE")"
date -u +%Y-%m-%dT%H:%M:%SZ >"$STATE"
log "done: log in as '$ADMIN_USER' with the password in $PW_FILE"
