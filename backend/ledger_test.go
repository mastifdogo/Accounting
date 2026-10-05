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
				{AccountID: 1, DebitCents: 10000},
				{AccountID: 2, CreditCents: 10000},
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
			e.Lines = []JournalLineCreate{{AccountID: 1, DebitCents: 100}, {AccountID: 2, CreditCents: 60}, {AccountID: 3, CreditCents: 40}}
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
			e.Lines = append(e.Lines, JournalLineCreate{AccountID: 3})
		}, "non-zero"},
		{"negative", func(e *JournalEntryCreate) {
			e.Lines[0].DebitCents = -10000
			e.Lines[1].CreditCents = -10000
		}, "must not be negative"},
		{"missing account", func(e *JournalEntryCreate) { e.Lines[0].AccountID = 0 }, "account_id: is required"},
		{"amount too large", func(e *JournalEntryCreate) {
			e.Lines[0].DebitCents = MaxCents + 1
			e.Lines[1].CreditCents = MaxCents + 1
		}, "exceeds the maximum"},
		{"total too large", func(e *JournalEntryCreate) {
			e.Lines = []JournalLineCreate{
				{AccountID: 1, DebitCents: MaxCents}, {AccountID: 1, DebitCents: MaxCents},
				{AccountID: 2, CreditCents: MaxCents}, {AccountID: 2, CreditCents: MaxCents},
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
