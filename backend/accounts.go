package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Must match the accounts_code_format CHECK constraint in db/schema.sql.
var accountCodeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

const accountSelect = `
	SELECT a.id, a.code, a.name, a.type, a.description, a.is_active,
	       COALESCE((SELECT sum(t.amount) FROM transactions t WHERE t.account_id = a.id), 0)::bigint,
	       a.created_at, a.updated_at
	  FROM accounts a`

func scanAccount(row pgx.Row) (*Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Code, &a.Name, &a.Type, &a.Description, &a.IsActive, &a.BalanceCents, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *App) CreateAccount(ctx context.Context, in AccountCreate) (*Account, error) {
	v := &ValidationError{}
	in.Code = strings.TrimSpace(in.Code)
	if !accountCodeRe.MatchString(in.Code) {
		v.add("code", "must be 1-32 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	checkText(v, "name", in.Name, 1, 200)
	checkText(v, "description", in.Description, 0, 1000)
	if !in.Type.Valid() {
		v.add("type", "must be one of asset, liability, equity, revenue, expense")
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}

	var id int64
	err := a.DB.QueryRow(ctx, `
		INSERT INTO accounts (code, name, type, description)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		in.Code, strings.TrimSpace(in.Name), string(in.Type), strings.TrimSpace(in.Description),
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	return a.GetAccount(ctx, id)
}

func (a *App) GetAccount(ctx context.Context, id int64) (*Account, error) {
	acc, err := scanAccount(a.DB.QueryRow(ctx, accountSelect+` WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

func (a *App) ListAccounts(ctx context.Context, includeInactive bool) ([]*Account, error) {
	rows, err := a.DB.Query(ctx, accountSelect+` WHERE $1 OR a.is_active ORDER BY a.code`, includeInactive)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	accounts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Account, error) { return scanAccount(r) })
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

func (a *App) UpdateAccount(ctx context.Context, id int64, in AccountUpdate) (*Account, error) {
	v := &ValidationError{}
	if in.Name == nil && in.Description == nil && in.IsActive == nil {
		v.add("", "at least one of name, description, is_active is required")
	}
	if in.Name != nil {
		checkText(v, "name", *in.Name, 1, 200)
		*in.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		checkText(v, "description", *in.Description, 0, 1000)
		*in.Description = strings.TrimSpace(*in.Description)
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}

	tag, err := a.DB.Exec(ctx, `
		UPDATE accounts
		   SET name        = COALESCE($2, name),
		       description = COALESCE($3, description),
		       is_active   = COALESCE($4, is_active)
		 WHERE id = $1`, id, in.Name, in.Description, in.IsActive)
	if err != nil {
		return nil, fmt.Errorf("update account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return a.GetAccount(ctx, id)
}

// GetAccountLedger returns an account's postings in [from, to] with an
// opening balance (everything before from) and a running balance per line.
func (a *App) GetAccountLedger(ctx context.Context, id int64, from, to *Date) (*AccountLedger, error) {
	acc, err := a.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}

	// One REPEATABLE READ snapshot so the opening balance and lines agree.
	tx, err := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	led := &AccountLedger{Account: *acc, Lines: []AccountLedgerLine{}}
	if from != nil {
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(t.amount), 0)::bigint
			  FROM transactions t JOIN journal_entries e ON e.id = t.journal_entry_id
			 WHERE t.account_id = $1 AND e.entry_date < $2`, id, *from).Scan(&led.OpeningBalanceCents)
		if err != nil {
			return nil, fmt.Errorf("opening balance: %w", err)
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT e.id, t.id, e.entry_date, e.description, e.reference, t.memo, t.amount
		  FROM transactions t JOIN journal_entries e ON e.id = t.journal_entry_id
		 WHERE t.account_id = $1
		   AND ($2::date IS NULL OR e.entry_date >= $2)
		   AND ($3::date IS NULL OR e.entry_date <= $3)
		 ORDER BY e.entry_date, e.id, t.line_number`, id, dateArg(from), dateArg(to))
	if err != nil {
		return nil, fmt.Errorf("account ledger: %w", err)
	}
	running := led.OpeningBalanceCents
	var (
		l      AccountLedgerLine
		amount int64
	)
	_, err = pgx.ForEachRow(rows, []any{&l.JournalEntryID, &l.LineID, &l.EntryDate, &l.Description, &l.Reference, &l.Memo, &amount},
		func() error {
			running += amount
			l.DebitCents, l.CreditCents = splitAmount(amount)
			l.RunningBalanceCents = running
			led.Lines = append(led.Lines, l)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("account ledger: %w", err)
	}
	led.ClosingBalanceCents = running
	return led, nil
}

// GetTrialBalance lists the balance of every account with postings on or
// before asOf, split into debit and credit columns.
func (a *App) GetTrialBalance(ctx context.Context, asOf *Date) (*TrialBalance, error) {
	day := NewDate(time.Now().UTC())
	if asOf != nil {
		day = *asOf
	}

	rows, err := a.DB.Query(ctx, `
		SELECT a.id, a.code, a.name, a.type, sum(t.amount)::bigint
		  FROM accounts a
		  JOIN transactions t    ON t.account_id = a.id
		  JOIN journal_entries e ON e.id = t.journal_entry_id
		 WHERE e.entry_date <= $1
		 GROUP BY a.id
		 ORDER BY a.code`, day)
	if err != nil {
		return nil, fmt.Errorf("trial balance: %w", err)
	}
	tb := &TrialBalance{AsOf: day, Rows: []TrialBalanceRow{}}
	var (
		r       TrialBalanceRow
		balance int64
	)
	_, err = pgx.ForEachRow(rows, []any{&r.AccountID, &r.AccountCode, &r.AccountName, &r.AccountType, &balance},
		func() error {
			r.DebitCents, r.CreditCents = splitAmount(balance)
			tb.TotalDebitCents += r.DebitCents
			tb.TotalCreditCents += r.CreditCents
			tb.Rows = append(tb.Rows, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("trial balance: %w", err)
	}
	return tb, nil
}
