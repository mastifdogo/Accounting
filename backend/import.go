package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	maxImportBytes = 10 << 20 // 10 MiB
	maxImportRows  = 50000
	maxEntryKeyLen = 100
)

// Import file columns. Header names are case-insensitive; column order is
// free. Rows sharing an entry_key form one journal entry and must be
// contiguous. See /imports/journal-entries in api/openapi.yaml.
var (
	importRequiredColumns = []string{"entry_key", "entry_date", "description", "account_code", "debit", "credit"}
	importOptionalColumns = []string{"reference", "memo", "currency"}
)

type importGroup struct {
	key      string
	firstRow int
	entry    JournalEntryCreate
	rows     []int      // file line number of each journal line
	currency []Currency // optional asserted currency of each line ("" = none)
}

// ImportJournalEntries imports every journal entry in a CSV file from the CSV
// directory in ONE database transaction: either the whole file is posted or
// nothing is. A file whose exact content was imported before is refused.
func (a *App) ImportJournalEntries(ctx context.Context, filename string) (*ImportResult, error) {
	f, info, err := a.OpenCsvFile(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() > maxImportBytes {
		v := &ValidationError{}
		v.add("filename", "file is larger than %d bytes", maxImportBytes)
		return nil, v
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxImportBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	if len(raw) > maxImportBytes {
		v := &ValidationError{}
		v.add("filename", "file is larger than %d bytes", maxImportBytes)
		return nil, v
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	groups, err := parseImportCSV(raw)
	if err != nil {
		return nil, err
	}
	lineCount := 0
	for _, g := range groups {
		lineCount += len(g.entry.Lines)
	}

	tx, err := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Friendly duplicate check; the UNIQUE constraint still guards races.
	var prevName string
	var prevAt time.Time
	err = tx.QueryRow(ctx, `SELECT filename, imported_at FROM csv_imports WHERE sha256 = $1`, digest).Scan(&prevName, &prevAt)
	if err == nil {
		return nil, &ConflictError{Message: fmt.Sprintf("this file's content was already imported as %q on %s",
			prevName, prevAt.UTC().Format(time.RFC3339))}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("check previous imports: %w", err)
	}

	var codes []string
	for _, g := range groups {
		codes = append(codes, lineCodes(g.entry.Lines)...)
	}
	accounts, err := lockAccounts(ctx, tx, codes)
	if err != nil {
		return nil, err
	}

	v := &ValidationError{}
	for _, g := range groups {
		fn := g.fieldNames()
		for i, l := range g.entry.Lines {
			if acc, ok := accounts[l.AccountCode]; ok && g.currency[i] != "" && g.currency[i] != acc.Currency {
				v.add(fn.line(i, "currency"), "account %q is in %s, not %s", l.AccountCode, acc.Currency, g.currency[i])
			}
		}
		checkLinesPostable(v, &g.entry, accounts, fn)
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}

	var importID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO csv_imports (filename, sha256, entry_count, line_count, imported_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		filename, digest, len(groups), lineCount, actorID(ctx)).Scan(&importID)
	if err != nil {
		return nil, fmt.Errorf("record import: %w", err)
	}
	for _, g := range groups {
		if _, err := insertJournalEntry(ctx, tx, g.entry, accounts, nil, &importID); err != nil {
			return nil, fmt.Errorf("entry_key %q: %w", g.key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit import: %w", err)
	}

	return &ImportResult{
		ImportID:        importID,
		Filename:        filename,
		SHA256:          digest,
		EntriesImported: len(groups),
		LinesImported:   lineCount,
	}, nil
}

func (g *importGroup) fieldNames() fieldNames {
	return fieldNames{
		entry: func(f string) string { return fmt.Sprintf("entry_key %s (row %d).%s", g.key, g.firstRow, f) },
		line: func(i int, f string) string {
			if f == "" {
				return fmt.Sprintf("row %d", g.rows[i])
			}
			return fmt.Sprintf("row %d.%s", g.rows[i], f)
		},
	}
}

// parseImportCSV turns the file into journal entries and runs every check
// that needs no database access. All problems are reported together.
func parseImportCSV(raw []byte) ([]*importGroup, error) {
	v := &ValidationError{}
	raw = bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF")) // tolerate Excel's BOM
	if !utf8.Valid(raw) {
		v.add("", "file is not valid UTF-8")
		return nil, v
	}

	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = 0 // every row must have as many fields as the header
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		v.add("", "file is empty")
		return nil, v
	}
	if err != nil {
		v.add("row 1", "%v", err)
		return nil, v
	}

	col := map[string]int{}
	known := map[string]bool{}
	for _, c := range append(append([]string{}, importRequiredColumns...), importOptionalColumns...) {
		known[c] = true
	}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		switch {
		case !known[name]:
			v.add("row 1", "unknown column %q", h)
		case col[name] != 0:
			v.add("row 1", "duplicate column %q", h)
		default:
			col[name] = i + 1 // store 1-based so 0 means "absent"
		}
	}
	for _, c := range importRequiredColumns {
		if col[c] == 0 {
			v.add("row 1", "missing required column %q", c)
		}
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}
	get := func(rec []string, name string) string {
		if i := col[name]; i != 0 {
			return strings.TrimSpace(rec[i-1])
		}
		return ""
	}

	var (
		groups  []*importGroup
		seen    = map[string]bool{}
		current *importGroup
		rows    int
	)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			v.add("", "%v", err) // csv.ParseError includes the line number
			return nil, v
		}
		line, _ := r.FieldPos(0)
		if rows++; rows > maxImportRows {
			v.add("", "file has more than %d data rows", maxImportRows)
			return nil, v
		}
		if strings.Join(rec, "") == "" {
			continue // skip blank lines
		}
		rowField := func(f string) string { return fmt.Sprintf("row %d.%s", line, f) }

		key := get(rec, "entry_key")
		switch {
		case key == "":
			v.add(rowField("entry_key"), "is required")
			continue
		case utf8.RuneCountInString(key) > maxEntryKeyLen:
			v.add(rowField("entry_key"), "must be at most %d characters", maxEntryKeyLen)
			continue
		}

		date := get(rec, "entry_date")
		description := get(rec, "description")
		reference := get(rec, "reference")

		if current == nil || current.key != key {
			if seen[key] {
				v.add(rowField("entry_key"), "rows for entry_key %q must be contiguous", key)
				continue
			}
			seen[key] = true
			d, err := ParseDate(date)
			if err != nil {
				v.add(rowField("entry_date"), "%s", err.Error())
			}
			current = &importGroup{key: key, firstRow: line, entry: JournalEntryCreate{
				EntryDate: d, Description: description, Reference: reference,
			}}
			groups = append(groups, current)
		} else {
			// Later rows may leave header fields blank, but may not contradict them.
			if date != "" && date != current.entry.EntryDate.String() && !current.entry.EntryDate.IsZero() {
				v.add(rowField("entry_date"), "differs from the first row of entry_key %q", key)
			}
			if description != "" && description != current.entry.Description {
				v.add(rowField("description"), "differs from the first row of entry_key %q", key)
			}
			if reference != "" && reference != current.entry.Reference {
				v.add(rowField("reference"), "differs from the first row of entry_key %q", key)
			}
		}

		debit, err := ParseCents(get(rec, "debit"))
		if err != nil {
			v.add(rowField("debit"), "%s", err.Error())
		}
		credit, err := ParseCents(get(rec, "credit"))
		if err != nil {
			v.add(rowField("credit"), "%s", err.Error())
		}
		cur := Currency(strings.ToUpper(get(rec, "currency")))
		if cur != "" && !cur.Valid() {
			v.add(rowField("currency"), "must be CAD or USD")
		}

		current.entry.Lines = append(current.entry.Lines, JournalLineCreate{
			AccountCode: get(rec, "account_code"),
			DebitCents:  debit,
			CreditCents: credit,
			Memo:        get(rec, "memo"),
		})
		current.rows = append(current.rows, line)
		current.currency = append(current.currency, cur)
	}

	if len(groups) == 0 && len(v.Details) == 0 {
		v.add("", "file contains no journal entries")
	}
	for _, g := range groups {
		validateEntry(v, &g.entry, g.fieldNames())
	}
	if err := v.orNil(); err != nil {
		return nil, err
	}
	return groups, nil
}
