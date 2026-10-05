// Command ledger is a lightweight double-entry accounting server.
//
// It is built as a single static binary (CGO_ENABLED=0) and talks to
// PostgreSQL through the pure-Go pgx driver. The HTTP API is defined in
// api/openapi.yaml; the types in this file mirror its schemas.
//
// Configuration (environment variables):
//
//	DATABASE_URL  PostgreSQL connection string (required)
//	HTTP_ADDR     listen address               (default ":8080")
//	CSV_DIR       CSV import/export directory  (default "/app/csv_data")
//	DB_MAX_CONNS  connection pool size         (default 4)
package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Configuration and startup
// ---------------------------------------------------------------------------

type Config struct {
	DatabaseURL string
	HTTPAddr    string
	CSVDir      string
	DBMaxConns  int32
}

func loadConfig() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    envOr("HTTP_ADDR", ":8080"),
		CSVDir:      envOr("CSV_DIR", "/app/csv_data"),
		DBMaxConns:  4,
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		cfg.DBMaxConns = int32(n)
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// connectDB opens a small connection pool sized for a low-resource container
// and verifies connectivity.
func connectDB(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pcfg.MaxConns = cfg.DBMaxConns
	pcfg.MinConns = 0
	pcfg.MaxConnIdleTime = 5 * time.Minute
	pcfg.MaxConnLifetime = time.Hour
	pcfg.HealthCheckPeriod = time.Minute
	// Store and compare timestamps in UTC regardless of server defaults.
	pcfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := os.MkdirAll(cfg.CSVDir, 0o750); err != nil {
		slog.Warn("CSV directory is not available; exports will fail", "dir", cfg.CSVDir, "err", err)
	}

	app := &App{DB: pool, CSVDir: cfg.CSVDir}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute, // large CSV downloads
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "csv_dir", cfg.CSVDir)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

// App holds shared dependencies for handlers and ledger operations.
type App struct {
	DB     *pgxpool.Pool
	CSVDir string
}

// ---------------------------------------------------------------------------
// Models (mirror components/schemas in api/openapi.yaml)
//
// All money is int64 cents. Debits and credits are separate non-negative
// fields at the API boundary; in the database a line is one signed amount
// (debit > 0, credit < 0).
//
// Accounts are identified by their code in the API; database ids stay
// internal. Every account has one currency and a journal entry must balance
// within each currency.
// ---------------------------------------------------------------------------

// MaxCents is the largest amount accepted at the API boundary (2^53 - 1), so
// that every value is exact when parsed by JavaScript.
const MaxCents int64 = 1<<53 - 1

type AccountType string

const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountEquity    AccountType = "equity"
	AccountRevenue   AccountType = "revenue"
	AccountExpense   AccountType = "expense"
)

func (t AccountType) Valid() bool {
	switch t {
	case AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense:
		return true
	}
	return false
}

type Currency string

const (
	CurrencyCAD Currency = "CAD"
	CurrencyUSD Currency = "USD"
)

func (c Currency) Valid() bool { return c == CurrencyCAD || c == CurrencyUSD }

type CurrencyInfo struct {
	Code       Currency `json:"code"`
	Name       string   `json:"name"`
	MinorUnits int      `json:"minor_units"`
}

type Account struct {
	ID           int64       `json:"-"`
	Code         string      `json:"code"`
	Name         string      `json:"name"`
	Type         AccountType `json:"type"`
	Currency     Currency    `json:"currency"`
	Description  string      `json:"description"`
	IsActive     bool        `json:"is_active"`
	BalanceCents int64       `json:"balance_cents"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type AccountCreate struct {
	Code        string      `json:"code"`
	Name        string      `json:"name"`
	Type        AccountType `json:"type"`
	Currency    Currency    `json:"currency"`
	Description string      `json:"description"`
}

type AccountUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	IsActive    *bool   `json:"is_active,omitempty"`
}

type JournalLineCreate struct {
	AccountCode string `json:"account_code"`
	DebitCents  int64  `json:"debit_cents"`
	CreditCents int64  `json:"credit_cents"`
	Memo        string `json:"memo"`
}

type JournalEntryCreate struct {
	EntryDate   Date                `json:"entry_date"`
	Description string              `json:"description"`
	Reference   string              `json:"reference"`
	Lines       []JournalLineCreate `json:"lines"`
}

type JournalLine struct {
	ID          int64    `json:"id"`
	LineNumber  int      `json:"line_number"`
	AccountCode string   `json:"account_code"`
	AccountName string   `json:"account_name"`
	Currency    Currency `json:"currency"`
	DebitCents  int64    `json:"debit_cents"`
	CreditCents int64    `json:"credit_cents"`
	Memo        string   `json:"memo"`
}

// CurrencyTotal is the total debits (= total credits) of an entry in one currency.
type CurrencyTotal struct {
	Currency    Currency `json:"currency"`
	AmountCents int64    `json:"amount_cents"`
}

type JournalEntry struct {
	ID           int64           `json:"id"`
	EntryDate    Date            `json:"entry_date"`
	Description  string          `json:"description"`
	Reference    string          `json:"reference"`
	ReversesID   *int64          `json:"reverses_id"`
	ReversedByID *int64          `json:"reversed_by_id"`
	ImportID     *int64          `json:"import_id"`
	PostedAt     time.Time       `json:"posted_at"`
	Totals       []CurrencyTotal `json:"totals"`
	Lines        []JournalLine   `json:"lines"`
}

type ReverseRequest struct {
	EntryDate   *Date  `json:"entry_date,omitempty"`
	Description string `json:"description,omitempty"`
}

type AccountLedgerLine struct {
	JournalEntryID      int64  `json:"journal_entry_id"`
	LineID              int64  `json:"line_id"`
	EntryDate           Date   `json:"entry_date"`
	Description         string `json:"description"`
	Reference           string `json:"reference"`
	Memo                string `json:"memo"`
	DebitCents          int64  `json:"debit_cents"`
	CreditCents         int64  `json:"credit_cents"`
	RunningBalanceCents int64  `json:"running_balance_cents"`
}

type AccountLedger struct {
	Account             Account             `json:"account"`
	OpeningBalanceCents int64               `json:"opening_balance_cents"`
	ClosingBalanceCents int64               `json:"closing_balance_cents"`
	Lines               []AccountLedgerLine `json:"lines"`
}

type TrialBalanceRow struct {
	AccountCode string      `json:"account_code"`
	AccountName string      `json:"account_name"`
	AccountType AccountType `json:"account_type"`
	Currency    Currency    `json:"currency"`
	DebitCents  int64       `json:"debit_cents"`
	CreditCents int64       `json:"credit_cents"`
}

// TrialBalanceTotal holds the column totals for one currency; debits always
// equal credits.
type TrialBalanceTotal struct {
	Currency    Currency `json:"currency"`
	DebitCents  int64    `json:"debit_cents"`
	CreditCents int64    `json:"credit_cents"`
}

type TrialBalance struct {
	AsOf   Date                `json:"as_of"`
	Rows   []TrialBalanceRow   `json:"rows"`
	Totals []TrialBalanceTotal `json:"totals"`
}

type ExportRequest struct {
	From     *Date     `json:"from,omitempty"`
	To       *Date     `json:"to,omitempty"`
	Currency *Currency `json:"currency,omitempty"`
}

type ImportRequest struct {
	Filename string `json:"filename"`
}

type ImportResult struct {
	ImportID        int64  `json:"import_id"`
	Filename        string `json:"filename"`
	SHA256          string `json:"sha256"`
	EntriesImported int    `json:"entries_imported"`
	LinesImported   int    `json:"lines_imported"`
}

type CsvFile struct {
	Filename   string    `json:"filename"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	RowCount   *int      `json:"row_count,omitempty"`
}

type Health struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

// splitAmount converts a signed database amount into debit/credit columns.
func splitAmount(amount int64) (debit, credit int64) {
	if amount >= 0 {
		return amount, 0
	}
	return 0, -amount // safe: the schema forbids the BIGINT minimum
}

// ---------------------------------------------------------------------------
// Date: a calendar day serialised as "YYYY-MM-DD" (OpenAPI format: date)
// ---------------------------------------------------------------------------

const dateLayout = "2006-01-02"

type Date struct{ time.Time }

func NewDate(t time.Time) Date {
	y, m, d := t.Date()
	return Date{time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q, expected YYYY-MM-DD", s)
	}
	return Date{t}, nil
}

func (d Date) String() string { return d.Format(dateLayout) }

func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.String())
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("date must be a string in YYYY-MM-DD format")
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Scan implements sql.Scanner so pgx can read DATE columns directly.
func (d *Date) Scan(src any) error {
	t, ok := src.(time.Time)
	if !ok {
		return fmt.Errorf("cannot scan %T into Date", src)
	}
	*d = NewDate(t)
	return nil
}

// Value implements driver.Valuer so Date can be used as a query argument.
func (d Date) Value() (driver.Value, error) {
	return d.Time, nil
}
