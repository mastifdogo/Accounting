# Deploying Ledger in a Proxmox LXC container

This guide installs Ledger and PostgreSQL in one small, unprivileged Debian
container, with CSV files and nightly database backups on a TrueNAS share.

```
Proxmox host
 └─ LXC "ledger" (Debian 12, 1 core, 512 MB)
     ├─ ledger.service        :8080   (single static binary, ~15–30 MB RAM)
     ├─ postgresql             localhost only
     └─ /app/csv_data  ◄── bind mount ── TrueNAS dataset (CSV files + backups/)
```

What runs where:

| Path | Contents |
|------|----------|
| `/usr/local/bin/ledger` | the server (UI embedded) and the `ledger user` CLI |
| `/usr/local/sbin/ledger-user` | root wrapper: runs `ledger user …` with the service config |
| `/usr/local/bin/ledger-backup` | nightly `pg_dump` (run by `ledger-backup.timer`) |
| `/etc/ledger/ledger.env` | configuration, including the DB password (root:ledger, 0640) |
| `/usr/local/share/ledger/` | `schema.sql`, `grants.sql`, `setup-db.sh`, PostgreSQL tuning |
| `/app/csv_data/` | CSV uploads/exports; `backups/` holds database dumps |

---

## Quick start: one command on the Proxmox host

`deploy/proxmox-deploy.sh` does steps 2–6 below for you. It creates the
container, bind-mounts and maps the CSV directory, installs PostgreSQL and
Ledger, creates the database and an admin user, starts everything and checks
health. Copy a release tarball to the Proxmox host (see step 1) and run:

```sh
tar xzf ledger-<version>-linux-amd64.tar.gz && cd ledger-<version>-linux-amd64
./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger
```

It shows the plan, asks for confirmation, and at the end prints the URL and a
generated admin password (shown once; change it under **Settings**).

Common options (run `--help` for all of them):

```sh
./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger \
  --ctid 120 --hostname ledger \
  --ip 192.168.1.50/24 --gateway 192.168.1.1 [--vlan 20] \
  --memory 512 --disk 4 --storage local-lvm \
  --admin-user alice --admin-password-file /root/pw.txt \
  --ssh-keys /root/.ssh/authorized_keys \
  --no-chown           # if TrueNAS maps ownership itself (NFS Mapall)
  --dry-run            # print the commands without running them
```

Upgrade an existing container to a new release. It backs up the database
first and keeps the configuration and port:

```sh
./deploy/proxmox-deploy.sh --upgrade --ctid 120 --release ledger-<new>-linux-amd64.tar.gz
```

The rest of this guide explains each step, for manual installs and
troubleshooting.

---

## 1. Build a release (on your workstation)

Requires Go 1.25+ and Node.js 22.

```sh
make release                 # dist/ledger-<version>-linux-amd64.tar.gz
make release GOARCH=arm64    # for an ARM host
```

The tarball contains the binary, SQL files, `deploy/` scripts and these docs.
Node is not needed on the server.

## 2. Create the container (Proxmox host shell)

```sh
pveam update
pveam available --section system | grep debian-12      # note the exact template name
pveam download local debian-12-standard_<version>_amd64.tar.zst

pct create 120 local:vztmpl/debian-12-standard_<version>_amd64.tar.zst \
  --hostname ledger --unprivileged 1 --features nesting=1 \
  --cores 1 --memory 512 --swap 256 --rootfs local-lvm:4 \
  --net0 name=eth0,bridge=vmbr0,ip=dhcp --onboot 1
```

`nesting=1` lets systemd apply the service sandbox (private /tmp, read-only
system, etc.). It is the default for new unprivileged containers in recent
Proxmox versions.

Give the container a fixed IP or a DHCP reservation so the URL stays stable.

## 3. Bind-mount the TrueNAS share

Mount the TrueNAS dataset on the **Proxmox host** first (Datacenter → Storage →
Add → NFS, or an `/etc/fstab` entry). Then pass a directory into the
container:

```sh
mkdir -p /mnt/pve/truenas/ledger            # host path on the TrueNAS mount
pct set 120 -mp0 /mnt/pve/truenas/ledger,mp=/app/csv_data
```

**Permissions.** The service runs as user `ledger` with UID/GID **990** inside
the container (fixed by `install.sh`). In an unprivileged container, UID 990
appears on the host as **100990**. Pick one:

- *TrueNAS NFS share → Advanced → Mapall User/Group* set to a TrueNAS user that
  owns the dataset. Every write from the Proxmox host maps to that user, so
  container UIDs don't matter. Simplest.
- Or make the directory owned by the mapped UID (on the host, or on TrueNAS
  if it lets you chown numerically):
  `chown 100990:100990 /mnt/pve/truenas/ledger`

`install.sh` checks that `ledger` can write to `/app/csv_data` and prints the
right `chown` command if it can't.

## 4. Install (inside the container)

```sh
pct enter 120                        # or ssh root@ledger
apt update && apt install -y postgresql
```

Copy the release tarball into the container (e.g. `pct push 120
ledger-….tar.gz /root/ledger.tar.gz` on the host), then:

```sh
cd /root && tar xzf ledger.tar.gz && cd ledger-*/
sh deploy/install.sh
```

Tune PostgreSQL for the small container and restart it:

```sh
cp /usr/local/share/ledger/postgresql-lowmem.conf /etc/postgresql/*/main/conf.d/ledger.conf
systemctl restart postgresql
```

## 5. Create the database

```sh
sh /usr/local/share/ledger/setup-db.sh
```

This creates:

- `ledger_owner` (cannot log in): owns every table, trigger and function.
- `ledger_app` (the service logs in as this, with a generated password): it can
  read data and add new entries, but it **cannot** alter tables, disable the
  balance/immutability triggers, or update/delete posted entries.

It applies `schema.sql` and `grants.sql` and writes `DATABASE_URL` into
`/etc/ledger/ledger.env`. Running it again is safe: existing roles, data and
the password are kept.

## 6. Create a user and start

```sh
ledger-user add alice                # prompts for a password (10+ characters)
systemctl enable --now ledger ledger-backup.timer
systemctl status ledger
```

Open `http://<container-ip>:8080/` and log in.

Managing users later:

```sh
ledger-user list
ledger-user passwd alice             # also logs out all of alice's sessions
ledger-user disable bob              # blocks login and ends sessions; history keeps the name
ledger-user enable bob
```

Users are never deleted: journal entries record who posted them.

## 7. HTTPS (recommended)

Ledger speaks plain HTTP. For access beyond a trusted LAN, put it behind a
reverse proxy that terminates TLS. With [Caddy](https://caddyserver.com) in
the same container:

```sh
apt install -y caddy
cat >/etc/caddy/Caddyfile <<'EOF'
ledger.home.example.com {
	reverse_proxy 127.0.0.1:8080
}
EOF
systemctl reload caddy
```

Then set `HTTP_ADDR=127.0.0.1:8080` in `/etc/ledger/ledger.env` and
`systemctl restart ledger`, so the app is reachable only through the proxy.

Any proxy works (Nginx Proxy Manager, Traefik, OPNsense/HAProxy) as long as it:

- passes the original `Host` header (nginx: `proxy_set_header Host $host;`);
  Ledger rejects cross-origin writes by comparing `Origin` with `Host`.
- sends `X-Forwarded-Proto: https`, so the session cookie gets the `Secure`
  flag (`COOKIE_SECURE=auto`, the default). Or set `COOKIE_SECURE=true`.

## 8. Backups and restore

`ledger-backup.timer` runs nightly at about 02:30. It writes
`/app/csv_data/backups/ledger-<UTC timestamp>.dump` (PostgreSQL custom format,
checked with `pg_restore --list` before keeping it). It also deletes dumps
older than `BACKUP_KEEP_DAYS` (default 30). Because the files land on TrueNAS,
your ZFS snapshots and replication cover them too.

```sh
systemctl start ledger-backup        # back up now
journalctl -u ledger-backup          # see results
systemctl list-timers ledger-backup.timer
```

**Restore** (keeps the current database as `ledger_old`):

```sh
DUMP=/app/csv_data/backups/ledger-20261005T023000Z.dump    # pick one
systemctl stop ledger
runuser -u postgres -- psql -c "ALTER DATABASE ledger RENAME TO ledger_old"
runuser -u postgres -- createdb -O ledger_owner ledger
runuser -u postgres -- psql -d ledger -v ON_ERROR_STOP=1 \
  -c "ALTER SCHEMA public OWNER TO ledger_owner" \
  -c "REVOKE CREATE ON SCHEMA public FROM PUBLIC" \
  -c "REVOKE ALL ON DATABASE ledger FROM PUBLIC" \
  -c "GRANT CONNECT ON DATABASE ledger TO ledger_app"
runuser -u postgres -- pg_restore -d ledger --role=ledger_owner --no-owner \
  --exit-on-error --single-transaction < "$DUMP"
{ echo "SET ROLE ledger_owner;"; cat /usr/local/share/ledger/grants.sql; } |
  runuser -u postgres -- psql -d ledger -v ON_ERROR_STOP=1 -v app_role=ledger_app
systemctl start ledger
# once satisfied:  runuser -u postgres -- dropdb ledger_old
```

The dump is fed on standard input because the `postgres` user can't read the
share. Restoring brings back the triggers and constraints, so the restored
ledger is enforced exactly like the original.

Test a restore now and then (into a scratch database name) so you know the
backups work.

## 9. Upgrading

Build a new release and copy it into the container, then:

```sh
tar xzf ledger-<new>.tar.gz && cd ledger-<new>/
sh deploy/install.sh                 # replaces binary, scripts and units; keeps ledger.env
systemctl restart ledger
```

If a release changes the database schema, its notes include a migration to
run as `ledger_owner` before restarting. Release 0.x ships the baseline
schema only.

## 10. Troubleshooting

| Symptom | Fix |
|---------|-----|
| `status=226/NAMESPACE` when starting | Enable `nesting=1` on the container (`pct set 120 --features nesting=1`, then restart it). Or comment out the `Protect*`/`Private*` lines in `/etc/systemd/system/ledger.service` and run `systemctl daemon-reload`. |
| Exports or uploads fail with "permission denied" | `/app/csv_data` isn't writable by UID 990 (host UID 100990). See step 3. |
| `ledger.service` waits forever at boot | `RequiresMountsFor=/app/csv_data`: the bind mount is missing. Check `pct config 120` and the host's TrueNAS mount. |
| `password authentication failed for user "ledger_app"` | Re-run `APP_PASSWORD=<new> sh /usr/local/share/ledger/setup-db.sh`; it resets the password and updates `ledger.env`. |
| Log in works but you're logged out immediately behind a proxy | The proxy serves HTTPS but doesn't send `X-Forwarded-Proto`, or `COOKIE_SECURE=true` is set while you browse over plain HTTP. |
| "cross-origin request rejected" (403) behind a proxy | The proxy rewrites `Host`. Pass the original host through. |
| Forgot the only password | `ledger-user passwd <name>` as root in the container. |

Logs: `journalctl -u ledger -f` (JSON lines, one per request).

## Resource use

Measured: the server peaks at about 16 MB RSS after a full workflow (accounts,
entries, CSV import/export, reports). It runs with `MemoryMax=256M` and
`GOMEMLIMIT=160MiB`. PostgreSQL with `postgresql-lowmem.conf` uses about
100–150 MB, so 512 MB of RAM covers the whole container with room to spare.
