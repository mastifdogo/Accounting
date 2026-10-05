#!/usr/bin/env bash
# Builds a Proxmox LXC template (rootfs .tar.zst) with Ledger and PostgreSQL
# pre-installed. Upload it to a Proxmox storage under "CT Templates" and create
# containers from it in the GUI or with `pct create`.
#
# Run as root on Debian/Ubuntu (the Proxmox host works too) with mmdebstrap:
#   apt install mmdebstrap zstd
#   make build                                  # or use a release tarball
#   sudo deploy/lxc-template/build-lxc-template.sh
#   sudo deploy/lxc-template/build-lxc-template.sh --release dist/ledger-v1-linux-amd64.tar.gz
#
# The template contains no secrets: SSH host keys, machine-id and the
# PostgreSQL cluster are removed, and each new container creates its own
# database password and admin password on first boot (ledger-firstboot).
set -euo pipefail

SUITE=bookworm
ARCH=amd64
MIRROR=""
OUT_DIR=""
RELEASE=""
VERSION=""

usage() {
	cat <<EOF
Usage: $0 [options]

  --release FILE   Ledger release tarball (default: this checkout's bin/ledger, db/ and deploy/)
  --suite NAME     bookworm (Debian 12, default), trixie (Debian 13), noble (Ubuntu 24.04), jammy (Ubuntu 22.04)
  --arch ARCH      amd64 (default) or arm64; must match the release binary
  --mirror URL     package mirror (default: deb.debian.org / archive.ubuntu.com)
  --version V      version string in the file name (default: from the release or git)
  --out DIR        output directory (default: <repo>/dist)
  -h, --help       show this help
EOF
}

die() {
	echo "ERROR: $*" >&2
	exit 1
}
log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--release) RELEASE=${2:?}; shift ;;
	--suite) SUITE=${2:?}; shift ;;
	--arch) ARCH=${2:?}; shift ;;
	--mirror) MIRROR=${2:?}; shift ;;
	--version) VERSION=${2:?}; shift ;;
	--out) OUT_DIR=${2:?}; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option: $1" ;;
	esac
	shift
done

[ "$(id -u)" -eq 0 ] || die "run as root (mmdebstrap needs it to run the install inside the image)"
command -v mmdebstrap >/dev/null || die "mmdebstrap not found: apt install mmdebstrap zstd"
command -v zstd >/dev/null || die "zstd not found: apt install zstd"

case "$SUITE" in
bookworm) DISTRO=debian; OS_VERSION=12 ;;
trixie) DISTRO=debian; OS_VERSION=13 ;;
noble) DISTRO=ubuntu; OS_VERSION=24.04 ;;
jammy) DISTRO=ubuntu; OS_VERSION=22.04 ;;
*) die "unsupported --suite $SUITE" ;;
esac
case "$ARCH" in amd64 | arm64) ;; *) die "unsupported --arch $ARCH" ;; esac

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
OUT_DIR=${OUT_DIR:-$repo/dist}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- Stage the Ledger release ---------------------------------------------------
stage="$work/release"
mkdir -p "$stage"
if [ -n "$RELEASE" ]; then
	[ -f "$RELEASE" ] || die "release file not found: $RELEASE"
	tar -xzf "$RELEASE" -C "$stage" --strip-components=1
	if [ -z "$VERSION" ]; then
		VERSION=$(basename "$RELEASE" | sed -n 's/^ledger-\(.*\)-linux-[a-z0-9]*\.tar\.gz$/\1/p')
	fi
	case "$RELEASE" in *linux-"$ARCH"*) ;; *linux-*) die "release architecture does not match --arch $ARCH" ;; esac
else
	[ -x "$repo/bin/ledger" ] || die "no $repo/bin/ledger: run 'make build' or pass --release FILE"
	mkdir -p "$stage/bin" "$stage/db"
	cp "$repo/bin/ledger" "$stage/bin/"
	cp "$repo/db/schema.sql" "$repo/db/grants.sql" "$stage/db/"
	cp -R "$repo/deploy" "$stage/deploy"
fi
[ -x "$stage/bin/ledger" ] && [ -f "$stage/deploy/install.sh" ] || die "the release is missing bin/ledger or deploy/install.sh"
VERSION=${VERSION:-$(git -C "$repo" describe --tags --always --dirty 2>/dev/null || echo dev)}
for f in ledger-firstboot.sh ledger-firstboot.service ledger-after-firstboot.conf; do
	[ -f "$here/$f" ] || die "missing $here/$f"
	cp "$here/$f" "$work/"
done

# --- Packages and sources --------------------------------------------------------
packages=(
	systemd-sysv dbus iproute2 iputils-ping openssh-server ca-certificates
	curl less nano procps tzdata postgresql
)
# Sources name the distribution's keyring explicitly (signed-by), so the
# build works on any host: apt on an Ubuntu build machine does not trust
# Debian's archive keys by default, and vice versa. The keyring package is
# also installed in the image so the same path resolves inside containers.
if [ "$DISTRO" = debian ]; then
	MIRROR=${MIRROR:-http://deb.debian.org/debian}
	keyring=/usr/share/keyrings/debian-archive-keyring.gpg
	keyring_pkg=debian-archive-keyring
	# Proxmox configures Debian guests through /etc/network/interfaces.
	packages+=(ifupdown isc-dhcp-client "$keyring_pkg")
	sources=(
		"deb [signed-by=$keyring] $MIRROR $SUITE main"
		"deb [signed-by=$keyring] $MIRROR $SUITE-updates main"
		"deb [signed-by=$keyring] http://security.debian.org/debian-security $SUITE-security main"
	)
else
	if [ "$ARCH" = arm64 ]; then MIRROR=${MIRROR:-http://ports.ubuntu.com/ubuntu-ports}; else MIRROR=${MIRROR:-http://archive.ubuntu.com/ubuntu}; fi
	keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
	keyring_pkg=ubuntu-keyring
	# Proxmox configures Ubuntu guests through systemd-networkd (part of systemd)
	# and writes /etc/resolv.conf itself, so systemd-resolved is left out.
	packages+=("$keyring_pkg")
	sources=(
		"deb [signed-by=$keyring] $MIRROR $SUITE main universe"
		"deb [signed-by=$keyring] $MIRROR $SUITE-updates main universe"
		"deb [signed-by=$keyring] $MIRROR $SUITE-security main universe"
	)
fi
[ -s "$keyring" ] || die "$keyring not found (or empty) on this machine: apt install $keyring_pkg"
pkg_list=$(IFS=,; echo "${packages[*]}")

name="$DISTRO-$OS_VERSION-ledger_${VERSION}_$ARCH.tar.zst"
mkdir -p "$OUT_DIR"
output="$OUT_DIR/$name"

# --- In-image customisation (runs on the build host, $1 = image root) --------------
cat >"$work/customize.sh" <<CUSTOMIZE
#!/bin/sh
set -eu
root=\$1
in_root() { chroot "\$root" "\$@"; }

# Ledger, installed by its own installer inside the image.
mkdir -p "\$root/opt/ledger-release"
cp -R "$stage/." "\$root/opt/ledger-release/"
mkdir -p "\$root/app/csv_data"
in_root env LEDGER_UID=990 CSV_DIR=/app/csv_data sh /opt/ledger-release/deploy/install.sh >/dev/null
in_root chown ledger:ledger /app/csv_data
chmod 0750 "\$root/app/csv_data"
rm -rf "\$root/opt/ledger-release"

# First-boot provisioning.
install -m 0755 "$work/ledger-firstboot.sh" "\$root/usr/local/sbin/ledger-firstboot"
install -m 0644 "$work/ledger-firstboot.service" "\$root/etc/systemd/system/ledger-firstboot.service"
install -d "\$root/etc/systemd/system/ledger.service.d"
install -m 0644 "$work/ledger-after-firstboot.conf" "\$root/etc/systemd/system/ledger.service.d/firstboot.conf"
in_root systemctl enable ledger-firstboot.service ledger.service ledger-backup.timer >/dev/null 2>&1
if [ "$DISTRO" = ubuntu ]; then
	in_root systemctl enable systemd-networkd.service >/dev/null 2>&1 || true
fi

# PostgreSQL: drop the cluster created at package install, so every container
# gets its own on first boot.
for c in "\$root"/etc/postgresql/*/main; do
	[ -d "\$c" ] || continue
	v=\$(basename "\$(dirname "\$c")")
	in_root pg_dropcluster "\$v" main
done

cat >"\$root/etc/motd" <<'MOTD'

  Ledger: first-boot setup has not finished yet.
  Check:  systemctl status ledger-firstboot

MOTD

# Remove anything that must be unique per container, and caches.
rm -f "\$root"/etc/ssh/ssh_host_*
: >"\$root/etc/machine-id"
rm -f "\$root/var/lib/dbus/machine-id"
rm -f "\$root/etc/hostname"
rm -rf "\$root"/var/lib/apt/lists/* "\$root"/var/cache/apt/*.bin "\$root"/var/cache/apt/archives/*.deb
find "\$root/var/log" -type f -exec truncate -s 0 {} +
rm -rf "\$root"/tmp/* "\$root"/var/tmp/* "\$root/root/.bash_history"
CUSTOMIZE
chmod +x "$work/customize.sh"

log "building $name (Ledger $VERSION on $DISTRO $OS_VERSION, $ARCH)"
mmdebstrap \
	--variant=apt \
	--architectures="$ARCH" \
	--include="$pkg_list" \
	--skip=output/dev \
	--dpkgopt='path-exclude=/usr/share/man/*' \
	--dpkgopt='path-exclude=/usr/share/doc/*' \
	--dpkgopt='path-include=/usr/share/doc/*/copyright' \
	--dpkgopt='path-exclude=/usr/share/locale/*' \
	--dpkgopt='path-include=/usr/share/locale/locale.alias' \
	--customize-hook="$work/customize.sh \"\$1\"" \
	"$SUITE" "$output" "${sources[@]}"

size=$(du -h "$output" | cut -f1)
log "template written: $output ($size)"
cat <<EOF

Next steps:
  1. Upload it to Proxmox: Datacenter > <storage> > CT Templates > Upload
     (or copy it to /var/lib/vz/template/cache/ on the host).
  2. Create a container from it: Create CT > Template: $name
     Unprivileged: yes. Under Options > Features, enable nesting.
     1 core and 512 MB RAM are enough.
  3. On the Proxmox host, bind-mount the CSV directory (before the first start):
       pct set <ctid> -mp0 /mnt/pve/truenas/ledger,mp=/app/csv_data
       chown 100990:100990 /mnt/pve/truenas/ledger
  4. Start it. On first boot it creates the database and an 'admin' user;
     the password is in /root/ledger-admin-password:
       pct exec <ctid> -- cat /root/ledger-admin-password
  5. Browse to http://<container-ip>:8080/
EOF
