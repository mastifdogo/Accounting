#!/usr/bin/env bash
# Deploys Ledger into a new Proxmox LXC container, or upgrades an existing one.
# Run on the Proxmox host as root, from an unpacked release (or a repo after
# `make build runtime`):
#
#   ./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger
#   ./deploy/proxmox-deploy.sh --upgrade --ctid 120
#
# A fresh deployment:
#   1. downloads the latest Debian 12 template (if needed)
#   2. creates an unprivileged container (nesting=1) with the CSV directory
#      bind-mounted at /app/csv_data, and maps its ownership to the service UID
#   3. installs PostgreSQL, Ledger, the systemd units and nightly backups
#   4. creates the database (owner + restricted app role) and an admin user
#   5. starts everything and checks /api/v1/health
#
# Upgrade mode takes a database backup, installs the new release over the old
# one (configuration is kept) and restarts the service.
#
# Run with --help for all options. --dry-run prints the commands without
# changing anything.
set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
CTID=""
HOSTNAME_="ledger"
CORES=1
MEMORY=512
SWAP=256
DISK=4
STORAGE="local-lvm"
TEMPLATE_STORAGE="local"
TEMPLATE=""
BRIDGE="vmbr0"
IP="dhcp"
GATEWAY=""
NAMESERVER=""
VLAN=""
CSV_PATH=""
CHOWN_CSV=1
UNPRIVILEGED=1
ONBOOT=1
SSH_KEYS=""
PORT=8080
ADMIN_USER="admin"
ADMIN_PASSWORD=""
RELEASE=""
UPGRADE=0
DRY_RUN=0
ASSUME_YES=0

LEDGER_UID=990 # must match deploy/install.sh
CT_CSV_DIR=/app/csv_data
CT_RELEASE_DIR=/opt/ledger-release

usage() {
	cat <<EOF
Usage:
  $0 --csv-path HOST_DIR [options]          create and deploy a new container
  $0 --upgrade --ctid ID [--release FILE]   upgrade an existing container

Container:
  --ctid ID              container ID (default: next free ID)
  --hostname NAME        hostname (default: $HOSTNAME_)
  --cores N              CPU cores (default: $CORES)
  --memory MB            RAM in MB (default: $MEMORY)
  --swap MB              swap in MB (default: $SWAP)
  --disk GB              root disk size in GB (default: $DISK)
  --storage NAME         storage for the root disk (default: $STORAGE)
  --template-storage N   storage holding templates (default: $TEMPLATE_STORAGE)
  --template FILE        template file name (default: latest debian-12-standard)
  --privileged           create a privileged container (not recommended)
  --no-onboot            don't start the container at host boot
  --ssh-keys FILE        authorized_keys file for root inside the container

Network:
  --bridge NAME          bridge (default: $BRIDGE)
  --ip CIDR|dhcp         IPv4 address, e.g. 192.168.1.50/24 (default: dhcp)
  --gateway IP           IPv4 gateway (required with a static --ip)
  --nameserver IP        DNS server (default: inherit from host)
  --vlan TAG             VLAN tag

Ledger:
  --csv-path HOST_DIR    host directory to bind-mount at $CT_CSV_DIR (required);
                         usually a directory on your TrueNAS mount
  --no-chown             don't chown HOST_DIR to the container's service UID
                         (use when TrueNAS maps ownership, e.g. NFS Mapall)
  --port N               HTTP port (default: $PORT)
  --admin-user NAME      first login user (default: $ADMIN_USER)
  --admin-password-file F
                         read the admin password from file F (default: generate one)
  --release FILE         release tarball (default: the release this script is in)

General:
  --upgrade              upgrade an existing container (needs --ctid)
  --dry-run              print commands instead of running them
  -y, --yes              don't ask for confirmation
  -h, --help             show this help
EOF
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2
	exit 1
}

# run CMD...: execute, or print in --dry-run mode.
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '  [dry-run] %s\n' "$*"
	else
		"$@"
	fi
}

# ct CMD...: run a command inside the container.
ct() { run pct exec "$CTID" -- "$@"; }

# ct_sh SCRIPT: run a shell snippet inside the container.
ct_sh() { run pct exec "$CTID" -- sh -c "$1"; }

need_arg() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value"; }

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
	case "$1" in
	--ctid) need_arg "$@"; CTID=$2; shift ;;
	--hostname) need_arg "$@"; HOSTNAME_=$2; shift ;;
	--cores) need_arg "$@"; CORES=$2; shift ;;
	--memory) need_arg "$@"; MEMORY=$2; shift ;;
	--swap) need_arg "$@"; SWAP=$2; shift ;;
	--disk) need_arg "$@"; DISK=$2; shift ;;
	--storage) need_arg "$@"; STORAGE=$2; shift ;;
	--template-storage) need_arg "$@"; TEMPLATE_STORAGE=$2; shift ;;
	--template) need_arg "$@"; TEMPLATE=$2; shift ;;
	--privileged) UNPRIVILEGED=0 ;;
	--no-onboot) ONBOOT=0 ;;
	--ssh-keys) need_arg "$@"; SSH_KEYS=$2; shift ;;
	--bridge) need_arg "$@"; BRIDGE=$2; shift ;;
	--ip) need_arg "$@"; IP=$2; shift ;;
	--gateway) need_arg "$@"; GATEWAY=$2; shift ;;
	--nameserver) need_arg "$@"; NAMESERVER=$2; shift ;;
	--vlan) need_arg "$@"; VLAN=$2; shift ;;
	--csv-path) need_arg "$@"; CSV_PATH=$2; shift ;;
	--no-chown) CHOWN_CSV=0 ;;
	--port) need_arg "$@"; PORT=$2; shift ;;
	--admin-user) need_arg "$@"; ADMIN_USER=$2; shift ;;
	--admin-password-file)
		need_arg "$@"
		[ -r "$2" ] || die "cannot read $2"
		ADMIN_PASSWORD=$(head -n1 "$2")
		shift
		;;
	--release) need_arg "$@"; RELEASE=$2; shift ;;
	--upgrade) UPGRADE=1 ;;
	--dry-run) DRY_RUN=1 ;;
	-y | --yes) ASSUME_YES=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
	shift
done

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || [ "$DRY_RUN" -eq 1 ] || die "run as root on the Proxmox host"
for cmd in pct pveam pvesh tar; do
	command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' not found: run this on a Proxmox VE host"
done

is_int() { case "$1" in '' | *[!0-9]*) return 1 ;; *) return 0 ;; esac; }
for v in CORES MEMORY SWAP DISK PORT; do
	is_int "${!v}" || die "--${v,,} must be a whole number"
done
[ -z "$CTID" ] || is_int "$CTID" || die "--ctid must be a number"
[ -z "$VLAN" ] || is_int "$VLAN" || die "--vlan must be a number"
case "$ADMIN_USER" in
'' | [!a-z0-9]* | *[!a-z0-9._-]*) die "--admin-user: use lowercase letters, digits, '.', '_' or '-'" ;;
esac
if [ -n "$ADMIN_PASSWORD" ] && [ "${#ADMIN_PASSWORD}" -lt 10 ]; then
	die "the admin password must be at least 10 characters"
fi

# Locate the release: an explicit tarball, or the directory this script is in.
here=$(cd "$(dirname "$0")" && pwd)
src_dir=$(cd "$here/.." && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
if [ -n "$RELEASE" ]; then
	[ -f "$RELEASE" ] || die "release file not found: $RELEASE"
	release_tar=$RELEASE
else
	# An unpacked release has bin/ and lib/ next to deploy/; a repository
	# checkout has them in out/ after `make build runtime`.
	if [ -x "$src_dir/bin/ledger" ] && [ -f "$src_dir/lib/ledger/ledger.mjs" ]; then
		tree=$src_dir
	elif [ -x "$src_dir/out/bin/ledger" ] && [ -f "$src_dir/out/lib/ledger/ledger.mjs" ]; then
		tree=$src_dir/out
	else
		die "no Ledger build next to this script; run 'make build runtime' or pass --release FILE"
	fi
	[ -x "$tree/lib/ledger/node" ] || die "no Node.js runtime in $tree/lib/ledger; run 'make runtime' or pass --release FILE"
	for f in db/schema.sql db/grants.sql deploy/install.sh deploy/setup-db.sh; do
		[ -f "$src_dir/$f" ] || die "missing $src_dir/$f"
	done
	stage="$tmp_dir/ledger-release"
	mkdir -p "$stage/db"
	cp -R "$tree/bin" "$tree/lib" "$src_dir/deploy" "$stage/"
	cp "$src_dir/db/schema.sql" "$src_dir/db/grants.sql" "$stage/db/"
	release_tar="$tmp_dir/ledger-release.tar.gz"
	tar -C "$tmp_dir" -czf "$release_tar" ledger-release
fi

# Read the listing once (piping tar into `grep -q` trips pipefail).
listing=$(tar -tzf "$release_tar") || die "cannot read $release_tar"
# Releases have one top-level directory (push_release strips it).
for f in bin/ledger lib/ledger/ledger.mjs lib/ledger/node deploy/install.sh; do
	grep -Eq "^[^/]+/$f\$" <<<"$listing" || die "$release_tar does not look like a Ledger release (no <dir>/$f)"
done

# Architecture sanity check (release names end in linux-<arch>).
host_arch=$(dpkg --print-architecture 2>/dev/null || uname -m)
case "$release_tar" in
*linux-arm64*) [ "$host_arch" = arm64 ] || warn "release is for arm64 but this host is $host_arch" ;;
*linux-amd64*) [ "$host_arch" = amd64 ] || [ "$host_arch" = x86_64 ] || warn "release is for amd64 but this host is $host_arch" ;;
esac

confirm() {
	[ "$ASSUME_YES" -eq 1 ] || [ "$DRY_RUN" -eq 1 ] && return 0
	printf '%s [y/N] ' "$1"
	read -r answer
	case "$answer" in y | Y | yes | YES) ;; *) die "aborted" ;; esac
}

ct_exists() { pct status "$CTID" >/dev/null 2>&1; }

# push_release: copy the release into the container and unpack it.
push_release() {
	log "copying release into container $CTID"
	run pct push "$CTID" "$release_tar" /root/ledger-release.tar.gz
	ct_sh "rm -rf $CT_RELEASE_DIR && mkdir -p $CT_RELEASE_DIR && tar -xzf /root/ledger-release.tar.gz -C $CT_RELEASE_DIR --strip-components=1 && rm /root/ledger-release.tar.gz"
}

wait_healthy() {
	log "checking http://127.0.0.1:$PORT/api/v1/health"
	[ "$DRY_RUN" -eq 1 ] && return 0
	for _ in $(seq 1 30); do
		if pct exec "$CTID" -- curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	pct exec "$CTID" -- journalctl -u ledger -n 30 --no-pager >&2 || true
	die "Ledger did not become healthy; see the log above"
}

ct_ip() {
	[ "$DRY_RUN" -eq 1 ] && { echo "<container-ip>"; return; }
	pct exec "$CTID" -- hostname -I 2>/dev/null | awk '{print $1}'
}

# ---------------------------------------------------------------------------
# Upgrade
# ---------------------------------------------------------------------------
if [ "$UPGRADE" -eq 1 ]; then
	[ -n "$CTID" ] || die "--upgrade needs --ctid"
	ct_exists || die "container $CTID does not exist"
	pct status "$CTID" | grep -q running || die "container $CTID is not running (pct start $CTID)"
	# Keep the port the container is configured with.
	if [ "$DRY_RUN" -eq 0 ]; then
		cur=$(pct exec "$CTID" -- sh -c "sed -n 's/^HTTP_ADDR=.*:\([0-9][0-9]*\)$/\1/p' /etc/ledger/ledger.env" || true)
		[ -n "$cur" ] && PORT=$cur
	fi
	confirm "Upgrade Ledger in container $CTID?"

	log "backing up the database first"
	ct systemctl start ledger-backup ||
		die "pre-upgrade backup failed (pct exec $CTID -- journalctl -u ledger-backup); nothing was changed"
	push_release
	log "installing"
	ct sh "$CT_RELEASE_DIR/deploy/install.sh"
	log "restarting"
	ct systemctl restart ledger
	wait_healthy
	log "upgrade complete: http://$(ct_ip):$PORT/"
	exit 0
fi

# ---------------------------------------------------------------------------
# Fresh deployment
# ---------------------------------------------------------------------------
[ -n "$CSV_PATH" ] || { usage >&2; die "--csv-path is required"; }
case "$CSV_PATH" in /*) ;; *) die "--csv-path must be an absolute host path" ;; esac
if [ "$IP" != dhcp ]; then
	case "$IP" in */*) ;; *) die "--ip must be CIDR (e.g. 192.168.1.50/24) or dhcp" ;; esac
	[ -n "$GATEWAY" ] || die "--gateway is required with a static --ip"
fi
[ -z "$SSH_KEYS" ] || [ -r "$SSH_KEYS" ] || die "cannot read $SSH_KEYS"

if [ -z "$CTID" ]; then
	CTID=$(pvesh get /cluster/nextid)
fi
ct_exists && die "container $CTID already exists (use --upgrade, or pick another --ctid)"

if [ -z "$ADMIN_PASSWORD" ]; then
	ADMIN_PASSWORD=$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')
	generated_password=1
fi

cat <<EOF

Ledger will be deployed with:
  container   $CTID ($HOSTNAME_), $( [ "$UNPRIVILEGED" -eq 1 ] && echo unprivileged || echo privileged ), nesting=1
  resources   $CORES core(s), ${MEMORY} MB RAM, ${SWAP} MB swap, ${DISK} GB on $STORAGE
  network     $BRIDGE, ip=$IP${GATEWAY:+, gw=$GATEWAY}${VLAN:+, vlan $VLAN}
  CSV share   $CSV_PATH (host) -> $CT_CSV_DIR (container)
  web         port $PORT, admin user "$ADMIN_USER"

EOF
confirm "Continue?"

# --- Template ----------------------------------------------------------------
if [ -z "$TEMPLATE" ]; then
	log "finding the latest Debian 12 template"
	run pveam update >/dev/null || warn "pveam update failed; using the cached template list"
	TEMPLATE=$(pveam available --section system 2>/dev/null | awk '{print $2}' |
		grep -E '^debian-12-standard_.*_amd64\.tar\.(zst|gz|xz)$' | sort -V | tail -n1 || true)
	[ -n "$TEMPLATE" ] || die "no debian-12-standard template found (pass --template)"
fi
if pveam list "$TEMPLATE_STORAGE" 2>/dev/null | grep -q "vztmpl/$TEMPLATE"; then
	log "template $TEMPLATE already downloaded"
else
	log "downloading $TEMPLATE to $TEMPLATE_STORAGE"
	run pveam download "$TEMPLATE_STORAGE" "$TEMPLATE"
fi

# --- CSV directory on the host -----------------------------------------------
log "preparing $CSV_PATH"
run mkdir -p "$CSV_PATH"
if [ "$CHOWN_CSV" -eq 1 ]; then
	offset=0
	[ "$UNPRIVILEGED" -eq 1 ] && offset=100000
	host_uid=$((offset + LEDGER_UID))
	if ! run chown "$host_uid:$host_uid" "$CSV_PATH"; then
		warn "could not chown $CSV_PATH to $host_uid (NFS root squash?)."
		warn "Make it writable by host UID $host_uid, or set TrueNAS NFS 'Mapall User', then rerun with --no-chown."
	fi
	run chmod 0750 "$CSV_PATH" || true
fi

# --- Container -----------------------------------------------------------------
net="name=eth0,bridge=$BRIDGE,ip=$IP"
[ -n "$GATEWAY" ] && net="$net,gw=$GATEWAY"
[ -n "$VLAN" ] && net="$net,tag=$VLAN"
create_args=(
	"$CTID" "$TEMPLATE_STORAGE:vztmpl/$TEMPLATE"
	--hostname "$HOSTNAME_"
	--ostype debian
	--unprivileged "$UNPRIVILEGED"
	--features nesting=1
	--cores "$CORES" --memory "$MEMORY" --swap "$SWAP"
	--rootfs "$STORAGE:$DISK"
	--net0 "$net"
	--mp0 "$CSV_PATH,mp=$CT_CSV_DIR"
	--onboot "$ONBOOT"
	--description "Ledger double-entry accounting"
)
[ -n "$NAMESERVER" ] && create_args+=(--nameserver "$NAMESERVER")
[ -n "$SSH_KEYS" ] && create_args+=(--ssh-public-keys "$SSH_KEYS")

log "creating container $CTID"
run pct create "${create_args[@]}"
log "starting container"
run pct start "$CTID"

log "waiting for the network"
if [ "$DRY_RUN" -eq 0 ]; then
	ok=0
	for _ in $(seq 1 60); do
		if pct exec "$CTID" -- sh -c 'ip -4 -o addr show scope global | grep -q inet && getent hosts deb.debian.org >/dev/null' 2>/dev/null; then
			ok=1
			break
		fi
		sleep 2
	done
	[ "$ok" -eq 1 ] || die "container $CTID has no network/DNS after 2 minutes (check --ip/--gateway/--nameserver)"
fi

# --- Packages ------------------------------------------------------------------
log "installing PostgreSQL (this takes a minute)"
ct_sh "export DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8 && apt-get update -q && apt-get install -y -q postgresql curl ca-certificates && apt-get clean"

# --- Ledger --------------------------------------------------------------------
push_release
log "installing Ledger"
ct env CSV_DIR="$CT_CSV_DIR" LEDGER_UID="$LEDGER_UID" sh "$CT_RELEASE_DIR/deploy/install.sh"

log "tuning PostgreSQL for a small container"
ct_sh "for d in /etc/postgresql/*/main/conf.d; do cp /usr/local/share/ledger/postgresql-lowmem.conf \"\$d/ledger.conf\"; done && systemctl restart postgresql"

log "creating the database"
ct sh /usr/local/share/ledger/setup-db.sh

if [ "$PORT" != 8080 ]; then
	log "setting HTTP port $PORT"
	ct sed -i "s/^HTTP_ADDR=.*/HTTP_ADDR=:$PORT/" /etc/ledger/ledger.env
fi

log "creating admin user '$ADMIN_USER'"
pw_file="$tmp_dir/pw"
(umask 077 && printf '%s\n' "$ADMIN_PASSWORD" >"$pw_file")
run pct push "$CTID" "$pw_file" /root/.ledger-admin-pw --perms 0600
ct_sh "ledger-user add '$ADMIN_USER' --password-stdin </root/.ledger-admin-pw; rc=\$?; rm -f /root/.ledger-admin-pw; exit \$rc"

log "starting Ledger and nightly backups"
ct systemctl enable --now ledger ledger-backup.timer
wait_healthy

# Check the share is writable by the service (exports, uploads, backups).
if ! ct runuser -u ledger -- sh -c "touch $CT_CSV_DIR/.write-test && rm $CT_CSV_DIR/.write-test" 2>/dev/null; then
	warn "the service user cannot write to $CT_CSV_DIR: exports, uploads and backups will fail."
	warn "Make $CSV_PATH writable by host UID $((UNPRIVILEGED * 100000 + LEDGER_UID)) (see docs/DEPLOYMENT.md, step 3)."
fi

ip=$(ct_ip)
cat <<EOF

$(printf '\033[1;32m')Ledger is running.$(printf '\033[0m')

  URL:        http://${ip:-<container-ip>}:$PORT/
  Username:   $ADMIN_USER
EOF
if [ -n "${generated_password:-}" ]; then
	echo "  Password:   $ADMIN_PASSWORD   (generated; shown only once — change it under Settings)"
else
	echo "  Password:   (the one from --admin-password-file)"
fi
cat <<EOF

  Container:  pct enter $CTID
  Users:      pct exec $CTID -- ledger-user list
  Logs:       pct exec $CTID -- journalctl -u ledger -f
  Backups:    $CSV_PATH/backups (nightly at 02:30)
  Upgrade:    $0 --upgrade --ctid $CTID --release <new tarball>

For HTTPS, put a reverse proxy in front (docs/DEPLOYMENT.md, step 7).
EOF
