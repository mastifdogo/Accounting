# Accounting

Lightweight double-entry accounting web app, sized for a low-resource Proxmox LXC container.

| Layer    | Tech                                                             |
|----------|------------------------------------------------------------------|
| Backend  | Go, single static binary (`CGO_ENABLED=0`), `chi` router, `pgx`  |
| Database | PostgreSQL 13+                                                   |
| Frontend | SvelteKit + `adapter-static`, served by the Go binary (Step 2)   |
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
backend/handlers.go   HTTP handlers and error mapping
```

## Accounting rules and where they are enforced

| Rule | Go | PostgreSQL |
|------|----|------------|
| Entry has >= 2 lines | `ValidateJournalEntry` | deferred constraint trigger, checked at `COMMIT` |
| Debits = credits (signed sum = 0) | `ValidateJournalEntry` | deferred constraint trigger, checked at `COMMIT` |
| No zero / both-sided lines | `ValidateJournalEntry` | `CHECK (amount <> 0)` |
| Integer money only | `int64` cents; JSON floats rejected | `BIGINT` cents |
| All-or-nothing posting | one `BEGIN … COMMIT` per entry | deferred triggers abort the `COMMIT` |
| Posted rows immutable | no update/delete code paths | triggers reject `UPDATE`/`DELETE`/`TRUNCATE`; lines can't be added to an entry from a later transaction |
| Corrections | `POST /journal-entries/{id}/reverse` | `UNIQUE (reverses_id)`: reversed once at most |
| Inactive accounts | pre-check with `FOR SHARE` lock | `BEFORE INSERT` trigger |

Lines are stored as one signed `amount` (debit > 0, credit < 0). The API exposes
separate `debit_cents` / `credit_cents`.

## Running

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/schema.sql
make build
DATABASE_URL=postgres://ledger_app:...@db/ledger ./bin/ledger
```

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
make test                                                       # unit tests
make test-integration TEST_DATABASE_URL=postgres://postgres@localhost/postgres
```

The integration tests create a throwaway database, apply `db/schema.sql`, test
the API end to end (including direct SQL attempts to bypass the rules), and
drop the database afterwards.
