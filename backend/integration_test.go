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
	"mime/multipart"
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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

func createAccount(t *testing.T, h http.Handler, code, name string, typ AccountType, cur Currency) Account {
	t.Helper()
	r := call(t, h, "POST", "/accounts", AccountCreate{Code: code, Name: name, Type: typ, Currency: cur})
	expectStatus(t, r, http.StatusCreated)
	var a Account
	r.decode(t, &a)
	return a
}

func entry(date, desc string, lines ...JournalLineCreate) map[string]any {
	return map[string]any{"entry_date": date, "description": desc, "lines": lines}
}

func dr(code string, cents int64) JournalLineCreate {
	return JournalLineCreate{AccountCode: code, DebitCents: cents}
}

func cr(code string, cents int64) JournalLineCreate {
	return JournalLineCreate{AccountCode: code, CreditCents: cents}
}

func TestLedgerEndToEnd(t *testing.T) {
	app, h := setupTestApp(t)
	ctx := context.Background()

	createAccount(t, h, "1000", "Bank CAD", AccountAsset, CurrencyCAD)
	createAccount(t, h, "1010", "Bank USD", AccountAsset, CurrencyUSD)
	createAccount(t, h, "1900", "FX clearing CAD", AccountAsset, CurrencyCAD)
	createAccount(t, h, "1910", "FX clearing USD", AccountAsset, CurrencyUSD)
	createAccount(t, h, "4000", "Sales", AccountRevenue, CurrencyCAD)
	createAccount(t, h, "6100", "=Supplies", AccountExpense, CurrencyCAD) // formula-looking name
	createAccount(t, h, "9000", "Old account", AccountExpense, CurrencyCAD)

	expectStatus(t, call(t, h, "POST", "/accounts", AccountCreate{Code: "1000", Name: "Dup", Type: AccountAsset, Currency: CurrencyCAD}), http.StatusConflict)
	expectStatus(t, call(t, h, "POST", "/accounts", AccountCreate{Code: "2000", Name: "Euro", Type: AccountAsset, Currency: "EUR"}), http.StatusUnprocessableEntity)
	expectStatus(t, call(t, h, "PATCH", "/accounts/9000", map[string]any{"is_active": false}), http.StatusOK)
	expectStatus(t, call(t, h, "PATCH", "/accounts/9000", map[string]any{"code": "9001"}), http.StatusBadRequest)
	expectStatus(t, call(t, h, "GET", "/accounts/nope", nil), http.StatusNotFound)

	r := call(t, h, "GET", "/currencies", nil)
	expectStatus(t, r, http.StatusOK)
	if !strings.Contains(string(r.Body), `"CAD"`) || !strings.Contains(string(r.Body), `"USD"`) {
		t.Fatalf("currencies: %s", r.Body)
	}

	// --- Balanced entries post successfully -------------------------------
	r = call(t, h, "POST", "/journal-entries", entry("2026-09-15", "Cash sale",
		dr("1000", 150000),
		JournalLineCreate{AccountCode: "4000", CreditCents: 150000, Memo: "Invoice 1 — café ☕"},
	))
	expectStatus(t, r, http.StatusCreated)
	var sale JournalEntry
	r.decode(t, &sale)
	if len(sale.Totals) != 1 || sale.Totals[0] != (CurrencyTotal{CurrencyCAD, 150000}) || len(sale.Lines) != 2 ||
		sale.Lines[1].CreditCents != 150000 || sale.Lines[1].AccountCode != "4000" || sale.Lines[1].Currency != CurrencyCAD {
		t.Fatalf("unexpected entry: %+v", sale)
	}

	r = call(t, h, "POST", "/journal-entries", entry("2026-10-02", "Supplies",
		JournalLineCreate{AccountCode: "6100", DebitCents: 4599, Memo: "+paper"},
		cr("1000", 4599),
	))
	expectStatus(t, r, http.StatusCreated)
	var purchase JournalEntry
	r.decode(t, &purchase)

	// Convert CAD 1,370.00 to USD 1,000.00 through the FX clearing accounts.
	r = call(t, h, "POST", "/journal-entries", entry("2026-10-03", "Buy USD",
		dr("1900", 137000), cr("1000", 137000),
		dr("1010", 100000), cr("1910", 100000),
	))
	expectStatus(t, r, http.StatusCreated)
	var fx JournalEntry
	r.decode(t, &fx)
	if len(fx.Totals) != 2 || fx.Totals[0] != (CurrencyTotal{CurrencyCAD, 137000}) || fx.Totals[1] != (CurrencyTotal{CurrencyUSD, 100000}) {
		t.Fatalf("unexpected fx totals: %+v", fx.Totals)
	}

	entriesBefore, linesBefore := countRows(t, app, "journal_entries"), countRows(t, app, "transactions")

	// --- Rejected entries leave no trace ----------------------------------
	rejected := []struct {
		name string
		body any
		want int
	}{
		{"unbalanced", entry("2026-10-03", "Bad", dr("1000", 100), cr("4000", 99)), http.StatusUnprocessableEntity},
		{"balanced overall but not per currency", entry("2026-10-03", "Bad", dr("1000", 100), cr("1010", 100)), http.StatusUnprocessableEntity},
		{"single line", entry("2026-10-03", "Bad", dr("1000", 100)), http.StatusUnprocessableEntity},
		{"inactive account", entry("2026-10-03", "Bad", dr("9000", 100), cr("1000", 100)), http.StatusUnprocessableEntity},
		{"unknown account", entry("2026-10-03", "Bad", dr("7777", 100), cr("1000", 100)), http.StatusUnprocessableEntity},
		{"account id instead of code", map[string]any{"entry_date": "2026-10-03", "description": "Bad", "lines": []map[string]any{
			{"account_id": 1, "debit_cents": 1, "credit_cents": 0},
			{"account_id": 2, "debit_cents": 0, "credit_cents": 1}}}, http.StatusBadRequest},
		{"float amount", map[string]any{"entry_date": "2026-10-03", "description": "Bad", "lines": []map[string]any{
			{"account_code": "1000", "debit_cents": 1.5, "credit_cents": 0},
			{"account_code": "4000", "debit_cents": 0, "credit_cents": 1.5}}}, http.StatusBadRequest},
		{"unknown field", map[string]any{"entry_date": "2026-10-03", "description": "Bad", "status": "draft",
			"lines": []JournalLineCreate{dr("1000", 1), cr("4000", 1)}}, http.StatusBadRequest},
	}
	for _, tc := range rejected {
		t.Run("reject "+tc.name, func(t *testing.T) {
			expectStatus(t, call(t, h, "POST", "/journal-entries", tc.body), tc.want)
		})
	}
	if e, l := countRows(t, app, "journal_entries"), countRows(t, app, "transactions"); e != entriesBefore || l != linesBefore {
		t.Fatalf("rejected entries changed the ledger: entries %d->%d, lines %d->%d", entriesBefore, e, linesBefore, l)
	}

	// Resolve accounts for the direct-insert tests below (bypassing the API).
	tx0, err := app.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := lockAccounts(ctx, tx0, []string{"1000", "1010", "4000"})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx0.Rollback(ctx)

	// --- Atomicity: a failing line rolls back the header ------------------
	t.Run("rollback on failing line", func(t *testing.T) {
		tx, err := app.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		bad := map[string]postingAccount{"1000": accounts["1000"], "ghost": {ID: 424242, Currency: CurrencyCAD, Active: true}}
		_, err = insertJournalEntry(ctx, tx, JournalEntryCreate{
			EntryDate: mustDate(t, "2026-10-04"), Description: "Partial",
			Lines: []JournalLineCreate{dr("1000", 5), cr("ghost", 5)},
		}, bad, nil, nil)
		if err == nil {
			t.Fatal("expected foreign key failure")
		}
		_ = tx.Rollback(ctx)
		if n := countRows(t, app, "journal_entries"); n != entriesBefore {
			t.Fatalf("header survived a failed line: %d entries", n)
		}
	})

	// --- Database rejects unbalanced commits even without the Go checks ---
	for name, lines := range map[string][]JournalLineCreate{
		"unbalanced":         {dr("1000", 1000), cr("4000", 1)},
		"cross-currency mix": {dr("1000", 1000), cr("1010", 1000)},
	} {
		t.Run("db rejects "+name, func(t *testing.T) {
			tx, err := app.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := insertJournalEntry(ctx, tx, JournalEntryCreate{
				EntryDate: mustDate(t, "2026-10-04"), Description: "Sneaky", Lines: lines,
			}, accounts, nil, nil); err != nil {
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
	}

	// --- Immutability ------------------------------------------------------
	t.Run("posted rows are immutable", func(t *testing.T) {
		for _, stmt := range []string{
			"UPDATE transactions SET amount = amount * 2",
			"DELETE FROM transactions",
			"UPDATE journal_entries SET description = 'changed'",
			"DELETE FROM journal_entries",
			"TRUNCATE transactions CASCADE",
			"UPDATE accounts SET code = 'X1000' WHERE code = '1000'",
			"UPDATE accounts SET currency = 'USD' WHERE code = '1000'",
		} {
			if _, err := app.DB.Exec(ctx, stmt); err == nil {
				t.Errorf("%s: expected an error", stmt)
			}
		}
		// Appending balanced lines to an already-posted entry is also refused.
		_, err := app.DB.Exec(ctx, `INSERT INTO transactions (journal_entry_id, account_id, currency, line_number, amount)
			VALUES ($1, $2, 'CAD', 10, 1), ($1, $3, 'CAD', 11, -1)`, sale.ID, accounts["1000"].ID, accounts["4000"].ID)
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
	want := []TrialBalanceTotal{{CurrencyCAD, 150000, 150000}, {CurrencyUSD, 100000, 100000}}
	if len(tb.Totals) != 2 || tb.Totals[0] != want[0] || tb.Totals[1] != want[1] {
		t.Fatalf("trial balance totals = %+v, want %+v", tb.Totals, want)
	}
	r = call(t, h, "GET", "/reports/trial-balance?as_of=2026-12-31&currency=usd", nil)
	expectStatus(t, r, http.StatusOK)
	r.decode(t, &tb)
	if len(tb.Rows) != 2 || len(tb.Totals) != 1 || tb.Totals[0].Currency != CurrencyUSD {
		t.Fatalf("USD trial balance: %+v", tb)
	}

	r = call(t, h, "GET", "/accounts/1000/ledger?from=2026-10-01", nil)
	expectStatus(t, r, http.StatusOK)
	var led AccountLedger
	r.decode(t, &led)
	if led.Account.Currency != CurrencyCAD || led.OpeningBalanceCents != 150000 || len(led.Lines) != 3 ||
		led.Lines[0].RunningBalanceCents != 150000-4599 || led.ClosingBalanceCents != 150000-137000 {
		t.Fatalf("unexpected cash ledger: %+v", led)
	}

	r = call(t, h, "GET", "/journal-entries?limit=2&account_code=1000", nil)
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
	if exp.RowCount == nil || *exp.RowCount != 10 || len(records) != 11 {
		t.Fatalf("expected 10 data rows, got %d (row_count %v)", len(records)-1, exp.RowCount)
	}
	// Account 1000 rows come first: +1500.00, -45.99, -1370.00, +45.99.
	wantRunning := []string{"1500.00", "1454.01", "84.01", "130.00"}
	for i, rec := range records[1:5] {
		if rec[0] != "1000" || rec[3] != "CAD" || rec[13] != wantRunning[i] {
			t.Errorf("account 1000 row %d = %v, want running balance %s", i, rec, wantRunning[i])
		}
	}
	if !strings.Contains(string(raw), "Invoice 1 — café ☕") {
		t.Error("UTF-8 memo not preserved")
	}
	if !strings.Contains(string(raw), "'=Supplies") || !strings.Contains(string(raw), "'+paper") {
		t.Error("formula-looking text was not neutralised")
	}

	// Date-filtered export emits opening balances; currency filter applies.
	r = call(t, h, "POST", "/exports/general-ledger", map[string]any{"from": "2026-10-01", "currency": "CAD"})
	expectStatus(t, r, http.StatusCreated)
	var exp2 CsvFile
	r.decode(t, &exp2)
	raw2, _ := os.ReadFile(filepath.Join(app.CSVDir, exp2.Filename))
	recs2, _ := csv.NewReader(bytes.NewReader(raw2)).ReadAll()
	if recs2[1][9] != "Opening balance" || recs2[1][13] != "1500.00" || recs2[1][11] != "" {
		t.Fatalf("expected cash opening balance row, got %v", recs2[1])
	}
	if strings.Contains(string(raw2), ",USD,") {
		t.Fatal("CAD-only export contains USD rows")
	}

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
}

func TestImportJournalEntries(t *testing.T) {
	app, h := setupTestApp(t)

	createAccount(t, h, "1000", "Bank CAD", AccountAsset, CurrencyCAD)
	createAccount(t, h, "1010", "Bank USD", AccountAsset, CurrencyUSD)
	createAccount(t, h, "1900", "FX clearing CAD", AccountAsset, CurrencyCAD)
	createAccount(t, h, "1910", "FX clearing USD", AccountAsset, CurrencyUSD)
	createAccount(t, h, "4000", "Sales", AccountRevenue, CurrencyCAD)

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(app.CSVDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	importFile := func(name string) apiResp {
		return call(t, h, "POST", "/imports/journal-entries", ImportRequest{Filename: name})
	}

	const header = "entry_key,entry_date,description,reference,account_code,debit,credit,memo,currency\n"
	write("good.csv", header+
		"S1,2026-10-01,Sale,INV-1,1000,250.00,,,CAD\n"+
		"S1,,,,4000,,250.00,Widgets,\n"+
		"FX1,2026-10-02,Buy USD,,1900,137.00,,,\n"+
		"FX1,2026-10-02,Buy USD,,1000,,137.00,,\n"+
		"FX1,2026-10-02,Buy USD,,1010,100.00,,,USD\n"+
		"FX1,2026-10-02,Buy USD,,1910,,100.00,,USD\n")

	r := importFile("good.csv")
	expectStatus(t, r, http.StatusCreated)
	var res ImportResult
	r.decode(t, &res)
	if res.EntriesImported != 2 || res.LinesImported != 6 || len(res.SHA256) != 64 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var importID *int64
	if err := app.DB.QueryRow(context.Background(), `SELECT min(import_id) FROM journal_entries`).Scan(&importID); err != nil || importID == nil || *importID != res.ImportID {
		t.Fatalf("entries not linked to import: %v %v", importID, err)
	}

	// Same content under another name is refused.
	raw, _ := os.ReadFile(filepath.Join(app.CSVDir, "good.csv"))
	write("copy.csv", string(raw))
	r = importFile("copy.csv")
	expectStatus(t, r, http.StatusConflict)
	if !strings.Contains(string(r.Body), "already imported") {
		t.Fatalf("unexpected body: %s", r.Body)
	}

	entries := countRows(t, app, "journal_entries")
	lines := countRows(t, app, "transactions")

	// A file with one bad entry imports nothing.
	cases := map[string]string{
		"one bad entry": header +
			"A,2026-10-05,Good,,1000,1.00,,,\nA,,,,4000,,1.00,,\n" +
			"B,2026-10-05,Bad,,1000,1.00,,,\nB,,,,4000,,0.99,,\n",
		"unknown account": header + "A,2026-10-05,x,,1000,1.00,,,\nA,,,,7777,,1.00,,\n",
		"per-currency":    header + "A,2026-10-05,x,,1000,1.00,,,\nA,,,,1010,,1.00,,\n",
		"currency mismatch": header +
			"A,2026-10-05,x,,1000,1.00,,,USD\nA,,,,4000,,1.00,,\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			fn := strings.ReplaceAll(name, " ", "_") + ".csv"
			write(fn, content)
			r := importFile(fn)
			expectStatus(t, r, http.StatusUnprocessableEntity)
			if !strings.Contains(string(r.Body), `"row `) && !strings.Contains(string(r.Body), "entry_key") {
				t.Errorf("errors should point at rows: %s", r.Body)
			}
		})
	}
	if countRows(t, app, "journal_entries") != entries || countRows(t, app, "transactions") != lines ||
		countRows(t, app, "csv_imports") != 1 {
		t.Fatal("failed imports changed the ledger")
	}

	expectStatus(t, importFile("missing.csv"), http.StatusNotFound)
	expectStatus(t, importFile("../etc/passwd"), http.StatusUnprocessableEntity)

	// Round-trip sanity: the imported amounts appear in the trial balance.
	r = call(t, h, "GET", "/reports/trial-balance?as_of=2026-12-31", nil)
	expectStatus(t, r, http.StatusOK)
	var tb TrialBalance
	r.decode(t, &tb)
	if len(tb.Totals) != 2 || tb.Totals[0].DebitCents != 25000 || tb.Totals[1].DebitCents != 10000 {
		t.Fatalf("unexpected trial balance after import: %+v", tb.Totals)
	}
}

func upload(t *testing.T, h http.Handler, filename string, content []byte, headers map[string]string) apiResp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return apiResp{Status: rec.Code, Body: rec.Body.Bytes()}
}

func TestUploadAndRequestSafety(t *testing.T) {
	app, h := setupTestApp(t)
	createAccount(t, h, "1000", "Bank CAD", AccountAsset, CurrencyCAD)
	createAccount(t, h, "4000", "Sales", AccountRevenue, CurrencyCAD)

	content := []byte("entry_key,entry_date,description,account_code,debit,credit\nS1,2026-10-01,Sale,1000,5.00,\nS1,,,4000,,5.00\n")

	// Upload, then import the uploaded file.
	r := upload(t, h, "sales.csv", content, nil)
	expectStatus(t, r, http.StatusCreated)
	var f CsvFile
	r.decode(t, &f)
	if f.Filename != "sales.csv" || f.SizeBytes != int64(len(content)) {
		t.Fatalf("unexpected upload result: %+v", f)
	}
	got, _ := os.ReadFile(filepath.Join(app.CSVDir, "sales.csv"))
	if !bytes.Equal(got, content) {
		t.Fatal("stored file differs from upload")
	}
	expectStatus(t, call(t, h, "POST", "/imports/journal-entries", ImportRequest{Filename: "sales.csv"}), http.StatusCreated)

	// Never overwrite; reject bad names, binary content, empty and huge files.
	expectStatus(t, upload(t, h, "sales.csv", []byte("x"), nil), http.StatusConflict)
	expectStatus(t, upload(t, h, "evil.sh", content, nil), http.StatusUnprocessableEntity)
	expectStatus(t, upload(t, h, ".hidden.csv", content, nil), http.StatusUnprocessableEntity)
	// Path components are stripped: the file lands inside the CSV directory.
	expectStatus(t, upload(t, h, "../../escape.csv", content, nil), http.StatusCreated)
	if _, err := os.Stat(filepath.Join(app.CSVDir, "escape.csv")); err != nil {
		t.Fatal("expected escape.csv inside the CSV directory")
	}
	expectStatus(t, upload(t, h, "bin.csv", []byte{0xff, 0xfe, 0x00, 0x01}, nil), http.StatusUnprocessableEntity)
	expectStatus(t, upload(t, h, "empty.csv", nil, nil), http.StatusUnprocessableEntity)
	expectStatus(t, upload(t, h, "huge.csv", bytes.Repeat([]byte("a"), maxUploadBytes+1), nil), http.StatusRequestEntityTooLarge)
	if _, err := os.Stat(filepath.Join(filepath.Dir(app.CSVDir), "escape.csv")); err == nil {
		t.Fatal("upload escaped the CSV directory")
	}
	tmps, _ := filepath.Glob(filepath.Join(app.CSVDir, ".*.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}

	// Cross-site browser requests are refused.
	expectStatus(t, upload(t, h, "csrf.csv", content, map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden)
	expectStatus(t, upload(t, h, "csrf2.csv", content, map[string]string{"Origin": "http://evil.example"}), http.StatusForbidden)
	expectStatus(t, upload(t, h, "same.csv", content, map[string]string{"Sec-Fetch-Site": "same-origin"}), http.StatusCreated)

	// JSON endpoints require application/json (a text/plain form post is a
	// CORS "simple request" a malicious page could send).
	req := httptest.NewRequest("POST", "/api/v1/accounts", strings.NewReader(`{"code":"5000","name":"X","type":"expense","currency":"CAD"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectStatus(t, apiResp{Status: rec.Code, Body: rec.Body.Bytes()}, http.StatusUnsupportedMediaType)

	// Non-API paths serve the frontend (or its placeholder); unknown API paths are JSON 404s.
	for path, want := range map[string]int{"/": 200, "/journal/new": 200, "/api/v2/nope": 404, "/api/v1/nope": 404} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if strings.HasPrefix(path, "/api/") && !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s: expected JSON error, got %s", path, rec.Body.String())
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s: missing security headers", path)
		}
	}
}
