package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Must match components/schemas/CsvFilename in api/openapi.yaml. Disallows
// path separators, so a validated name can never escape the CSV directory.
var csvFilenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.csv$`)

// generalLedgerHeader is the column layout of the General Ledger export.
var generalLedgerHeader = []string{
	"account_code", "account_name", "account_type", "currency",
	"entry_date", "journal_entry_id", "line_id", "line_number",
	"reference", "description", "memo",
	"debit", "credit", "running_balance",
}

// FormatCents renders integer cents as a decimal string ("-1234.05") using
// integer arithmetic only.
func FormatCents(c int64) string {
	sign := ""
	u := uint64(c)
	if c < 0 {
		sign = "-"
		u = uint64(-(c + 1)) + 1 // correct even for math.MinInt64
	}
	return fmt.Sprintf("%s%d.%02d", sign, u/100, u%100)
}

// ParseCents parses a non-negative decimal amount with at most two fraction
// digits ("1234", "1234.5", "1234.56") into cents, using integer arithmetic
// only. An empty string is zero. Thousands separators, signs, currency
// symbols and exponents are rejected.
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 || (hasDot && frac == "") {
		return 0, fmt.Errorf("invalid amount %q: use digits with up to two decimals, e.g. 1234.56", s)
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("invalid amount %q: use digits with up to two decimals, e.g. 1234.56", s)
			}
		}
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > MaxCents/100 {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	c := w*100 + f
	if c > MaxCents {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	return c, nil
}

// csvText neutralises spreadsheet formula injection: text cells starting with
// = + - @ (or tab / CR) are prefixed with a single quote so spreadsheet apps
// treat them as text. Numeric columns are written by FormatCents and are
// never passed through here.
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ExportGeneralLedger writes the General Ledger (every posting, grouped by
// account, with a per-account running balance) to a new UTF-8 CSV file in
// a.CSVDir and returns its metadata.
//
// When from is set, each account's balance before that date is emitted as an
// "Opening balance" row so running balances stay correct. Each account has a
// single currency, so every running balance is in one currency; currency
// limits the export to accounts in that currency.
//
// The file is written to a temporary name, fsynced, then renamed into place,
// so readers on the share never see a partially written export.
func (a *App) ExportGeneralLedger(ctx context.Context, from, to *Date, currency *Currency) (*CsvFile, error) {
	v := &ValidationError{}
	if from != nil && to != nil && to.Before(from.Time) {
		v.add("to", "must not be before from")
	}
	if currency != nil && !currency.Valid() {
		v.add("currency", "must be CAD or USD")
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(a.CSVDir, ".general_ledger-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create export file in %s: %w", a.CSVDir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	bw := bufio.NewWriterSize(tmp, 64*1024)
	rowCount, err := a.writeGeneralLedgerCSV(ctx, bw, from, to, currency)
	if err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, fmt.Errorf("write export: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		return nil, fmt.Errorf("chmod export: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync export: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close export: %w", err)
	}

	now := time.Now().UTC()
	name := fmt.Sprintf("general_ledger_%s_%03d.csv", now.Format("20060102T150405Z"), now.Nanosecond()/1e6)
	final := filepath.Join(a.CSVDir, name)
	if err := os.Rename(tmpName, final); err != nil {
		return nil, fmt.Errorf("finalise export: %w", err)
	}
	committed = true

	info, err := os.Stat(final)
	if err != nil {
		return nil, fmt.Errorf("stat export: %w", err)
	}
	return &CsvFile{Filename: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC(), RowCount: &rowCount}, nil
}

// writeGeneralLedgerCSV streams the ledger from a single SQL statement (one
// consistent snapshot) into w and returns the number of data rows written.
func (a *App) writeGeneralLedgerCSV(ctx context.Context, w *bufio.Writer, from, to *Date, currency *Currency) (int, error) {
	cw := csv.NewWriter(w) // RFC 4180 quoting; "\n" line endings
	if err := cw.Write(generalLedgerHeader); err != nil {
		return 0, err
	}

	rows, err := a.DB.Query(ctx, `
		SELECT a.code, a.name, a.type::text, a.currency, x.kind, x.entry_date,
		       x.journal_entry_id, x.line_id, x.line_number,
		       x.reference, x.description, x.memo, x.amount
		  FROM (
		        -- Opening balances (only when a start date is given)
		        SELECT t.account_id, 0 AS kind, $1::date AS entry_date,
		               NULL::bigint AS journal_entry_id, NULL::bigint AS line_id, 0 AS line_number,
		               '' AS reference, 'Opening balance' AS description, '' AS memo,
		               sum(t.amount)::bigint AS amount
		          FROM transactions t
		          JOIN journal_entries e ON e.id = t.journal_entry_id
		         WHERE $1::date IS NOT NULL AND e.entry_date < $1::date
		         GROUP BY t.account_id
		        HAVING sum(t.amount) <> 0
		        UNION ALL
		        -- Postings in range
		        SELECT t.account_id, 1, e.entry_date,
		               e.id, t.id, t.line_number,
		               e.reference, e.description, t.memo, t.amount
		          FROM transactions t
		          JOIN journal_entries e ON e.id = t.journal_entry_id
		         WHERE ($1::date IS NULL OR e.entry_date >= $1::date)
		           AND ($2::date IS NULL OR e.entry_date <= $2::date)
		       ) x
		  JOIN accounts a ON a.id = x.account_id
		 WHERE ($3::text IS NULL OR a.currency = $3)
		 ORDER BY a.code, x.kind, x.entry_date, x.journal_entry_id, x.line_number`,
		dateArg(from), dateArg(to), currency)
	if err != nil {
		return 0, fmt.Errorf("query general ledger: %w", err)
	}

	var (
		code, name, typ, cur, reference, description, memo string
		kind, lineNumber                                   int
		entryDate                                          Date
		entryID, lineID                                    *int64
		amount, running                                    int64
		currentCode                                        string
		count                                              int
		record                                             = make([]string, len(generalLedgerHeader))
	)
	optID := func(p *int64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatInt(*p, 10)
	}

	_, err = pgx.ForEachRow(rows,
		[]any{&code, &name, &typ, &cur, &kind, &entryDate, &entryID, &lineID, &lineNumber, &reference, &description, &memo, &amount},
		func() error {
			if code != currentCode || count == 0 {
				currentCode, running = code, 0
			}
			running += amount

			debit, credit := "", ""
			lineNo := ""
			if kind == 1 {
				d, c := splitAmount(amount)
				if d > 0 {
					debit = FormatCents(d)
				} else {
					credit = FormatCents(c)
				}
				lineNo = strconv.Itoa(lineNumber)
			}

			record[0] = csvText(code)
			record[1] = csvText(name)
			record[2] = typ
			record[3] = cur
			record[4] = entryDate.String()
			record[5] = optID(entryID)
			record[6] = optID(lineID)
			record[7] = lineNo
			record[8] = csvText(reference)
			record[9] = csvText(description)
			record[10] = csvText(memo)
			record[11] = debit
			record[12] = credit
			record[13] = FormatCents(running)
			count++
			return cw.Write(record)
		})
	if err != nil {
		return 0, fmt.Errorf("write general ledger: %w", err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return 0, fmt.Errorf("write general ledger: %w", err)
	}
	return count, nil
}

// ListCsvFiles returns the CSV files in the CSV directory, newest first.
func (a *App) ListCsvFiles() ([]CsvFile, error) {
	entries, err := os.ReadDir(a.CSVDir)
	if err != nil {
		return nil, fmt.Errorf("read CSV directory: %w", err)
	}
	files := []CsvFile{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !csvFilenameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed between ReadDir and Info
		}
		files = append(files, CsvFile{Filename: e.Name(), SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModifiedAt.After(files[j].ModifiedAt) })
	return files, nil
}

// OpenCsvFile opens a CSV file for reading. The name is validated and the
// open is confined to the CSV directory with os.Root, so symlinks or ".."
// cannot escape it.
func (a *App) OpenCsvFile(name string) (*os.File, fs.FileInfo, error) {
	if !csvFilenameRe.MatchString(name) {
		return nil, nil, ErrNotFound
	}
	root, err := os.OpenRoot(a.CSVDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open CSV directory: %w", err)
	}
	defer root.Close()

	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}
