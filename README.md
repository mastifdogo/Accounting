Fully vibe coded local bookkeeping software

# Accounting

Lightweight double-entry accounting web app, sized for a low-resource Proxmox LXC container.

| Layer    | Tech                                                             |
|----------|------------------------------------------------------------------|
| Backend  | Node.js 24 (TypeScript), `node:http`, `pg`, bundled to one file  |
| Database | PostgreSQL 13+                                                   |
| Frontend | SvelteKit 3 SPA (`adapter-static`), served by the backend        |
| Files    | CSV import/export in `/app/csv_data` (TrueNAS bind mount)        |
| Contract | [`api/openapi.yaml`](api/openapi.yaml)                           |

## Installation

### On Proxmox (one command)

`deploy/proxmox-deploy.sh` runs on the **Proxmox host** as root. It creates the
LXC container and installs everything inside it.

1. Download `ledger-<version>-linux-amd64.tar.gz` from a
   [GitHub Release](https://github.com/mastifdogo/Accounting/releases), or build
   one on a machine with Node.js 22.18+:

   ```sh
   make release        # dist/ledger-<version>-linux-amd64.tar.gz  (ARCH=arm64 for ARM)
   ```

   The tarball includes the Node.js runtime, so nothing else needs installing.

2. Copy the tarball to the Proxmox host, unpack it and run the script, pointing
   `--csv-path` at a directory on your TrueNAS mount:

   ```sh
   scp dist/ledger-*-linux-amd64.tar.gz root@proxmox:/root/
   ssh root@proxmox
   tar xzf ledger-*-linux-amd64.tar.gz && cd ledger-*-linux-amd64
   ./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger
   ```

3. Open the URL it prints and log in with the admin username and the generated
   password (shown once; change it under **Settings**).

The script will:

- download the latest Debian 12 template if needed
- create an unprivileged container (`nesting=1`) with the CSV directory
  bind-mounted at `/app/csv_data`, owned by the mapped service UID (100990)
- install PostgreSQL (tuned for a small container), Ledger, the systemd service
  and nightly backups to `/app/csv_data/backups`
- create the database with a restricted app role, plus the admin user
- start everything and check `/api/v1/health` and that the share is writable

Common options (`--help` lists them all):

| Option | Purpose | Default |
|--------|---------|---------|
| `--csv-path DIR` | host directory to bind-mount (required) | — |
| `--ctid ID` | container ID | next free |
| `--hostname NAME` | container hostname | `ledger` |
| `--ip CIDR --gateway IP` | static address, e.g. `192.168.1.50/24` | DHCP |
| `--vlan TAG`, `--bridge NAME`, `--nameserver IP` | network | `vmbr0` |
| `--cores N --memory MB --disk GB --storage NAME` | sizing | 1, 512, 4, `local-lvm` |
| `--port N` | web port | `8080` |
| `--admin-user NAME` | first login user | `admin` |
| `--admin-password-file FILE` | use this password instead of generating one | generated |
| `--ssh-keys FILE` | authorized_keys for root in the container | none |
| `--no-chown` | leave CSV directory ownership alone (TrueNAS NFS "Mapall") | chown |
| `--release FILE` | release tarball to install | the unpacked release |
| `--dry-run` | print every command without running it | off |
| `-y` | don't ask for confirmation | ask |

Example with a static address:

```sh
./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger \
  --ctid 120 --ip 192.168.1.50/24 --gateway 192.168.1.1 --admin-user alice
```

**Upgrading** to a new release backs up the database first, then keeps the
configuration and port:

```sh
./deploy/proxmox-deploy.sh --upgrade --ctid 120 --release ledger-<new>-linux-amd64.tar.gz
```

### As a Proxmox LXC template

**Download a ready-made one:** every
[GitHub Release](https://github.com/mastifdogo/Accounting/releases) has a
Debian 12 CT template (`debian-12-ledger_<version>_amd64.tar.zst`) with Ledger
and PostgreSQL pre-installed. On the Proxmox host:

```sh
wget -P /var/lib/vz/template/cache \
  https://github.com/mastifdogo/Accounting/releases/download/<version>/debian-12-ledger_<version>_amd64.tar.zst
```

It then appears under **local → CT Templates**. Each release's notes include
the exact URL and its SHA-256 checksum.

**Or build it yourself** (needs root and `apt install mmdebstrap zstd`):

```sh
make lxc-template                 # dist/debian-12-ledger_<version>_amd64.tar.zst
make lxc-template SUITE=noble     # Ubuntu 24.04 (also trixie, jammy; ARCH=arm64)
```

Upload it under **CT Templates**, create an unprivileged container with
nesting enabled, add the CSV bind mount
(`pct set <ctid> -mp0 /mnt/pve/truenas/ledger,mp=/app/csv_data`) and start it.
On first boot each container creates its own database, database password and
`admin` login. Read the password with
`pct exec <ctid> -- cat /root/ledger-admin-password`. Details are in
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md#alternative-a-ready-made-lxc-template).

### On any Debian/Ubuntu host or container

Without Proxmox, run the steps the script automates yourself, as root, from
the unpacked release:

```sh
apt install -y postgresql
sh deploy/install.sh                                  # user, program, systemd units, /etc/ledger/ledger.env
cp /usr/local/share/ledger/postgresql-lowmem.conf /etc/postgresql/*/main/conf.d/ledger.conf
systemctl restart postgresql
sh /usr/local/share/ledger/setup-db.sh                # roles, schema, grants; writes DATABASE_URL
ledger-user add alice                                 # prompts for a password
systemctl enable --now ledger ledger-backup.timer
```

[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) explains every step, plus TrueNAS
permissions, HTTPS behind a reverse proxy, backup/restore and troubleshooting.

### Publishing a release

Push a version tag. GitHub Actions (`.github/workflows/release.yml`) runs the
unit and PostgreSQL integration tests, then builds the release tarball and the
Debian 12 and Ubuntu 24.04 LXC templates, and attaches them with `SHA256SUMS`
to a GitHub Release:

```sh
git tag v0.4.0 && git push origin v0.4.0
```

Or use **Actions → Release → Run workflow** and enter a tag. Tags with a
suffix (`v0.5.0-rc.1`) are published as pre-releases.

## Layout

```
api/openapi.yaml      API contract (frontend and backend both follow it)
db/schema.sql         Tables, CHECK constraints, balance/immutability triggers
backend/src/main.ts       Startup, HTTP server, `ledger user` dispatch
backend/src/types.ts      Models matching the spec
backend/src/ledger.ts     Journal entry validation, transactional posting, reversal
backend/src/accounts.ts   Chart of accounts, account ledger, trial balance
backend/src/files.ts      General Ledger CSV export, file listing/download, uploads
backend/src/import.ts     All-or-nothing CSV import of journal entries
backend/src/auth.ts       Users, bcrypt passwords, sessions
backend/src/reports.ts    Balance sheet and income statement
backend/src/cli.ts        `ledger user add|passwd|disable|enable|list`
backend/src/http.ts       Routes, handlers, error mapping, request safety
backend/src/decode.ts     Strict JSON request decoding (exact integers, no unknown fields)
backend/src/csv.ts        RFC 4180 CSV reader/writer
backend/src/money.ts      Integer-cent parsing and formatting (bigint)
backend/ledger.sh         Launcher installed as /usr/local/bin/ledger
backend/test/             Unit tests and PostgreSQL integration tests (node:test)
frontend/             SvelteKit app; API types generated from api/openapi.yaml
deploy/               proxmox-deploy.sh, install.sh, setup-db.sh, systemd units, backups
deploy/lxc-template/  LXC template builder and first-boot provisioning
.github/workflows/    release.yml: tests, builds and publishes releases + templates
docs/DEPLOYMENT.md    Proxmox LXC + TrueNAS deployment guide
```

## Accounting rules and where they are enforced

| Rule | Application | PostgreSQL |
|------|-------------|------------|
| Entry has >= 2 lines | `validateJournalEntry` | deferred constraint trigger, checked at `COMMIT` |
| Debits = credits **per currency** | `checkLinesPostable` | deferred constraint trigger, checked at `COMMIT` |
| Line currency = account currency | taken from the account | composite FK `(account_id, currency)` |
| Only CAD / USD, 2 decimals | `isCurrency` | `currencies` table + `CHECK (minor_units = 2)` |
| Account code/currency fixed | not editable via API | `BEFORE UPDATE` trigger |
| No zero / both-sided lines | `validateJournalEntry` | `CHECK (amount <> 0)` |
| Integer money only | `bigint` cents; JSON floats rejected | `BIGINT` cents |
| All-or-nothing posting | one `BEGIN … COMMIT` per entry | deferred triggers abort the `COMMIT` |
| Posted rows immutable | no update/delete code paths | triggers reject `UPDATE`/`DELETE`/`TRUNCATE`; lines can't be added to an entry from a later transaction |
| Corrections | `POST /journal-entries/{id}/reverse` | `UNIQUE (reverses_id)`: reversed once at most |
| Inactive accounts | pre-check with `FOR SHARE` lock | `BEFORE INSERT` trigger |
| CSV import all-or-nothing, never twice | one transaction per file | `UNIQUE (sha256)` on `csv_imports` |
| Who posted what | session user recorded | `created_by` / `imported_by`; users can't be deleted |
| App can't bypass the rules | — | app role owns nothing (see `db/grants.sql`) |

Lines are stored as one signed `amount` (debit > 0, credit < 0). The API exposes
separate `debit_cents` / `credit_cents`, and refers to accounts by `code`.

### Currencies

Each account has one currency (CAD or USD) and each journal entry must balance
within every currency, so amounts are exact and no exchange rates are stored.
Move value between currencies through an FX clearing account in each currency
(type `equity`, so the balance sheet's assets show only real balances):

| account              | debit   | credit  |
|----------------------|---------|---------|
| 1900 FX clearing CAD | 1370.00 |         |
| 1000 Bank CAD        |         | 1370.00 |
| 1010 Bank USD        | 1000.00 |         |
| 1910 FX clearing USD |         | 1000.00 |

Reports (trial balance, ledger export) total each currency separately.

### CSV import

Drop a file in `/app/csv_data` and `POST /api/v1/imports/journal-entries
{"filename": "..."}`. Rows sharing an `entry_key` form one entry:

```
entry_key,entry_date,description,account_code,debit,credit
S1,2026-10-01,Cash sale,1000,250.00,
S1,,,4000,,250.00
```

Optional columns: `reference`, `memo`, `currency`. Full rules are in
`api/openapi.yaml`.

## Running

For production, see [Installation](#installation). To run locally for
development:

Needs Node.js 22.18 or newer.

```sh
createdb ledger && psql -d ledger -v ON_ERROR_STOP=1 -f db/schema.sql
make build            # frontend (vite build) + backend bundle into out/
export DATABASE_URL=postgres://localhost/ledger
./out/bin/ledger user add alice
CSV_DIR=./csv ./out/bin/ledger      # http://localhost:8080
```

| Variable        | Default          |
|-----------------|------------------|
| `DATABASE_URL`  | required         |
| `HTTP_ADDR`     | `:8080`          |
| `CSV_DIR`       | `/app/csv_data`  |
| `DB_MAX_CONNS`  | `4`              |
| `COOKIE_SECURE` | `auto`           |
| `WEB_DIR`       | `web` next to `ledger.mjs` |
| `LEDGER_MAX_HEAP_MB` | `96` (JavaScript heap limit, set by the launcher) |

### Authentication

Users log in with a username and password (bcrypt) and get a server-side
session in an HttpOnly, SameSite=Strict cookie. Sessions end after 8 hours idle
or 7 days. Repeated failed logins are throttled. There's no sign-up page:
users are managed with `ledger user …` on the server. Every journal entry and
import records who posted it.

### Reports

Trial balance, balance sheet and income statement, each per currency. Net
income that hasn't been closed to equity appears on the balance sheet as
"Net income (unclosed)", so assets = liabilities + equity always holds.

### Frontend development

```sh
(cd backend && npm ci && npm start) &   # API on :8080, straight from the TypeScript sources
(cd frontend && npm run dev)            # Vite on :5173, proxies /api to :8080
make api-types                          # after editing api/openapi.yaml
```

The frontend only talks to the API through `openapi-fetch` with types
generated from `api/openapi.yaml`, so a contract change that breaks the UI
fails `npm run check`.

In production the app connects as a role that does **not** own the tables, so
it can't drop or disable the triggers (`deploy/setup-db.sh`, `db/grants.sql`).

## Tests

```sh
make test                                                       # type checks + unit tests (backend and frontend)
make test-integration TEST_DATABASE_URL=postgres://postgres:secret@localhost/postgres
```

The integration tests create a throwaway database, apply `db/schema.sql`, test
the API end to end (including direct SQL attempts to bypass the rules), and
drop the database afterwards.
