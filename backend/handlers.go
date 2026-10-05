package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Routes builds the HTTP router. Paths match api/openapi.yaml under /api/v1.
func (a *App) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Timeout(60 * time.Second))

		r.Get("/health", a.handleHealth)
		r.Get("/currencies", a.handleListCurrencies)

		r.Get("/accounts", a.handleListAccounts)
		r.Post("/accounts", a.handleCreateAccount)
		r.Get("/accounts/{accountCode}", a.handleGetAccount)
		r.Patch("/accounts/{accountCode}", a.handleUpdateAccount)
		r.Get("/accounts/{accountCode}/ledger", a.handleAccountLedger)

		r.Get("/journal-entries", a.handleListJournalEntries)
		r.Post("/journal-entries", a.handleCreateJournalEntry)
		r.Get("/journal-entries/{entryId}", a.handleGetJournalEntry)
		r.Post("/journal-entries/{entryId}/reverse", a.handleReverseJournalEntry)

		r.Get("/reports/trial-balance", a.handleTrialBalance)

		r.Post("/exports/general-ledger", a.handleExportGeneralLedger)
		r.Get("/files", a.handleListFiles)
		r.Get("/files/{filename}", a.handleDownloadFile)
		r.Post("/imports/journal-entries", a.handleImportJournalEntries)

		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed", nil)
		})
	})

	// Step 2: the SvelteKit static build will be served from here.
	return r
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()))
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := a.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, Health{Status: "degraded", Database: "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, Health{Status: "ok", Database: "ok"})
}

func (a *App) handleListCurrencies(w http.ResponseWriter, r *http.Request) {
	cs, err := a.ListCurrencies(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": cs})
}

func (a *App) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	includeInactive, err := queryBool(r, "include_inactive")
	if err != nil {
		writeErr(w, r, err)
		return
	}
	accounts, err := a.ListAccounts(r.Context(), includeInactive)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": accounts})
}

func (a *App) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var in AccountCreate
	if !decodeJSON(w, r, &in, true) {
		return
	}
	acc, err := a.CreateAccount(r.Context(), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, acc)
}

func (a *App) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := a.GetAccount(r.Context(), chi.URLParam(r, "accountCode"))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (a *App) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	var in AccountUpdate
	if !decodeJSON(w, r, &in, true) {
		return
	}
	acc, err := a.UpdateAccount(r.Context(), chi.URLParam(r, "accountCode"), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (a *App) handleAccountLedger(w http.ResponseWriter, r *http.Request) {
	from, to, err := queryDateRange(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	led, err := a.GetAccountLedger(r.Context(), chi.URLParam(r, "accountCode"), from, to)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, led)
}

func (a *App) handleListJournalEntries(w http.ResponseWriter, r *http.Request) {
	from, to, err := queryDateRange(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	f := EntryFilter{From: from, To: to, Limit: 50}
	v := &ValidationError{}
	q := r.URL.Query()
	if s := q.Get("account_code"); s != "" {
		if !accountCodeRe.MatchString(s) {
			v.add("account_code", "is not a valid account code")
		} else {
			f.AccountCode = &s
		}
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err != nil || n < 1 || n > 500 {
			v.add("limit", "must be an integer between 1 and 500")
		} else {
			f.Limit = n
		}
	}
	if s := q.Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err != nil || n < 0 {
			v.add("offset", "must be a non-negative integer")
		} else {
			f.Offset = n
		}
	}
	if err := v.orNil(); err != nil {
		writeErr(w, r, err)
		return
	}

	entries, err := a.ListJournalEntries(r.Context(), f)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries, "limit": f.Limit, "offset": f.Offset})
}

func (a *App) handleCreateJournalEntry(w http.ResponseWriter, r *http.Request) {
	var in JournalEntryCreate
	if !decodeJSON(w, r, &in, true) {
		return
	}
	e, err := a.CreateJournalEntry(r.Context(), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (a *App) handleGetJournalEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "entryId")
	if !ok {
		return
	}
	e, err := a.GetJournalEntry(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *App) handleReverseJournalEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "entryId")
	if !ok {
		return
	}
	var req ReverseRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	e, err := a.ReverseJournalEntry(r.Context(), id, req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (a *App) handleTrialBalance(w http.ResponseWriter, r *http.Request) {
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
	tb, err := a.GetTrialBalance(r.Context(), asOf, currency)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tb)
}

func (a *App) handleExportGeneralLedger(w http.ResponseWriter, r *http.Request) {
	var req ExportRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	f, err := a.ExportGeneralLedger(r.Context(), req.From, req.To, req.Currency)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (a *App) handleListFiles(w http.ResponseWriter, r *http.Request) {
	files, err := a.ListCsvFiles()
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": files})
}

func (a *App) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "filename")
	f, info, err := a.OpenCsvFile(name)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (a *App) handleImportJournalEntries(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if !csvFilenameRe.MatchString(req.Filename) {
		v := &ValidationError{}
		v.add("filename", "must be a .csv file name in the CSV directory")
		writeErr(w, r, v)
		return
	}
	res, err := a.ImportJournalEntries(r.Context(), req.Filename)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

// decodeJSON decodes a JSON body, rejecting unknown fields (the OpenAPI
// schemas set additionalProperties: false). An empty body is accepted when
// required is false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, required bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if errors.Is(err, io.EOF) && !required {
		return true
	}
	if err == nil && dec.More() {
		err = errors.New("unexpected data after JSON object")
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "bad_request", "request body too large", nil)
		} else if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "bad_request", "request body is required", nil)
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), nil)
		}
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "not_found", "not found", nil)
		return 0, false
	}
	return id, true
}

func queryBool(r *http.Request, name string) (bool, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		v := &ValidationError{}
		v.add(name, "must be true or false")
		return false, v
	}
	return b, nil
}

func queryDate(r *http.Request, name string) (*Date, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, nil
	}
	d, err := ParseDate(s)
	if err != nil {
		v := &ValidationError{}
		v.add(name, "%s", err.Error())
		return nil, v
	}
	return &d, nil
}

func queryCurrency(r *http.Request) (*Currency, error) {
	s := r.URL.Query().Get("currency")
	if s == "" {
		return nil, nil
	}
	c := Currency(strings.ToUpper(s))
	if !c.Valid() {
		v := &ValidationError{}
		v.add("currency", "must be CAD or USD")
		return nil, v
	}
	return &c, nil
}

func queryDateRange(r *http.Request) (from, to *Date, err error) {
	if from, err = queryDate(r, "from"); err != nil {
		return nil, nil, err
	}
	if to, err = queryDate(r, "to"); err != nil {
		return nil, nil, err
	}
	return from, to, nil
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

type apiError struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details []FieldError `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string, details []FieldError) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Details: details}})
}

// writeErr maps ledger and PostgreSQL errors to API error responses. Database
// constraint violations (the last line of defence) surface as 409/422 with
// the trigger's message rather than as opaque 500s.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ve *ValidationError
		ce *ConflictError
		pg *pgconn.PgError
	)
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "request validation failed", ve.Details)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found", nil)
	case errors.As(err, &ce):
		writeError(w, http.StatusConflict, "conflict", ce.Message, nil)
	case errors.As(err, &pg):
		switch {
		case pg.Code == "23505": // unique_violation
			writeError(w, http.StatusConflict, "conflict", uniqueMessage(pg), nil)
		case pg.Code == "23001": // restrict_violation (immutability triggers)
			writeError(w, http.StatusConflict, "conflict", pg.Message, nil)
		case strings.HasPrefix(pg.Code, "23"), strings.HasPrefix(pg.Code, "22"):
			// integrity constraint / data exception (e.g. unbalanced entry)
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", pg.Message, nil)
		default:
			internalError(w, r, err)
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "internal_error", "request timed out or was cancelled", nil)
	default:
		internalError(w, r, err)
	}
}

func uniqueMessage(pg *pgconn.PgError) string {
	switch pg.ConstraintName {
	case "accounts_code_unique":
		return "an account with this code already exists"
	case "journal_entries_reversed_once":
		return "this journal entry has already been reversed"
	case "csv_imports_sha256_unique":
		return "this file's content has already been imported"
	}
	return pg.Message
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("internal error", "err", err, "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
}
