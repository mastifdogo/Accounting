package main

// Integration tests against a real PostgreSQL server.
//
// Set TEST_DATABASE_URL to a role that may CREATE DATABASE, e.g.
//
//	TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres go test ./...
//
// Each run creates a throwaway database, applies ../db/schema.sql and drops it
// afterwards. Without TEST_DATABASE_URL these tests are skipped.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func setupTestApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	dbName := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	cfg := Config{DatabaseURL: u.String(), DBMaxConns: 4}
	pool, err := connectDB(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})

	schema, err := os.ReadFile(filepath.Join("..", "db", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// No arguments => simple protocol, so the multi-statement script runs as-is.
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	app := &App{DB: pool, CSVDir: t.TempDir()}
	return app, app.Routes()
}

type apiResp struct {
	Status int
	Body   []byte
}

func (r apiResp) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

func call(t *testing.T, h http.Handler, method, path string, body any) apiResp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{Status: rec.Code, Body: rec.Body.Bytes()}
}

func expectStatus(t *testing.T, r apiResp, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %d, want %d; body: %s", r.Status, want, r.Body)
	}
}

func countRows(t *testing.T, app *App, table string) int {
	t.Helper()
	var n int
	if err := app.DB.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func createAccount(t *testing.T, h http.Handler, code, name string, typ AccountType) Account {
	t.Helper()
	r := call(t, h, "POST", "/accounts", AccountCreate{Code: code, Name: name, Type: typ})
	expectStatus(t, r, http.StatusCreated)
	var a Account
	r.decode(t, &a)
	return a
}

func entry(date, desc string, lines ...JournalLineCreate) map[string]any {
	return map[string]any{"entry_date": date, "description": desc, "lines": lines}
}

func TestLedgerEndToEnd(t *testing.T) {
	app, h := setupTestApp(t)
	ctx := context.Background()

	cash := createAccount(t, h, "1000", "Cash", AccountAsset)
	sales := createAccount(t, h, "4000", "Sales", AccountRevenue)
	supplies := createAccount(t, h, "6100", "=Supplies", AccountExpense) // formula-looking name
	old := createAccount(t, h, "9000", "Old account", AccountExpense)

	expectStatus(t, call(t, h, "POST", "/accounts", AccountCreate{Code: "1000", Name: "Dup", Type: AccountAsset}), http.StatusConflict)
	expectStatus(t, call(t, h, "PATCH", fmt.Sprintf("/accounts/%d", old.ID), map[string]any{"is_active": false}), http.StatusOK)

	// --- Balanced entries post successfully -------------------------------
	r := call(t, h, "POST", "/journal-entries", entry("2026-09-15", "Cash sale",
		JournalLineCreate{AccountID: cash.ID, DebitCents: 150000},
		JournalLineCreate{AccountID: sales.ID, CreditCents: 150000, Memo: "Invoice 1 — café ☕"},
	))
	expectStatus(t, r, http.StatusCreated)
	var sale JournalEntry
	r.decode(t, &sale)
	if sale.TotalCents != 150000 || len(sale.Lines) != 2 || sale.Lines[1].CreditCents != 150000 {
		t.Fatalf("unexpected entry: %+v", sale)
	}

	r = call(t, h, "POST", "/journal-entries", entry("2026-10-02", "Supplies",
		JournalLineCreate{AccountID: supplies.ID, DebitCents: 4599, Memo: "+paper"},
		JournalLineCreate{AccountID: cash.ID, CreditCents: 4599},
	))
	expectStatus(t, r, http.StatusCreated)
	var purchase JournalEntry
	r.decode(t, &purchase)

	entriesBefore, linesBefore := countRows(t, app, "journal_entries"), countRows(t, app, "transactions")

	// --- Rejected entries leave no trace ----------------------------------
	rejected := []struct {
		name string
		body any
		want int
	}{
		{"unbalanced", entry("2026-10-03", "Bad",
			JournalLineCreate{AccountID: cash.ID, DebitCents: 100},
			JournalLineCreate{AccountID: sales.ID, CreditCents: 99}), http.StatusUnprocessableEntity},
		{"single line", entry("2026-10-03", "Bad",
			JournalLineCreate{AccountID: cash.ID, DebitCents: 100}), http.StatusUnprocessableEntity},
		{"inactive account", entry("2026-10-03", "Bad",
			JournalLineCreate{AccountID: old.ID, DebitCents: 100},
			JournalLineCreate{AccountID: cash.ID, CreditCents: 100}), http.StatusUnprocessableEntity},
		{"unknown account", entry("2026-10-03", "Bad",
			JournalLineCreate{AccountID: 999999, DebitCents: 100},
			JournalLineCreate{AccountID: cash.ID, CreditCents: 100}), http.StatusUnprocessableEntity},
		{"float amount", map[string]any{"entry_date": "2026-10-03", "description": "Bad", "lines": []map[string]any{
			{"account_id": cash.ID, "debit_cents": 1.5, "credit_cents": 0},
			{"account_id": sales.ID, "debit_cents": 0, "credit_cents": 1.5}}}, http.StatusBadRequest},
		{"unknown field", map[string]any{"entry_date": "2026-10-03", "description": "Bad", "status": "draft",
			"lines": []JournalLineCreate{{AccountID: cash.ID, DebitCents: 1}, {AccountID: sales.ID, CreditCents: 1}}}, http.StatusBadRequest},
	}
	for _, tc := range rejected {
		t.Run("reject "+tc.name, func(t *testing.T) {
			expectStatus(t, call(t, h, "POST", "/journal-entries", tc.body), tc.want)
		})
	}
	if e, l := countRows(t, app, "journal_entries"), countRows(t, app, "transactions"); e != entriesBefore || l != linesBefore {
		t.Fatalf("rejected entries changed the ledger: entries %d->%d, lines %d->%d", entriesBefore, e, linesBefore, l)
	}

	// --- Atomicity: a failing line rolls back the header ------------------
	// Bypass Go validation and the account pre-check to prove the database
	// transaction alone rolls back a partially valid entry.
	t.Run("rollback on failing line", func(t *testing.T) {
		tx, err := app.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = insertJournalEntry(ctx, tx, JournalEntryCreate{
			EntryDate: mustDate(t, "2026-10-04"), Description: "Partial",
			Lines: []JournalLineCreate{{AccountID: cash.ID, DebitCents: 5}, {AccountID: 424242, CreditCents: 5}},
		}, nil)
		if err == nil {
			t.Fatal("expected foreign key failure")
		}
		_ = tx.Rollback(ctx)
		if n := countRows(t, app, "journal_entries"); n != entriesBefore {
			t.Fatalf("header survived a failed line: %d entries", n)
		}
	})

	// --- Database rejects unbalanced commits even without the Go checks ---
	t.Run("db rejects unbalanced commit", func(t *testing.T) {
		tx, err := app.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := insertJournalEntry(ctx, tx, JournalEntryCreate{
			EntryDate: mustDate(t, "2026-10-04"), Description: "Sneaky",
			Lines: []JournalLineCreate{{AccountID: cash.ID, DebitCents: 1000}, {AccountID: sales.ID, CreditCents: 1}},
		}, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		err = tx.Commit(ctx)
		if err == nil || !strings.Contains(err.Error(), "unbalanced") {
			t.Fatalf("expected unbalanced commit error, got %v", err)
		}
		if n := countRows(t, app, "journal_entries"); n != entriesBefore {
			t.Fatalf("unbalanced entry was committed")
		}
	})

	// --- Immutability ------------------------------------------------------
	t.Run("posted rows are immutable", func(t *testing.T) {
		for _, stmt := range []string{
			"UPDATE transactions SET amount = amount * 2",
			"DELETE FROM transactions",
			"UPDATE journal_entries SET description = 'changed'",
			"DELETE FROM journal_entries",
			"TRUNCATE transactions CASCADE",
		} {
			if _, err := app.DB.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Errorf("%s: expected immutability error, got %v", stmt, err)
			}
		}
		// Appending balanced lines to an already-posted entry is also refused.
		_, err := app.DB.Exec(ctx, `INSERT INTO transactions (journal_entry_id, account_id, line_number, amount)
			VALUES ($1, $2, 10, 1), ($1, $3, 11, -1)`, sale.ID, cash.ID, sales.ID)
		if err == nil || !strings.Contains(err.Error(), "already posted") {
			t.Errorf("expected 'already posted' error, got %v", err)
		}
	})

	// --- Reversal ----------------------------------------------------------
	r = call(t, h, "POST", fmt.Sprintf("/journal-entries/%d/reverse", purchase.ID), map[string]any{"entry_date": "2026-10-05"})
	expectStatus(t, r, http.StatusCreated)
	var rev JournalEntry
	r.decode(t, &rev)
	if rev.ReversesID == nil || *rev.ReversesID != purchase.ID || rev.Lines[0].CreditCents != 4599 || rev.Lines[1].DebitCents != 4599 {
		t.Fatalf("bad reversal: %+v", rev)
	}
	expectStatus(t, call(t, h, "POST", fmt.Sprintf("/journal-entries/%d/reverse", purchase.ID), nil), http.StatusConflict)
	expectStatus(t, call(t, h, "POST", fmt.Sprintf("/journal-entries/%d/reverse", rev.ID), nil), http.StatusConflict)

	r = call(t, h, "GET", fmt.Sprintf("/journal-entries/%d", purchase.ID), nil)
	expectStatus(t, r, http.StatusOK)
	var reloaded JournalEntry
	r.decode(t, &reloaded)
	if reloaded.ReversedByID == nil || *reloaded.ReversedByID != rev.ID {
		t.Fatalf("reversed_by_id not set: %+v", reloaded)
	}

	// --- Reports -----------------------------------------------------------
	r = call(t, h, "GET", "/reports/trial-balance?as_of=2026-12-31", nil)
	expectStatus(t, r, http.StatusOK)
	var tb TrialBalance
	r.decode(t, &tb)
	if tb.TotalDebitCents != tb.TotalCreditCents || tb.TotalDebitCents != 150000 {
		t.Fatalf("trial balance does not balance: %+v", tb)
	}

	r = call(t, h, "GET", fmt.Sprintf("/accounts/%d/ledger?from=2026-10-01", cash.ID), nil)
	expectStatus(t, r, http.StatusOK)
	var led AccountLedger
	r.decode(t, &led)
	if led.OpeningBalanceCents != 150000 || led.ClosingBalanceCents != 150000 || len(led.Lines) != 2 ||
		led.Lines[0].RunningBalanceCents != 150000-4599 {
		t.Fatalf("unexpected cash ledger: %+v", led)
	}

	r = call(t, h, "GET", "/journal-entries?limit=2", nil)
	expectStatus(t, r, http.StatusOK)
	var page struct{ Data []JournalEntry }
	r.decode(t, &page)
	if len(page.Data) != 2 || page.Data[0].ID != rev.ID {
		t.Fatalf("unexpected list page: %+v", page)
	}

	// --- General Ledger CSV export ----------------------------------------
	r = call(t, h, "POST", "/exports/general-ledger", nil)
	expectStatus(t, r, http.StatusCreated)
	var exp CsvFile
	r.decode(t, &exp)
	raw, err := os.ReadFile(filepath.Join(app.CSVDir, exp.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte("\xEF\xBB\xBF")) {
		t.Fatal("export must be UTF-8 without BOM")
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	if strings.Join(records[0], ",") != strings.Join(generalLedgerHeader, ",") {
		t.Fatalf("header = %v", records[0])
	}
	if exp.RowCount == nil || *exp.RowCount != 6 || len(records) != 7 {
		t.Fatalf("expected 6 data rows, got %d (row_count %v)", len(records)-1, exp.RowCount)
	}
	// Cash rows come first (ordered by account code): +1500.00, -45.99, +45.99.
	cashRows := records[1:4]
	wantRunning := []string{"1500.00", "1454.01", "1500.00"}
	for i, rec := range cashRows {
		if rec[0] != "1000" || rec[12] != wantRunning[i] {
			t.Errorf("cash row %d = %v, want running balance %s", i, rec, wantRunning[i])
		}
	}
	if !strings.Contains(string(raw), "Invoice 1 — café ☕") {
		t.Error("UTF-8 memo not preserved")
	}
	if !strings.Contains(string(raw), "'=Supplies") || !strings.Contains(string(raw), "'+paper") {
		t.Error("formula-looking text was not neutralised")
	}

	// Date-filtered export emits opening balances.
	r = call(t, h, "POST", "/exports/general-ledger", map[string]any{"from": "2026-10-01"})
	expectStatus(t, r, http.StatusCreated)
	var exp2 CsvFile
	r.decode(t, &exp2)
	raw2, _ := os.ReadFile(filepath.Join(app.CSVDir, exp2.Filename))
	recs2, _ := csv.NewReader(bytes.NewReader(raw2)).ReadAll()
	if recs2[1][8] != "Opening balance" || recs2[1][12] != "1500.00" || recs2[1][10] != "" {
		t.Fatalf("expected cash opening balance row, got %v", recs2[1])
	}

	// No temp files left behind.
	tmps, _ := filepath.Glob(filepath.Join(app.CSVDir, ".*.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}

	// --- File listing and download ----------------------------------------
	r = call(t, h, "GET", "/files", nil)
	expectStatus(t, r, http.StatusOK)
	var files struct{ Data []CsvFile }
	r.decode(t, &files)
	if len(files.Data) != 2 {
		t.Fatalf("expected 2 files, got %+v", files)
	}
	r = call(t, h, "GET", "/files/"+exp.Filename, nil)
	expectStatus(t, r, http.StatusOK)
	if !bytes.Equal(r.Body, raw) {
		t.Fatal("downloaded file differs from export")
	}
	for _, bad := range []string{"..%2Fetc%2Fpasswd", "missing.csv", ".hidden.csv", "x.txt"} {
		expectStatus(t, call(t, h, "GET", "/files/"+bad, nil), http.StatusNotFound)
	}
	expectStatus(t, call(t, h, "POST", "/imports/journal-entries", map[string]any{"filename": "x.csv"}), http.StatusNotImplemented)
}
