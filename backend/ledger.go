package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// maxReportedErrors caps the details returned for one request (e.g. a large
// CSV import full of mistakes).
const maxReportedErrors = 100

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
	if len(e.Details) == maxReportedErrors {
		e.Details = append(e.Details, FieldError{Message: "too many errors; further errors omitted"})
	}
	if len(e.Details) > maxReportedErrors {
		return
	}
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

// fieldNames tells the validators how to name fields in error messages, so
// the same checks can report "lines[1].account_code" for the JSON API and
// "row 7.account_code" for CSV imports.
type fieldNames struct {
	entry func(field string) string
	line  func(i int, field string) string
}

var jsonFieldNames = fieldNames{
	entry: func(f string) string { return f },
	line: func(i int, f string) string {
		if f == "" {
			return fmt.Sprintf("lines[%d]", i)
		}
		return fmt.Sprintf("lines[%d].%s", i, f)
	},
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// ValidateJournalEntry checks the double-entry rules that need no database
// access, so callers get precise, per-field errors. Per-currency balance is
// checked once account currencies are known (checkLinesPostable), and the
// database re-checks the critical invariants with constraints and triggers.
func ValidateJournalEntry(in *JournalEntryCreate) error {
	v := &ValidationError{}
	validateEntry(v, in, jsonFieldNames)
	return v.orNil()
}

func validateEntry(v *ValidationError, in *JournalEntryCreate, fn fieldNames) {
	start := len(v.Details)

	if in.EntryDate.IsZero() {
		v.add(fn.entry("entry_date"), "is required")
	} else if y := in.EntryDate.Year(); y < 1900 || y > 9999 {
		v.add(fn.entry("entry_date"), "year must be between 1900 and 9999")
	}
	checkText(v, fn.entry("description"), in.Description, 1, 500)
	checkText(v, fn.entry("reference"), in.Reference, 0, 100)

	switch n := len(in.Lines); {
	case n < minLinesPerEntry:
		v.add(fn.entry("lines"), "a journal entry needs at least %d lines, got %d", minLinesPerEntry, n)
	case n > maxLinesPerEntry:
		v.add(fn.entry("lines"), "a journal entry may have at most %d lines, got %d", maxLinesPerEntry, n)
		return // don't produce thousands of per-line errors
	}

	var totalDebit, totalCredit int64
	overflow := false
	for i, l := range in.Lines {
		if !accountCodeRe.MatchString(l.AccountCode) {
			if l.AccountCode == "" {
				v.add(fn.line(i, "account_code"), "is required")
			} else {
				v.add(fn.line(i, "account_code"), "%q is not a valid account code", l.AccountCode)
			}
		}
		checkText(v, fn.line(i, "memo"), l.Memo, 0, 500)

		switch {
		case l.DebitCents < 0 || l.CreditCents < 0:
			v.add(fn.line(i, ""), "debit and credit must not be negative")
			continue
		case l.DebitCents > MaxCents || l.CreditCents > MaxCents:
			v.add(fn.line(i, ""), "amount exceeds the maximum of %d cents", MaxCents)
			continue
		case l.DebitCents > 0 && l.CreditCents > 0:
			v.add(fn.line(i, ""), "a line must be either a debit or a credit, not both")
			continue
		case l.DebitCents == 0 && l.CreditCents == 0:
			v.add(fn.line(i, ""), "a line must have a non-zero debit or credit")
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

	// Balancing per currency implies balancing overall, so an overall
	// mismatch can be reported before account currencies are known.
	if overflow {
		v.add(fn.entry("lines"), "entry total exceeds the maximum of %d cents", MaxCents)
	} else if len(v.Details) == start && totalDebit != totalCredit {
		v.add(fn.entry("lines"), "entry is unbalanced: debits %s, credits %s (difference %s)",
			FormatCents(totalDebit), FormatCents(totalCredit), FormatCents(totalDebit-totalCredit))
	}
}

// ---------------------------------------------------------------------------
// Account resolution (codes -> ids, inside the posting transaction)
// ---------------------------------------------------------------------------

type postingAccount struct {
	ID       int64
	Currency Currency
	Active   bool
}

// lockAccounts loads the given account codes and takes a share lock on them,
// so an account cannot be deactivated by a concurrent request before the
// posting transaction commits.
func lockAccounts(ctx context.Context, tx pgx.Tx, codes []string) (map[string]postingAccount, error) {
	rows, err := tx.Query(ctx, `
		SELECT code, id, currency, is_active
		  FROM accounts WHERE code = ANY($1)
		 ORDER BY id FOR SHARE`, codes)
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	accounts := make(map[string]postingAccount, len(codes))
	var (
		code string
		a    postingAccount
	)
	if _, err := pgx.ForEachRow(rows, []any{&code, &a.ID, &a.Currency, &a.Active}, func() error {
		accounts[code] = a
		return nil
	}); err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	return accounts, nil
}

func lineCodes(lines []JournalLineCreate) []string {
	codes := make([]string, len(lines))
	for i, l := range lines {
		codes[i] = l.AccountCode
	}
	return codes
}

// checkLinesPostable verifies every line's account exists and is active, and
// that the entry balances within each currency.
func checkLinesPostable(v *ValidationError, in *JournalEntryCreate, accounts map[string]postingAccount, fn fieldNames) {
	start := len(v.Details)
	sums := map[Currency]int64{} // bounded by MaxCents per side: cannot overflow
	for i, l := range in.Lines {
		acc, ok := accounts[l.AccountCode]
		switch {
		case !ok:
			v.add(fn.line(i, "account_code"), "account %q does not exist", l.AccountCode)
		case !acc.Active:
			v.add(fn.line(i, "account_code"), "account %q is inactive", l.AccountCode)
		default:
			sums[acc.Currency] += l.DebitCents - l.CreditCents
		}
	}
	if len(v.Details) > start {
		return
	}
	for _, c := range sortedCurrencies(sums) {
		if d := sums[c]; d != 0 {
			v.add(fn.entry("lines"), "entry is unbalanced in %s: debits minus credits = %s; "+
				"post cross-currency amounts through an FX clearing account in each currency", c, FormatCents(d))
		}
	}
}

func sortedCurrencies[V any](m map[Currency]V) []Currency {
	cs := make([]Currency, 0, len(m))
	for c := range m {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i] < cs[j] })
	return cs
}

// ---------------------------------------------------------------------------
// Writing journal entries
// ---------------------------------------------------------------------------

// CreateJournalEntry validates a journal entry and writes the header and all
// of its lines atomically:
//
//	BEGIN
//	  lock referenced accounts (FOR SHARE); check existence, activity and
//	  per-currency balance
//	  INSERT journal_entries ...
//	  INSERT transactions ... (all lines, one statement)
//	COMMIT  <- deferred triggers verify >= 2 lines and per-currency zero sum
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

	accounts, err := lockAccounts(ctx, tx, lineCodes(in.Lines))
	if err != nil {
		return nil, err
	}
	v := &ValidationError{}
	checkLinesPostable(v, &in, accounts, jsonFieldNames)
	if err := v.orNil(); err != nil {
		return nil, err
	}

	id, err := insertJournalEntry(ctx, tx, in, accounts, nil, nil)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit journal entry: %w", err)
	}
	return a.GetJournalEntry(ctx, id)
}

// insertJournalEntry writes the header and its lines inside tx and returns
// the new entry id. It must only be called inside a transaction, with every
// line's account present in accounts.
func insertJournalEntry(ctx context.Context, tx pgx.Tx, in JournalEntryCreate, accounts map[string]postingAccount,
	reversesID, importID *int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO journal_entries (entry_date, description, reference, reverses_id, import_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		in.EntryDate, strings.TrimSpace(in.Description), strings.TrimSpace(in.Reference), reversesID, importID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert journal entry: %w", err)
	}

	n := len(in.Lines)
	accountIDs := make([]int64, n)
	currencies := make([]string, n)
	lineNumbers := make([]int32, n)
	amounts := make([]int64, n)
	memos := make([]string, n)
	for i, l := range in.Lines {
		acc, ok := accounts[l.AccountCode]
		if !ok {
			return 0, fmt.Errorf("insert transaction lines: account %q was not resolved", l.AccountCode)
		}
		accountIDs[i] = acc.ID
		currencies[i] = string(acc.Currency)
		lineNumbers[i] = int32(i + 1)
		amounts[i] = l.DebitCents - l.CreditCents // debit > 0, credit < 0
		memos[i] = strings.TrimSpace(l.Memo)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO transactions (journal_entry_id, account_id, currency, line_number, amount, memo)
		SELECT $1, l.account_id, l.currency, l.line_number, l.amount, l.memo
		  FROM unnest($2::bigint[], $3::text[], $4::int[], $5::bigint[], $6::text[])
		       AS l(account_id, currency, line_number, amount, memo)`,
		id, accountIDs, currencies, lineNumbers, amounts, memos)
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
			AccountCode: l.AccountCode,
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

	accounts, err := lockAccounts(ctx, tx, lineCodes(in.Lines))
	if err != nil {
		return nil, err
	}
	v := &ValidationError{}
	checkLinesPostable(v, &in, accounts, jsonFieldNames)
	if err := v.orNil(); err != nil {
		return nil, err
	}
	newID, err := insertJournalEntry(ctx, tx, in, accounts, &id, nil)
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
	       r.id AS reversed_by_id, e.import_id, e.posted_at
	  FROM journal_entries e
	  LEFT JOIN journal_entries r ON r.reverses_id = e.id`

func scanEntry(row pgx.Row) (*JournalEntry, error) {
	var e JournalEntry
	err := row.Scan(&e.ID, &e.EntryDate, &e.Description, &e.Reference, &e.ReversesID, &e.ReversedByID, &e.ImportID, &e.PostedAt)
	if err != nil {
		return nil, err
	}
	e.Lines = []JournalLine{}
	e.Totals = []CurrencyTotal{}
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
	From, To    *Date
	AccountCode *string
	Limit       int
	Offset      int
}

func (a *App) ListJournalEntries(ctx context.Context, f EntryFilter) ([]*JournalEntry, error) {
	rows, err := a.DB.Query(ctx, entrySelect+`
		WHERE ($1::date IS NULL OR e.entry_date >= $1)
		  AND ($2::date IS NULL OR e.entry_date <= $2)
		  AND ($3::text IS NULL OR EXISTS (
		        SELECT 1 FROM transactions t JOIN accounts a ON a.id = t.account_id
		         WHERE t.journal_entry_id = e.id AND a.code = $3))
		ORDER BY e.entry_date DESC, e.id DESC
		LIMIT $4 OFFSET $5`,
		dateArg(f.From), dateArg(f.To), f.AccountCode, f.Limit, f.Offset)
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

// attachLines loads lines for all given entries in one query and computes
// each entry's per-currency totals.
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
		SELECT t.journal_entry_id, t.id, t.line_number, a.code, a.name, t.currency, t.amount, t.memo
		  FROM transactions t
		  JOIN accounts a ON a.id = t.account_id
		 WHERE t.journal_entry_id = ANY($1)
		 ORDER BY t.journal_entry_id, t.line_number`, ids)
	if err != nil {
		return fmt.Errorf("load lines: %w", err)
	}
	totals := make(map[int64]map[Currency]int64, len(entries))
	var (
		entryID, amount int64
		l               JournalLine
	)
	_, err = pgx.ForEachRow(rows, []any{&entryID, &l.ID, &l.LineNumber, &l.AccountCode, &l.AccountName, &l.Currency, &amount, &l.Memo},
		func() error {
			l.DebitCents, l.CreditCents = splitAmount(amount)
			e := byID[entryID]
			e.Lines = append(e.Lines, l)
			if totals[entryID] == nil {
				totals[entryID] = map[Currency]int64{}
			}
			totals[entryID][l.Currency] += l.DebitCents
			return nil
		})
	if err != nil {
		return fmt.Errorf("load lines: %w", err)
	}
	for id, m := range totals {
		for _, c := range sortedCurrencies(m) {
			byID[id].Totals = append(byID[id].Totals, CurrencyTotal{Currency: c, AmountCents: m[c]})
		}
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
