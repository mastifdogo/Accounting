# Accounting

Lightweight double-entry accounting web app, sized for a low-resource Proxmox LXC container.

| Layer    | Tech                                                             |
|----------|------------------------------------------------------------------|
| Backend  | Go, single static binary (`CGO_ENABLED=0`), `chi` router, `pgx`  |
| Database | PostgreSQL 13+                                                   |
| Frontend | SvelteKit 3 SPA (`adapter-static`), embedded in the Go binary    |
| Files    | CSV import/export in `/app/csv_data` (TrueNAS bind mount)        |
| Contract | [`api/openapi.yaml`](api/openapi.yaml)                           |

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
deploy/               systemd units, install/DB setup scripts, backups
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

**Production (Proxmox):** `make release`, copy the tarball to the Proxmox host, then

```sh
./deploy/proxmox-deploy.sh --csv-path /mnt/pve/truenas/ledger
```

This creates the LXC container and installs everything. Details, manual steps,
HTTPS, backups and upgrades are in [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

**Locally:**

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

| Variable       | Default          |
|----------------|------------------|
| `DATABASE_URL` | required         |
| `HTTP_ADDR`    | `:8080`          |
| `CSV_DIR`      | `/app/csv_data`  |
| `DB_MAX_CONNS` | `4`              |

Run the app as a role that does **not** own the tables, so it can't drop or
disable the triggers (see the end of `db/schema.sql`).

## Tests

```sh
make test                                                       # Go + frontend checks/unit tests
make test-integration TEST_DATABASE_URL=postgres://postgres@localhost/postgres
```

The integration tests create a throwaway database, apply `db/schema.sql`, test
the API end to end (including direct SQL attempts to bypass the rules), and
drop the database afterwards.
