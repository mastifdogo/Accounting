-- =============================================================================
-- Double-entry accounting ledger: PostgreSQL schema (PostgreSQL 13+)
--
-- Invariants enforced IN THE DATABASE (the backend validates too, but the
-- database is the final authority):
--
--   1. Every journal entry has at least two transaction lines.
--   2. For EACH CURRENCY in a journal entry, the signed sum of its lines is
--      exactly zero (debits are stored as positive cents, credits as negative
--      cents). Supported currencies: CAD and USD.
--   3. Money is stored as BIGINT cents. No floating point anywhere.
--   4a. Every account has one fixed currency, and every line is in its
--      account's currency (composite foreign key). Cross-currency movements
--      are posted through an FX clearing account in each currency, so every
--      currency balances on its own and no exchange rates are stored.
--   4. Posted journal entries, transaction lines and CSV import records are
--      immutable:
--      UPDATE, DELETE and TRUNCATE are rejected. Corrections are made by
--      posting a reversing entry.
--   5. Lines can only be added to a journal entry inside the same database
--      transaction that created it, so a posted entry cannot be "amended" by
--      appending more (even balanced) lines later.
--
-- Invariants 1 and 2 are checked by DEFERRED constraint triggers, which run at
-- COMMIT time. This lets the backend insert the header and all lines inside a
-- single BEGIN ... COMMIT block; if the entry does not balance, the COMMIT
-- fails and the whole entry is rolled back.
--
-- Apply with:  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f db/schema.sql
-- =============================================================================

BEGIN;

-- -----------------------------------------------------------------------------
-- Account types
-- -----------------------------------------------------------------------------
CREATE TYPE account_type AS ENUM ('asset', 'liability', 'equity', 'revenue', 'expense');

-- -----------------------------------------------------------------------------
-- currencies: the ledger works in cents, so only 2-decimal currencies allowed
-- -----------------------------------------------------------------------------
CREATE TABLE currencies (
    code        CHAR(3)  PRIMARY KEY,
    name        TEXT     NOT NULL,
    minor_units SMALLINT NOT NULL DEFAULT 2,

    CONSTRAINT currencies_code_format CHECK (code ~ '^[A-Z]{3}$'),
    CONSTRAINT currencies_cents_only  CHECK (minor_units = 2)
);

INSERT INTO currencies (code, name) VALUES
    ('CAD', 'Canadian dollar'),
    ('USD', 'US dollar');

-- -----------------------------------------------------------------------------
-- accounts: the chart of accounts. `code` is the public identifier used by the
-- API and CSV files; `id` is internal.
-- -----------------------------------------------------------------------------
CREATE TABLE accounts (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        TEXT         NOT NULL,
    name        TEXT         NOT NULL,
    type        account_type NOT NULL,
    currency    CHAR(3)      NOT NULL REFERENCES currencies (code) ON DELETE RESTRICT,
    description TEXT         NOT NULL DEFAULT '',
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT accounts_code_unique   UNIQUE (code),
    -- Target of the transactions (account_id, currency) foreign key.
    CONSTRAINT accounts_id_currency_unique UNIQUE (id, currency),
    CONSTRAINT accounts_code_format   CHECK (code ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$'),
    CONSTRAINT accounts_name_nonempty CHECK (length(btrim(name)) BETWEEN 1 AND 200)
);

-- -----------------------------------------------------------------------------
-- users: people who can log in. Users are never deleted (journal entries
-- reference who posted them); disable them instead.
-- -----------------------------------------------------------------------------
CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    is_active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,

    CONSTRAINT users_username_unique UNIQUE (username),
    CONSTRAINT users_username_format CHECK (username ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    CONSTRAINT users_password_hash_nonempty CHECK (length(password_hash) > 0)
);

-- -----------------------------------------------------------------------------
-- sessions: server-side login sessions. Only a SHA-256 hash of the session
-- token is stored, so a database leak does not expose usable cookies.
-- -----------------------------------------------------------------------------
CREATE TABLE sessions (
    token_hash   BYTEA       PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,

    CONSTRAINT sessions_token_hash_length CHECK (length(token_hash) = 32)
);

CREATE INDEX sessions_user_idx    ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- -----------------------------------------------------------------------------
-- csv_imports: one row per imported CSV file. The SHA-256 of the file content
-- is unique, so the same file can never be imported twice.
-- -----------------------------------------------------------------------------
CREATE TABLE csv_imports (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    filename    TEXT        NOT NULL,
    sha256      CHAR(64)    NOT NULL,
    entry_count INTEGER     NOT NULL,
    line_count  INTEGER     NOT NULL,
    imported_by BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT csv_imports_sha256_unique UNIQUE (sha256),
    CONSTRAINT csv_imports_sha256_format CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT csv_imports_counts        CHECK (entry_count >= 1 AND line_count >= 2 * entry_count)
);

-- -----------------------------------------------------------------------------
-- journal_entries: one row per balanced business event (the header)
-- -----------------------------------------------------------------------------
CREATE TABLE journal_entries (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_date   DATE        NOT NULL,
    description  TEXT        NOT NULL,
    reference    TEXT        NOT NULL DEFAULT '',
    -- If this entry reverses another, points at the original. An entry can be
    -- reversed at most once.
    reverses_id  BIGINT      REFERENCES journal_entries (id) ON DELETE RESTRICT,
    -- Set when the entry was created by a CSV import.
    import_id    BIGINT      REFERENCES csv_imports (id) ON DELETE RESTRICT,
    -- Who posted the entry (audit trail). NULL only for rows written directly
    -- in SQL by an administrator.
    created_by   BIGINT      REFERENCES users (id) ON DELETE RESTRICT,
    posted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ID of the database transaction that created this row. Always overwritten
    -- by trigger; used to stop lines being appended to an already-posted entry.
    created_txid BIGINT      NOT NULL DEFAULT txid_current(),

    CONSTRAINT journal_entries_description_nonempty CHECK (length(btrim(description)) BETWEEN 1 AND 500),
    CONSTRAINT journal_entries_reference_length     CHECK (length(reference) <= 100),
    CONSTRAINT journal_entries_not_self_reversing   CHECK (reverses_id IS NULL OR reverses_id <> id),
    CONSTRAINT journal_entries_reversed_once        UNIQUE (reverses_id)
);

CREATE INDEX journal_entries_entry_date_idx ON journal_entries (entry_date, id);
CREATE INDEX journal_entries_import_idx     ON journal_entries (import_id) WHERE import_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- transactions: the individual debit/credit lines of a journal entry
--
-- amount > 0  => debit
-- amount < 0  => credit
-- amount = 0  => rejected
-- -----------------------------------------------------------------------------
CREATE TABLE transactions (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    journal_entry_id BIGINT NOT NULL REFERENCES journal_entries (id) ON DELETE RESTRICT,
    account_id       BIGINT NOT NULL,
    currency         CHAR(3) NOT NULL,
    line_number      INTEGER NOT NULL,
    amount           BIGINT NOT NULL,
    memo             TEXT   NOT NULL DEFAULT '',

    -- A line is always in its account's currency. Because this FK references
    -- (id, currency), an account's currency also cannot change once it has
    -- postings.
    CONSTRAINT transactions_account_currency_fkey
        FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency)
        ON UPDATE RESTRICT ON DELETE RESTRICT,

    CONSTRAINT transactions_amount_nonzero  CHECK (amount <> 0),
    -- Exclude the BIGINT minimum so that negating any amount can never overflow.
    CONSTRAINT transactions_amount_range    CHECK (amount > -9223372036854775808),
    CONSTRAINT transactions_line_number_pos CHECK (line_number >= 1),
    CONSTRAINT transactions_memo_length     CHECK (length(memo) <= 500),
    CONSTRAINT transactions_line_unique     UNIQUE (journal_entry_id, line_number)
);

CREATE INDEX transactions_account_idx ON transactions (account_id, journal_entry_id);

-- =============================================================================
-- Trigger functions
-- =============================================================================

-- Shared balance check: >= 2 lines and, per currency, signed sum exactly zero.
-- SUM over BIGINT returns NUMERIC, so it cannot overflow while summing.
CREATE FUNCTION assert_journal_entry_balanced(p_entry_id BIGINT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    v_lines    INTEGER;
    v_currency CHAR(3);
    v_sum      NUMERIC;
BEGIN
    -- Defensive: nothing to check if the header does not exist.
    IF NOT EXISTS (SELECT 1 FROM journal_entries WHERE id = p_entry_id) THEN
        RETURN;
    END IF;

    SELECT count(*) INTO v_lines FROM transactions WHERE journal_entry_id = p_entry_id;

    IF v_lines < 2 THEN
        RAISE EXCEPTION 'journal entry % must have at least 2 transaction lines (has %)', p_entry_id, v_lines
              USING ERRCODE = 'check_violation',
                    CONSTRAINT = 'journal_entry_min_lines';
    END IF;

    SELECT currency, sum(amount)
      INTO v_currency, v_sum
      FROM transactions
     WHERE journal_entry_id = p_entry_id
     GROUP BY currency
    HAVING sum(amount) <> 0
     ORDER BY currency
     LIMIT 1;

    IF FOUND THEN
        RAISE EXCEPTION 'journal entry % is unbalanced in %: debits minus credits = % cents',
                        p_entry_id, v_currency, v_sum
              USING ERRCODE = 'check_violation',
                    CONSTRAINT = 'journal_entry_balanced';
    END IF;
END;
$$;

-- Deferred check fired once per inserted line (at COMMIT).
CREATE FUNCTION trg_transactions_check_balance() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_journal_entry_balanced(NEW.journal_entry_id);
    RETURN NULL;
END;
$$;

-- Deferred check fired per inserted header (at COMMIT). Catches entries that
-- were inserted with zero lines, where no line trigger would ever fire.
CREATE FUNCTION trg_journal_entries_check_balance() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_journal_entry_balanced(NEW.id);
    RETURN NULL;
END;
$$;

-- Stamp the creating transaction id; never trust a client-supplied value.
CREATE FUNCTION trg_journal_entries_stamp_txid() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    NEW.created_txid := txid_current();
    NEW.posted_at    := now();
    RETURN NEW;
END;
$$;

-- A line may only be added to an entry created in the current transaction,
-- and only against an active account.
CREATE FUNCTION trg_transactions_before_insert() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    v_txid   BIGINT;
    v_active BOOLEAN;
BEGIN
    SELECT created_txid INTO v_txid FROM journal_entries WHERE id = NEW.journal_entry_id;
    -- A missing entry is left for the foreign key to report.
    IF FOUND AND v_txid <> txid_current() THEN
        RAISE EXCEPTION 'journal entry % is already posted; lines cannot be added to it', NEW.journal_entry_id
              USING ERRCODE = 'restrict_violation',
                    CONSTRAINT = 'journal_entry_immutable';
    END IF;

    SELECT is_active INTO v_active FROM accounts WHERE id = NEW.account_id;
    IF FOUND AND NOT v_active THEN
        RAISE EXCEPTION 'account % is inactive and cannot receive postings', NEW.account_id
              USING ERRCODE = 'check_violation',
                    CONSTRAINT = 'account_active';
    END IF;

    RETURN NEW;
END;
$$;

-- Generic immutability guard for posted ledger rows.
CREATE FUNCTION trg_reject_modification() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: posted ledger records are immutable; post a reversing entry instead',
                    TG_OP, TG_TABLE_NAME
          USING ERRCODE = 'restrict_violation',
                CONSTRAINT = TG_TABLE_NAME || '_immutable';
END;
$$;

-- Accounts may be renamed or deactivated, but their code never changes and
-- their type and currency are fixed once they have postings (changing it would silently rewrite historical reports), and
-- accounts with postings are protected from deletion by the FK as well.
CREATE FUNCTION trg_accounts_before_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id THEN
        RAISE EXCEPTION 'account id cannot be changed'
              USING ERRCODE = 'restrict_violation', CONSTRAINT = 'account_id_immutable';
    END IF;

    -- The code is the account's public identifier (API, CSV files), so it
    -- never changes.
    IF NEW.code <> OLD.code THEN
        RAISE EXCEPTION 'account code % cannot be changed', OLD.code
              USING ERRCODE = 'restrict_violation', CONSTRAINT = 'account_code_immutable';
    END IF;

    IF NEW.currency <> OLD.currency
       AND EXISTS (SELECT 1 FROM transactions WHERE account_id = OLD.id) THEN
        RAISE EXCEPTION 'account % has postings; its currency cannot be changed', OLD.code
              USING ERRCODE = 'restrict_violation', CONSTRAINT = 'account_currency_immutable';
    END IF;

    IF NEW.type <> OLD.type
       AND EXISTS (SELECT 1 FROM transactions WHERE account_id = OLD.id) THEN
        RAISE EXCEPTION 'account % has postings; its type cannot be changed', OLD.id
              USING ERRCODE = 'restrict_violation', CONSTRAINT = 'account_type_immutable';
    END IF;

    NEW.created_at := OLD.created_at;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- =============================================================================
-- Triggers
-- =============================================================================

-- Balance enforcement (deferred until COMMIT)
CREATE CONSTRAINT TRIGGER transactions_balanced
    AFTER INSERT ON transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_transactions_check_balance();

CREATE CONSTRAINT TRIGGER journal_entries_balanced
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION trg_journal_entries_check_balance();

-- Header bookkeeping
CREATE TRIGGER journal_entries_stamp_txid
    BEFORE INSERT ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION trg_journal_entries_stamp_txid();

-- Line guards
CREATE TRIGGER transactions_before_insert
    BEFORE INSERT ON transactions
    FOR EACH ROW EXECUTE FUNCTION trg_transactions_before_insert();

-- Immutability: no UPDATE / DELETE / TRUNCATE on posted records
CREATE TRIGGER transactions_no_update_delete
    BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER transactions_no_truncate
    BEFORE TRUNCATE ON transactions
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER journal_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER journal_entries_no_truncate
    BEFORE TRUNCATE ON journal_entries
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

-- Accounts
CREATE TRIGGER accounts_before_update
    BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION trg_accounts_before_update();

CREATE TRIGGER csv_imports_no_update_delete
    BEFORE UPDATE OR DELETE ON csv_imports
    FOR EACH ROW EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER csv_imports_no_truncate
    BEFORE TRUNCATE ON csv_imports
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

-- Users are referenced by the audit trail: disable, never delete.
CREATE TRIGGER users_no_delete
    BEFORE DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER users_no_truncate
    BEFORE TRUNCATE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER currencies_no_truncate
    BEFORE TRUNCATE ON currencies
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

CREATE TRIGGER accounts_no_truncate
    BEFORE TRUNCATE ON accounts
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

-- =============================================================================
-- Privileges
--
-- Run the application as a role that does NOT own these tables, so it cannot
-- DROP, ALTER or DISABLE the triggers above. deploy/setup-db.sh creates an
-- owner role and an application role and applies db/grants.sql.
-- =============================================================================

COMMIT;
