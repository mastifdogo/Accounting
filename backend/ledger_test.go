package main

import (
	"math"
	"strings"
	"testing"
)

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFormatCents(t *testing.T) {
	cases := map[int64]string{
		0:             "0.00",
		5:             "0.05",
		100:           "1.00",
		123456:        "1234.56",
		-1:            "-0.01",
		-123456:       "-1234.56",
		math.MaxInt64: "92233720368547758.07",
		math.MinInt64: "-92233720368547758.08",
	}
	for in, want := range cases {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCsvText(t *testing.T) {
	for in, want := range map[string]string{
		"Cash":          "Cash",
		"=SUM(A1:A9)":   "'=SUM(A1:A9)",
		"+1":            "'+1",
		"-cmd":          "'-cmd",
		"@x":            "'@x",
		"":              "",
		"Ünïcødé €":     "Ünïcødé €",
		"\tleading tab": "'\tleading tab",
	} {
		if got := csvText(in); got != want {
			t.Errorf("csvText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateJournalEntry(t *testing.T) {
	valid := func() JournalEntryCreate {
		return JournalEntryCreate{
			EntryDate:   mustDate(t, "2026-10-01"),
			Description: "Sale",
			Lines: []JournalLineCreate{
				{AccountCode: "1000", DebitCents: 10000},
				{AccountCode: "4000", CreditCents: 10000},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*JournalEntryCreate)
		wantErr string // substring; "" means valid
	}{
		{"valid", func(*JournalEntryCreate) {}, ""},
		{"split credit", func(e *JournalEntryCreate) {
			e.Lines = []JournalLineCreate{{AccountCode: "1000", DebitCents: 100}, {AccountCode: "4000", CreditCents: 60}, {AccountCode: "6100", CreditCents: 40}}
		}, ""},
		{"missing date", func(e *JournalEntryCreate) { e.EntryDate = Date{} }, "entry_date: is required"},
		{"blank description", func(e *JournalEntryCreate) { e.Description = "   " }, "description: is required"},
		{"description too long", func(e *JournalEntryCreate) { e.Description = strings.Repeat("x", 501) }, "at most 500"},
		{"NUL in memo", func(e *JournalEntryCreate) { e.Lines[0].Memo = "a\x00b" }, "NUL"},
		{"one line", func(e *JournalEntryCreate) { e.Lines = e.Lines[:1] }, "at least 2 lines"},
		{"no lines", func(e *JournalEntryCreate) { e.Lines = nil }, "at least 2 lines"},
		{"too many lines", func(e *JournalEntryCreate) { e.Lines = make([]JournalLineCreate, 1001) }, "at most 1000"},
		{"unbalanced", func(e *JournalEntryCreate) { e.Lines[1].CreditCents = 9999 }, "unbalanced"},
		{"both sides", func(e *JournalEntryCreate) { e.Lines[0].CreditCents = 1 }, "not both"},
		{"zero line", func(e *JournalEntryCreate) {
			e.Lines = append(e.Lines, JournalLineCreate{AccountCode: "6100"})
		}, "non-zero"},
		{"negative", func(e *JournalEntryCreate) {
			e.Lines[0].DebitCents = -10000
			e.Lines[1].CreditCents = -10000
		}, "must not be negative"},
		{"missing account", func(e *JournalEntryCreate) { e.Lines[0].AccountCode = "" }, "lines[0].account_code: is required"},
		{"bad account code", func(e *JournalEntryCreate) { e.Lines[0].AccountCode = "../x" }, "not a valid account code"},
		{"amount too large", func(e *JournalEntryCreate) {
			e.Lines[0].DebitCents = MaxCents + 1
			e.Lines[1].CreditCents = MaxCents + 1
		}, "exceeds the maximum"},
		{"total too large", func(e *JournalEntryCreate) {
			e.Lines = []JournalLineCreate{
				{AccountCode: "1000", DebitCents: MaxCents}, {AccountCode: "1000", DebitCents: MaxCents},
				{AccountCode: "4000", CreditCents: MaxCents}, {AccountCode: "4000", CreditCents: MaxCents},
			}
		}, "entry total exceeds"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := valid()
			tc.mutate(&e)
			err := ValidateJournalEntry(&e)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDateJSON(t *testing.T) {
	var d Date
	if err := d.UnmarshalJSON([]byte(`"2026-02-30"`)); err == nil {
		t.Fatal("expected invalid calendar date to be rejected")
	}
	if err := d.UnmarshalJSON([]byte(`"2026-02-28"`)); err != nil {
		t.Fatal(err)
	}
	b, _ := d.MarshalJSON()
	if string(b) != `"2026-02-28"` {
		t.Fatalf("got %s", b)
	}
}

func TestParseCents(t *testing.T) {
	ok := map[string]int64{"": 0, "0": 0, "1": 100, "1.5": 150, "1.05": 105, " 1234.56 ": 123456, "0.01": 1, "90071992547409.91": MaxCents}
	for in, want := range ok {
		got, err := ParseCents(in)
		if err != nil || got != want {
			t.Errorf("ParseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"1.234", "-1", "+1", "1,000.00", "$5", "1e3", ".5", "5.", "abc", "1.2.3", "90071992547409.92", "99999999999999999999"} {
		if got, err := ParseCents(in); err == nil {
			t.Errorf("ParseCents(%q) = %d, want error", in, got)
		}
	}
}

func TestParseImportCSV(t *testing.T) {
	good := "\xEF\xBB\xBFEntry_Key,entry_date,description,reference,account_code,debit,credit,memo,currency\n" +
		"A,2026-10-01,Sale,INV-1,1000,100.00,,,CAD\n" +
		"A,,,,4000,,100,\"memo, with comma\",\n" +
		"B,2026-10-02,FX,,1000,13.70,,,\n" +
		"B,2026-10-02,FX,,1900,,13.70,,\n" +
		"B,2026-10-02,FX,,1910,10.00,,,usd\n" +
		"B,2026-10-02,FX,,1010,,10.00,,\n"
	groups, err := parseImportCSV([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || len(groups[0].entry.Lines) != 2 || len(groups[1].entry.Lines) != 4 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	a := groups[0]
	if a.entry.Reference != "INV-1" || a.entry.Lines[1].CreditCents != 10000 || a.entry.Lines[1].Memo != "memo, with comma" || a.rows[1] != 3 {
		t.Fatalf("unexpected entry A: %+v rows %v", a.entry, a.rows)
	}
	if groups[1].currency[2] != CurrencyUSD {
		t.Fatalf("currency not parsed: %v", groups[1].currency)
	}

	bad := map[string]string{
		"missing column": "entry_key,entry_date,description,account_code,debit\nA,2026-10-01,x,1000,1\n",
		"unknown column": "entry_key,entry_date,description,account_code,debit,credit,amount\n",
		"duplicate col":  "entry_key,entry_date,description,account_code,debit,credit,debit\n",
		"empty":          "",
		"no entries":     "entry_key,entry_date,description,account_code,debit,credit\n",
		"unbalanced":     "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.00,\nA,,,4000,,0.99\n",
		"single line":    "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.00,\n",
		"bad amount":     "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.001,\nA,,,4000,,1.001\n",
		"bad date":       "entry_key,entry_date,description,account_code,debit,credit\nA,01/10/2026,x,1000,1,\nA,,,4000,,1\n",
		"conflicting":    "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1,\nA,2026-10-02,,4000,,1\n",
		"non-contiguous": "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1,\nB,2026-10-01,y,1000,1,\nA,,,4000,,1\n",
		"bad currency":   "entry_key,entry_date,description,account_code,debit,credit,currency\nA,2026-10-01,x,1000,1,,EUR\nA,,,4000,,1,\n",
		"ragged row":     "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1\n",
		"invalid utf8":   "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,\xff,1000,1,\n",
	}
	for name, in := range bad {
		if _, err := parseImportCSV([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
