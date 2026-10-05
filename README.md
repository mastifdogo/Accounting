# Accounting

Lightweight double-entry accounting web app, sized for a low-resource Proxmox LXC container.

| Layer    | Tech                                                             |
|----------|------------------------------------------------------------------|
| Backend  | Go, single static binary (`CGO_ENABLED=0`), `chi` router, `pgx`  |
| Database | PostgreSQL 13+                                                   |
| Frontend | SvelteKit 3 SPA (`adapter-static`), embedded in the Go binary    |
| Files    | CSV import/export in `/app/csv_data` (TrueNAS bind mount)        |
| Contract | [`api/openapi.yaml`](api/openapi.yaml)                           |

## Installation

### On Proxmox (one command)

`deploy/proxmox-deploy.sh` runs on the **Proxmox host** as root. It creates the
LXC container and installs everything inside it.

1. Build a release on a machine with Go 1.25+ and Node.js 22:

   ```sh
   make release        # dist/ledger-<version>-linux-amd64.tar.gz  (GOARCH=arm64 for ARM)
   ```

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

### On any Debian/Ubuntu host or container

Without Proxmox, run the steps the script automates yourself, as root, from
the unpacked release:

```sh
apt install -y postgresql
sh deploy/install.sh                                  # user, binary, systemd units, /etc/ledger/ledger.env
cp /usr/local/share/ledger/postgresql-lowmem.conf /etc/postgresql/*/main/conf.d/ledger.conf
systemctl restart postgresql
sh /usr/local/share/ledger/setup-db.sh                # roles, schema, grants; writes DATABASE_URL
ledger-user add alice                                 # prompts for a password
systemctl enable --now ledger ledger-backup.timer
```

[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) explains every step, plus TrueNAS
permissions, HTTPS behind a reverse proxy, backup/restore and troubleshooting.

## Layout

```
api/openapi.yaml      API contract (frontend and backend both follow it)
db/schema.sql         Tables, CHECK constraints, balance/immutability triggers
backend/main.go       Config, DB pool, HTTP server, models matching the spec
backend/ledger.go     Journal entry validation, transactional posting, reversal
backend/accounts.go   Chart of accounts, account ledger, trial balance
backend/export.go     General Ledger CSV export, CSV file listing/download
backend/import.go     All-or-nothing CSV import of journal entries
backend/upload.go     CSV upload (never overwrites)
backend/auth.go       Users, bcrypt passwords, sessions, login rate limiting
backend/reports.go    Balance sheet and income statement
backend/cli.go        `ledger user add|passwd|disable|enable|list`
backend/static.go     Serves the embedded frontend (backend/web) with SPA fallback
backend/handlers.go   HTTP handlers, error mapping, request safety
frontend/             SvelteKit app; API types generated from api/openapi.yaml
deploy/               proxmox-deploy.sh, install.sh, setup-db.sh, systemd units, backups
docs/DEPLOYMENT.md    Proxmox LXC + TrueNAS deployment guide
```

## Accounting rules and where they are enforced

| Rule | Go | PostgreSQL |
|------|----|------------|
| Entry has >= 2 lines | `ValidateJournalEntry` | deferred constraint trigger, checked at `COMMIT` |
| Debits = credits **per currency** | `checkLinesPostable` | deferred constraint trigger, checked at `COMMIT` |
| Line currency = account currency | taken from the account | composite FK `(account_id, currency)` |
| Only CAD / USD, 2 decimals | `Currency.Valid` | `currencies` table + `CHECK (minor_units = 2)` |
| Account code/currency fixed | not editable via API | `BEFORE UPDATE` trigger |
| No zero / both-sided lines | `ValidateJournalEntry` | `CHECK (amount <> 0)` |
| Integer money only | `int64` cents; JSON floats rejected | `BIGINT` cents |
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

```sh
createdb ledger && psql -d ledger -v ON_ERROR_STOP=1 -f db/schema.sql
make build            # npm ci + vite build, then a static Go binary embedding it
export DATABASE_URL=postgres://localhost/ledger
./bin/ledger user add alice
CSV_DIR=./csv ./bin/ledger      # http://localhost:8080
```

| Variable        | Default          |
|-----------------|------------------|
| `DATABASE_URL`  | required         |
| `HTTP_ADDR`     | `:8080`          |
| `CSV_DIR`       | `/app/csv_data`  |
| `DB_MAX_CONNS`  | `4`              |
| `COOKIE_SECURE` | `auto`           |

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
./bin/ledger &                 # API on :8080
cd frontend && npm run dev     # Vite on :5173, proxies /api to :8080
make api-types                 # after editing api/openapi.yaml
```

The frontend only talks to the API through `openapi-fetch` with types
generated from `api/openapi.yaml`, so a contract change that breaks the UI
fails `npm run check`.

In production the app connects as a role that does **not** own the tables, so
it can't drop or disable the triggers (`deploy/setup-db.sh`, `db/grants.sql`).

## Tests

```sh
make test                                                       # Go + frontend checks/unit tests
make test-integration TEST_DATABASE_URL=postgres://postgres@localhost/postgres
```

The integration tests create a throwaway database, apply `db/schema.sql`, test
the API end to end (including direct SQL attempts to bypass the rules), and
drop the database afterwards.
