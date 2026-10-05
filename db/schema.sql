-- =============================================================================
-- Double-entry accounting ledger: PostgreSQL schema (PostgreSQL 13+)
--
-- Invariants enforced IN THE DATABASE (the Go backend validates too, but the
-- database is the final authority):
--
--   1. Every journal entry has at least two transaction lines.
--   2. The signed sum of a journal entry's lines is exactly zero
--      (debits are stored as positive cents, credits as negative cents).
--   3. Money is stored as BIGINT cents. No floating point anywhere.
--   4. Posted journal entries and transaction lines are immutable:
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
-- accounts: the chart of accounts
-- -----------------------------------------------------------------------------
CREATE TABLE accounts (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        TEXT         NOT NULL,
    name        TEXT         NOT NULL,
    type        account_type NOT NULL,
    description TEXT         NOT NULL DEFAULT '',
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT accounts_code_unique   UNIQUE (code),
    CONSTRAINT accounts_code_format   CHECK (code ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$'),
    CONSTRAINT accounts_name_nonempty CHECK (length(btrim(name)) BETWEEN 1 AND 200)
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
    account_id       BIGINT NOT NULL REFERENCES accounts (id)        ON DELETE RESTRICT,
    line_number      INTEGER NOT NULL,
    amount           BIGINT NOT NULL,
    memo             TEXT   NOT NULL DEFAULT '',

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

-- Shared balance check: >= 2 lines and signed sum exactly zero.
-- SUM over BIGINT returns NUMERIC, so it cannot overflow while summing.
CREATE FUNCTION assert_journal_entry_balanced(p_entry_id BIGINT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    v_lines INTEGER;
    v_sum   NUMERIC;
BEGIN
    -- Defensive: nothing to check if the header does not exist.
    IF NOT EXISTS (SELECT 1 FROM journal_entries WHERE id = p_entry_id) THEN
        RETURN;
    END IF;

    SELECT count(*), COALESCE(sum(amount), 0)
      INTO v_lines, v_sum
      FROM transactions
     WHERE journal_entry_id = p_entry_id;

    IF v_lines < 2 THEN
        RAISE EXCEPTION 'journal entry % must have at least 2 transaction lines (has %)', p_entry_id, v_lines
              USING ERRCODE = 'check_violation',
                    CONSTRAINT = 'journal_entry_min_lines';
    END IF;

    IF v_sum <> 0 THEN
        RAISE EXCEPTION 'journal entry % is unbalanced: debits minus credits = % cents', p_entry_id, v_sum
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

-- Accounts may be renamed or deactivated, but their type is fixed once they
-- have postings (changing it would silently rewrite historical reports), and
-- accounts with postings are protected from deletion by the FK as well.
CREATE FUNCTION trg_accounts_before_update() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id THEN
        RAISE EXCEPTION 'account id cannot be changed'
              USING ERRCODE = 'restrict_violation', CONSTRAINT = 'account_id_immutable';
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

CREATE TRIGGER accounts_no_truncate
    BEFORE TRUNCATE ON accounts
    FOR EACH STATEMENT EXECUTE FUNCTION trg_reject_modification();

-- =============================================================================
-- Recommended privileges (defence in depth)
--
-- Run the application as a role that does not own these tables, so it cannot
-- DROP or DISABLE the triggers above. Example (adjust the role name):
--
--   CREATE ROLE ledger_app LOGIN PASSWORD '...';
--   GRANT USAGE ON SCHEMA public TO ledger_app;
--   GRANT SELECT, INSERT, UPDATE ON accounts TO ledger_app;
--   GRANT SELECT, INSERT ON journal_entries, transactions TO ledger_app;
--   REVOKE UPDATE, DELETE, TRUNCATE ON journal_entries, transactions FROM ledger_app;
-- =============================================================================

COMMIT;
