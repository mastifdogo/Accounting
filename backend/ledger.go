package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	minLinesPerEntry = 2
	maxLinesPerEntry = 1000
)

// ---------------------------------------------------------------------------
// Errors shared by ledger operations (mapped to HTTP status in handlers.go)
// ---------------------------------------------------------------------------

var ErrNotFound = errors.New("not found")

type FieldError struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ValidationError reports every problem found in a request at once.
type ValidationError struct{ Details []FieldError }

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Details))
	for i, d := range e.Details {
		if d.Field != "" {
			msgs[i] = d.Field + ": " + d.Message
		} else {
			msgs[i] = d.Message
		}
	}
	return "validation failed: " + strings.Join(msgs, "; ")
}

func (e *ValidationError) add(field, format string, args ...any) {
	e.Details = append(e.Details, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (e *ValidationError) orNil() error {
	if len(e.Details) == 0 {
		return nil
	}
	return e
}

// ConflictError signals a request that is well-formed but clashes with
// existing ledger state (e.g. reversing an entry twice).
type ConflictError struct{ Message string }

func (e *ConflictError) Error() string { return e.Message }

// checkText validates a free-text field's length in characters (matching
// PostgreSQL's length()) and rejects bytes PostgreSQL TEXT cannot store.
func checkText(v *ValidationError, field, s string, minLen, maxLen int) {
	if !utf8.ValidString(s) {
		v.add(field, "must be valid UTF-8")
		return
	}
	if strings.ContainsRune(s, 0) {
		v.add(field, "must not contain NUL characters")
		return
	}
	n := utf8.RuneCountInString(s)
	if minLen > 0 && utf8.RuneCountInString(strings.TrimSpace(s)) < minLen {
		v.add(field, "is required")
	} else if n > maxLen {
		v.add(field, "must be at most %d characters", maxLen)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// ValidateJournalEntry checks the double-entry rules in Go so callers get
// precise, per-field errors. The database re-checks the critical invariants
// (balance, line count, immutability) with constraints and triggers.
func ValidateJournalEntry(in *JournalEntryCreate) error {
	v := &ValidationError{}

	if in.EntryDate.IsZero() {
		v.add("entry_date", "is required")
	} else if y := in.EntryDate.Year(); y < 1900 || y > 9999 {
		v.add("entry_date", "year must be between 1900 and 9999")
	}
	checkText(v, "description", in.Description, 1, 500)
	checkText(v, "reference", in.Reference, 0, 100)

	switch n := len(in.Lines); {
	case n < minLinesPerEntry:
		v.add("lines", "a journal entry needs at least %d lines, got %d", minLinesPerEntry, n)
	case n > maxLinesPerEntry:
		v.add("lines", "a journal entry may have at most %d lines, got %d", maxLinesPerEntry, n)
		return v // don't produce thousands of per-line errors
	}

	var totalDebit, totalCredit int64
	overflow := false
	for i, l := range in.Lines {
		f := fmt.Sprintf("lines[%d]", i)
		if l.AccountID <= 0 {
			v.add(f+".account_id", "is required")
		}
		checkText(v, f+".memo", l.Memo, 0, 500)

		switch {
		case l.DebitCents < 0 || l.CreditCents < 0:
			v.add(f, "debit_cents and credit_cents must not be negative")
			continue
		case l.DebitCents > MaxCents || l.CreditCents > MaxCents:
			v.add(f, "amount exceeds the maximum of %d cents", MaxCents)
			continue
		case l.DebitCents > 0 && l.CreditCents > 0:
			v.add(f, "a line must be either a debit or a credit, not both")
			continue
		case l.DebitCents == 0 && l.CreditCents == 0:
			v.add(f, "a line must have a non-zero debit or credit")
			continue
		}

		// Each term is <= MaxCents and the running totals are kept <= MaxCents,
		// so these additions can never overflow int64.
		totalDebit += l.DebitCents
		totalCredit += l.CreditCents
		if totalDebit > MaxCents || totalCredit > MaxCents {
			overflow = true
			break
		}
	}

	if overflow {
		v.add("lines", "entry total exceeds the maximum of %d cents", MaxCents)
	} else if len(v.Details) == 0 && totalDebit != totalCredit {
		v.add("lines", "entry is unbalanced: debits %s, credits %s (difference %s)",
			FormatCents(totalDebit), FormatCents(totalCredit), FormatCents(totalDebit-totalCredit))
	}
	return v.orNil()
}

// ---------------------------------------------------------------------------
// Writing journal entries
// ---------------------------------------------------------------------------

// CreateJournalEntry validates a journal entry and writes the header and all
// of its lines atomically:
//
//	BEGIN
//	  lock referenced accounts (FOR SHARE) and check they exist and are active
//	  INSERT journal_entries ...
//	  INSERT transactions ... (all lines, one statement)
//	COMMIT  <- deferred triggers verify >= 2 lines and sum(amount) = 0 here
//
// Any failure, including the COMMIT-time balance check, rolls everything back.
func (a *App) CreateJournalEntry(ctx context.Context, in JournalEntryCreate) (*JournalEntry, error) {
	if err := ValidateJournalEntry(&in); err != nil {
		return nil, err
	}

	tx, err := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// Rollback is a no-op once Commit has succeeded.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := checkAccountsPostable(ctx, tx, in.Lines); err != nil {
		return nil, err
	}

	id, err := insertJournalEntry(ctx, tx, in, nil)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit journal entry: %w", err)
	}
	return a.GetJournalEntry(ctx, id)
}

// checkAccountsPostable verifies every referenced account exists and is
// active, and takes a share lock so the account cannot be deactivated by a
// concurrent request before this transaction commits.
func checkAccountsPostable(ctx context.Context, tx pgx.Tx, lines []JournalLineCreate) error {
	ids := make([]int64, len(lines))
	for i, l := range lines {
		ids[i] = l.AccountID
	}

	rows, err := tx.Query(ctx,
		`SELECT id, is_active FROM accounts WHERE id = ANY($1) ORDER BY id FOR SHARE`, ids)
	if err != nil {
		return fmt.Errorf("lock accounts: %w", err)
	}
	active := make(map[int64]bool, len(ids))
	var (
		id int64
		ok bool
	)
	if _, err := pgx.ForEachRow(rows, []any{&id, &ok}, func() error {
		active[id] = ok
		return nil
	}); err != nil {
		return fmt.Errorf("lock accounts: %w", err)
	}

	v := &ValidationError{}
	for i, l := range lines {
		isActive, exists := active[l.AccountID]
		f := fmt.Sprintf("lines[%d].account_id", i)
		switch {
		case !exists:
			v.add(f, "account %d does not exist", l.AccountID)
		case !isActive:
			v.add(f, "account %d is inactive", l.AccountID)
		}
	}
	return v.orNil()
}

// insertJournalEntry writes the header and its lines inside tx and returns
// the new entry id. It must only be called inside a transaction.
func insertJournalEntry(ctx context.Context, tx pgx.Tx, in JournalEntryCreate, reversesID *int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_date, description, reference, reverses_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		in.EntryDate, strings.TrimSpace(in.Description), strings.TrimSpace(in.Reference), reversesID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert journal entry: %w", err)
	}

	n := len(in.Lines)
	accountIDs := make([]int64, n)
	lineNumbers := make([]int32, n)
	amounts := make([]int64, n)
	memos := make([]string, n)
	for i, l := range in.Lines {
		accountIDs[i] = l.AccountID
		lineNumbers[i] = int32(i + 1)
		amounts[i] = l.DebitCents - l.CreditCents // debit > 0, credit < 0
		memos[i] = strings.TrimSpace(l.Memo)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO transactions (journal_entry_id, account_id, line_number, amount, memo)
		SELECT $1, l.account_id, l.line_number, l.amount, l.memo
		  FROM unnest($2::bigint[], $3::int[], $4::bigint[], $5::text[])
		       AS l(account_id, line_number, amount, memo)`,
		id, accountIDs, lineNumbers, amounts, memos)
	if err != nil {
		return 0, fmt.Errorf("insert transaction lines: %w", err)
	}
	if tag.RowsAffected() != int64(n) {
		return 0, fmt.Errorf("insert transaction lines: wrote %d of %d lines", tag.RowsAffected(), n)
	}
	return id, nil
}

// ReverseJournalEntry posts a new entry that swaps every debit and credit of
// the original, in one transaction. Reversals cannot themselves be reversed,
// and an entry can be reversed only once (also enforced by a UNIQUE index).
func (a *App) ReverseJournalEntry(ctx context.Context, id int64, req ReverseRequest) (*JournalEntry, error) {
	orig, err := a.GetJournalEntry(ctx, id)
	if err != nil {
		return nil, err
	}
	if orig.ReversesID != nil {
		return nil, &ConflictError{Message: fmt.Sprintf("entry %d is itself a reversal and cannot be reversed", id)}
	}
	if orig.ReversedByID != nil {
		return nil, &ConflictError{Message: fmt.Sprintf("entry %d was already reversed by entry %d", id, *orig.ReversedByID)}
	}

	in := JournalEntryCreate{
		EntryDate:   NewDate(time.Now().UTC()),
		Description: fmt.Sprintf("Reversal of entry #%d", id),
		Reference:   orig.Reference,
		Lines:       make([]JournalLineCreate, len(orig.Lines)),
	}
	if req.EntryDate != nil {
		in.EntryDate = *req.EntryDate
	}
	if strings.TrimSpace(req.Description) != "" {
		in.Description = req.Description
	}
	for i, l := range orig.Lines {
		in.Lines[i] = JournalLineCreate{
			AccountID:   l.AccountID,
			DebitCents:  l.CreditCents,
			CreditCents: l.DebitCents,
			Memo:        l.Memo,
		}
	}
	if err := ValidateJournalEntry(&in); err != nil {
		return nil, err
	}

	tx, err := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := checkAccountsPostable(ctx, tx, in.Lines); err != nil {
		return nil, err
	}
	newID, err := insertJournalEntry(ctx, tx, in, &id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reversal: %w", err)
	}
	return a.GetJournalEntry(ctx, newID)
}

// ---------------------------------------------------------------------------
// Reading journal entries
// ---------------------------------------------------------------------------

const entrySelect = `
	SELECT e.id, e.entry_date, e.description, e.reference, e.reverses_id,
	       r.id AS reversed_by_id, e.posted_at
	  FROM journal_entries e
	  LEFT JOIN journal_entries r ON r.reverses_id = e.id`

func scanEntry(row pgx.Row) (*JournalEntry, error) {
	var e JournalEntry
	err := row.Scan(&e.ID, &e.EntryDate, &e.Description, &e.Reference, &e.ReversesID, &e.ReversedByID, &e.PostedAt)
	if err != nil {
		return nil, err
	}
	e.Lines = []JournalLine{}
	return &e, nil
}

func (a *App) GetJournalEntry(ctx context.Context, id int64) (*JournalEntry, error) {
	e, err := scanEntry(a.DB.QueryRow(ctx, entrySelect+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get journal entry: %w", err)
	}
	if err := a.attachLines(ctx, []*JournalEntry{e}); err != nil {
		return nil, err
	}
	return e, nil
}

type EntryFilter struct {
	From, To  *Date
	AccountID *int64
	Limit     int
	Offset    int
}

func (a *App) ListJournalEntries(ctx context.Context, f EntryFilter) ([]*JournalEntry, error) {
	rows, err := a.DB.Query(ctx, entrySelect+`
		WHERE ($1::date IS NULL OR e.entry_date >= $1)
		  AND ($2::date IS NULL OR e.entry_date <= $2)
		  AND ($3::bigint IS NULL OR EXISTS (
		        SELECT 1 FROM transactions t WHERE t.journal_entry_id = e.id AND t.account_id = $3))
		ORDER BY e.entry_date DESC, e.id DESC
		LIMIT $4 OFFSET $5`,
		dateArg(f.From), dateArg(f.To), f.AccountID, f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("list journal entries: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*JournalEntry, error) { return scanEntry(r) })
	if err != nil {
		return nil, fmt.Errorf("list journal entries: %w", err)
	}
	if err := a.attachLines(ctx, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// attachLines loads lines for all given entries in one query.
func (a *App) attachLines(ctx context.Context, entries []*JournalEntry) error {
	if len(entries) == 0 {
		return nil
	}
	byID := make(map[int64]*JournalEntry, len(entries))
	ids := make([]int64, len(entries))
	for i, e := range entries {
		byID[e.ID] = e
		ids[i] = e.ID
	}

	rows, err := a.DB.Query(ctx, `
		SELECT t.journal_entry_id, t.id, t.line_number, t.account_id, a.code, a.name, t.amount, t.memo
		  FROM transactions t
		  JOIN accounts a ON a.id = t.account_id
		 WHERE t.journal_entry_id = ANY($1)
		 ORDER BY t.journal_entry_id, t.line_number`, ids)
	if err != nil {
		return fmt.Errorf("load lines: %w", err)
	}
	var (
		entryID, amount int64
		l               JournalLine
	)
	_, err = pgx.ForEachRow(rows, []any{&entryID, &l.ID, &l.LineNumber, &l.AccountID, &l.AccountCode, &l.AccountName, &amount, &l.Memo},
		func() error {
			l.DebitCents, l.CreditCents = splitAmount(amount)
			e := byID[entryID]
			e.Lines = append(e.Lines, l)
			e.TotalCents += l.DebitCents
			return nil
		})
	if err != nil {
		return fmt.Errorf("load lines: %w", err)
	}
	return nil
}

// dateArg converts an optional date to a query argument (NULL when nil).
func dateArg(d *Date) any {
	if d == nil {
		return nil
	}
	return *d
}
