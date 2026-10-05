package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Financial statements. All amounts are in each account's natural sign
// (assets/expenses debit-positive; liabilities/equity/revenue
// credit-positive) and reported per currency: currencies are never added.

type ReportLine struct {
	AccountCode string `json:"account_code"`
	AccountName string `json:"account_name"`
	AmountCents int64  `json:"amount_cents"`
}

type BalanceSheetSection struct {
	Currency    Currency     `json:"currency"`
	Assets      []ReportLine `json:"assets"`
	Liabilities []ReportLine `json:"liabilities"`
	Equity      []ReportLine `json:"equity"`
	// Revenue minus expenses to date, not yet closed to an equity account.
	NetIncomeCents        int64 `json:"net_income_cents"`
	TotalAssetsCents      int64 `json:"total_assets_cents"`
	TotalLiabilitiesCents int64 `json:"total_liabilities_cents"`
	// Equity accounts plus net income.
	TotalEquityCents int64 `json:"total_equity_cents"`
	// Assets = liabilities + equity (always true for a valid ledger).
	Balanced bool `json:"balanced"`
}

type BalanceSheet struct {
	AsOf       Date                  `json:"as_of"`
	Currencies []BalanceSheetSection `json:"currencies"`
}

type IncomeStatementSection struct {
	Currency           Currency     `json:"currency"`
	Revenue            []ReportLine `json:"revenue"`
	Expenses           []ReportLine `json:"expenses"`
	TotalRevenueCents  int64        `json:"total_revenue_cents"`
	TotalExpensesCents int64        `json:"total_expenses_cents"`
	NetIncomeCents     int64        `json:"net_income_cents"`
}

type IncomeStatement struct {
	From       Date                     `json:"from"`
	To         Date                     `json:"to"`
	Currencies []IncomeStatementSection `json:"currencies"`
}

type accountSum struct {
	code, name string
	typ        AccountType
	currency   Currency
	natural    int64
}

// accountSums returns the natural-sign balance of each account with a
// non-zero total over entries dated in [from, to] (from may be nil), ordered
// by currency and code.
func (a *App) accountSums(ctx context.Context, from *Date, to Date, currency *Currency, types []AccountType) ([]accountSum, error) {
	ts := make([]string, len(types))
	for i, t := range types {
		ts[i] = string(t)
	}
	rows, err := a.DB.Query(ctx, `
		SELECT a.code, a.name, a.type, a.currency, sum(t.amount)::bigint
		  FROM accounts a
		  JOIN transactions t    ON t.account_id = a.id
		  JOIN journal_entries e ON e.id = t.journal_entry_id
		 WHERE e.entry_date <= $2
		   AND ($1::date IS NULL OR e.entry_date >= $1)
		   AND ($3::text IS NULL OR a.currency = $3)
		   AND a.type::text = ANY($4)
		 GROUP BY a.id
		HAVING sum(t.amount) <> 0
		 ORDER BY a.currency, a.code`, dateArg(from), to, currency, ts)
	if err != nil {
		return nil, fmt.Errorf("account sums: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accountSum, error) {
		var s accountSum
		var debitPositive int64
		err := r.Scan(&s.code, &s.name, &s.typ, &s.currency, &debitPositive)
		if s.typ == AccountAsset || s.typ == AccountExpense {
			s.natural = debitPositive
		} else {
			s.natural = -debitPositive
		}
		return s, err
	})
}

func (a *App) GetBalanceSheet(ctx context.Context, asOf *Date, currency *Currency) (*BalanceSheet, error) {
	day := NewDate(time.Now().UTC())
	if asOf != nil {
		day = *asOf
	}
	sums, err := a.accountSums(ctx, nil, day, currency,
		[]AccountType{AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense})
	if err != nil {
		return nil, err
	}

	bs := &BalanceSheet{AsOf: day, Currencies: []BalanceSheetSection{}}
	sections := map[Currency]*BalanceSheetSection{}
	var order []Currency
	for _, s := range sums {
		sec := sections[s.currency]
		if sec == nil {
			sec = &BalanceSheetSection{Currency: s.currency, Assets: []ReportLine{}, Liabilities: []ReportLine{}, Equity: []ReportLine{}}
			sections[s.currency] = sec
			order = append(order, s.currency)
		}
		line := ReportLine{AccountCode: s.code, AccountName: s.name, AmountCents: s.natural}
		switch s.typ {
		case AccountAsset:
			sec.Assets = append(sec.Assets, line)
			sec.TotalAssetsCents += s.natural
		case AccountLiability:
			sec.Liabilities = append(sec.Liabilities, line)
			sec.TotalLiabilitiesCents += s.natural
		case AccountEquity:
			sec.Equity = append(sec.Equity, line)
			sec.TotalEquityCents += s.natural
		case AccountRevenue:
			sec.NetIncomeCents += s.natural
		case AccountExpense:
			sec.NetIncomeCents -= s.natural
		}
	}
	for _, c := range order {
		sec := sections[c]
		sec.TotalEquityCents += sec.NetIncomeCents
		sec.Balanced = sec.TotalAssetsCents == sec.TotalLiabilitiesCents+sec.TotalEquityCents
		bs.Currencies = append(bs.Currencies, *sec)
	}
	return bs, nil
}

func (a *App) GetIncomeStatement(ctx context.Context, from, to *Date, currency *Currency) (*IncomeStatement, error) {
	end := NewDate(time.Now().UTC())
	if to != nil {
		end = *to
	}
	start := Date{time.Date(end.Year(), 1, 1, 0, 0, 0, 0, time.UTC)} // default: year to date
	if from != nil {
		start = *from
	}
	if end.Before(start.Time) {
		v := &ValidationError{}
		v.add("to", "must not be before from")
		return nil, v
	}
	sums, err := a.accountSums(ctx, &start, end, currency, []AccountType{AccountRevenue, AccountExpense})
	if err != nil {
		return nil, err
	}

	is := &IncomeStatement{From: start, To: end, Currencies: []IncomeStatementSection{}}
	for _, s := range sums {
		n := len(is.Currencies)
		if n == 0 || is.Currencies[n-1].Currency != s.currency {
			is.Currencies = append(is.Currencies, IncomeStatementSection{Currency: s.currency, Revenue: []ReportLine{}, Expenses: []ReportLine{}})
		}
		sec := &is.Currencies[len(is.Currencies)-1]
		line := ReportLine{AccountCode: s.code, AccountName: s.name, AmountCents: s.natural}
		if s.typ == AccountRevenue {
			sec.Revenue = append(sec.Revenue, line)
			sec.TotalRevenueCents += s.natural
		} else {
			sec.Expenses = append(sec.Expenses, line)
			sec.TotalExpensesCents += s.natural
		}
		sec.NetIncomeCents = sec.TotalRevenueCents - sec.TotalExpensesCents
	}
	return is, nil
}

func (a *App) handleBalanceSheet(w http.ResponseWriter, r *http.Request) {
	asOf, err := queryDate(r, "as_of")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	currency, err := queryCurrency(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	bs, err := a.GetBalanceSheet(r.Context(), asOf, currency)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (a *App) handleIncomeStatement(w http.ResponseWriter, r *http.Request) {
	from, to, err := queryDateRange(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	currency, err := queryCurrency(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	is, err := a.GetIncomeStatement(r.Context(), from, to, currency)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, is)
}
