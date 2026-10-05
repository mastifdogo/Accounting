package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie      = "ledger_session"
	sessionIdleTimeout = 8 * time.Hour
	sessionMaxAge      = 7 * 24 * time.Hour
	sessionTouchEvery  = time.Minute
	bcryptCost         = 12
	minPasswordLen     = 10
	maxPasswordBytes   = 72 // bcrypt limit

	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

// Must match the users_username_format CHECK constraint in db/schema.sql.
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var (
	ErrUnauthorized = errors.New("unauthorized")
	// ErrBadCredentials is deliberately vague: it does not reveal whether the
	// username exists or is disabled.
	ErrBadCredentials = errors.New("invalid username or password")
	ErrTooManyLogins  = errors.New("too many failed login attempts; try again later")
)

type User struct {
	ID          int64      `json:"-"`
	Username    string     `json:"username"`
	IsActive    bool       `json:"is_active"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type PasswordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ---------------------------------------------------------------------------
// Request context
// ---------------------------------------------------------------------------

type ctxKey int

const userCtxKey ctxKey = iota

func withUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// currentUser returns the logged-in user, or nil outside an authenticated request.
func currentUser(ctx context.Context) *User {
	u, _ := ctx.Value(userCtxKey).(*User)
	return u
}

// actorID returns the logged-in user's id for audit columns (NULL if none).
func actorID(ctx context.Context) *int64 {
	if u := currentUser(ctx); u != nil {
		return &u.ID
	}
	return nil
}

// ---------------------------------------------------------------------------
// Passwords
// ---------------------------------------------------------------------------

func normalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validatePassword(v *ValidationError, field, pw string) {
	switch {
	case len([]rune(pw)) < minPasswordLen:
		v.add(field, "must be at least %d characters", minPasswordLen)
	case len(pw) > maxPasswordBytes:
		v.add(field, "must be at most %d bytes", maxPasswordBytes)
	}
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// dummyHash is compared against when a username does not exist, so a failed
// login takes the same time whether or not the user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy password for timing"), bcryptCost)

// ---------------------------------------------------------------------------
// Users (also used by the `ledger user` CLI)
// ---------------------------------------------------------------------------

func (a *App) CreateUser(ctx context.Context, username, password string) (*User, error) {
	username = normalizeUsername(username)
	v := &ValidationError{}
	if !usernameRe.MatchString(username) {
		v.add("username", "must be 1-64 characters: lowercase letters, digits, '.', '_' or '-'")
	}
	validatePassword(v, "password", password)
	if err := v.orNil(); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	var u User
	err = a.DB.QueryRow(ctx, `
		INSERT INTO users (username, password_hash) VALUES ($1, $2)
		RETURNING id, username, is_active, created_at, last_login_at`, username, hash,
	).Scan(&u.ID, &u.Username, &u.IsActive, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &u, nil
}

// SetPassword replaces a user's password and ends all of their sessions.
func (a *App) SetPassword(ctx context.Context, username, password string) error {
	v := &ValidationError{}
	validatePassword(v, "password", password)
	if err := v.orNil(); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return a.updateUser(ctx, username, `password_hash = $2`, hash)
}

// SetUserActive enables or disables a user; disabling ends their sessions.
func (a *App) SetUserActive(ctx context.Context, username string, active bool) error {
	return a.updateUser(ctx, username, `is_active = $2`, active)
}

func (a *App) updateUser(ctx context.Context, username, set string, arg any) error {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var id int64
	err = tx.QueryRow(ctx, `UPDATE users SET `+set+` WHERE username = $1 RETURNING id`,
		normalizeUsername(username), arg).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	return tx.Commit(ctx)
}

func (a *App) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := a.DB.Query(ctx, `SELECT id, username, is_active, created_at, last_login_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) {
		var u User
		err := r.Scan(&u.ID, &u.Username, &u.IsActive, &u.CreatedAt, &u.LastLoginAt)
		return u, err
	})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Login checks credentials and creates a session, returning its token.
func (a *App) Login(ctx context.Context, username, password string) (*User, string, error) {
	username = normalizeUsername(username)
	var (
		u    User
		hash string
	)
	err := a.DB.QueryRow(ctx, `
		SELECT id, username, is_active, created_at, last_login_at, password_hash
		  FROM users WHERE username = $1`, username,
	).Scan(&u.ID, &u.Username, &u.IsActive, &u.CreatedAt, &u.LastLoginAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, "", ErrBadCredentials
	}
	if err != nil {
		return nil, "", fmt.Errorf("login: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || !u.IsActive {
		return nil, "", ErrBadCredentials
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Opportunistic cleanup keeps the table small without a background job.
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR last_seen_at < now() - $1::interval`,
		sessionIdleTimeout.String()); err != nil {
		return nil, "", fmt.Errorf("clean sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, now() + $3::interval)`,
		hashToken(token), u.ID, sessionMaxAge.String()); err != nil {
		return nil, "", fmt.Errorf("create session: %w", err)
	}
	if err := tx.QueryRow(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1 RETURNING last_login_at`, u.ID).
		Scan(&u.LastLoginAt); err != nil {
		return nil, "", fmt.Errorf("record login: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return &u, token, nil
}

// SessionUser resolves a session token to an active user, sliding the idle
// timeout forward.
func (a *App) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" || len(token) > 100 {
		return nil, ErrUnauthorized
	}
	var (
		u        User
		lastSeen time.Time
	)
	err := a.DB.QueryRow(ctx, `
		SELECT u.id, u.username, u.is_active, u.created_at, u.last_login_at, s.last_seen_at
		  FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1
		   AND s.expires_at > now()
		   AND s.last_seen_at > now() - $2::interval
		   AND u.is_active`, hashToken(token), sessionIdleTimeout.String(),
	).Scan(&u.ID, &u.Username, &u.IsActive, &u.CreatedAt, &u.LastLoginAt, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}
	if time.Since(lastSeen) > sessionTouchEvery {
		if _, err := a.DB.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE token_hash = $1`, hashToken(token)); err != nil {
			return nil, fmt.Errorf("touch session: %w", err)
		}
	}
	return &u, nil
}

func (a *App) Logout(ctx context.Context, token string) error {
	_, err := a.DB.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// ChangePassword lets a logged-in user change their own password. Their
// other sessions are ended; the current one is kept.
func (a *App) ChangePassword(ctx context.Context, u *User, keepToken string, in PasswordChange) error {
	var hash string
	if err := a.DB.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, u.ID).Scan(&hash); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	v := &ValidationError{}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.CurrentPassword)) != nil {
		v.add("current_password", "is incorrect")
	}
	validatePassword(v, "new_password", in.NewPassword)
	if err := v.orNil(); err != nil {
		return err
	}
	newHash, err := hashPassword(in.NewPassword)
	if err != nil {
		return err
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, newHash); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2`, u.ID, hashToken(keepToken)); err != nil {
		return fmt.Errorf("end other sessions: %w", err)
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Login rate limiting (in memory; per client IP + username)
// ---------------------------------------------------------------------------

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{failures: map[string][]time.Time{}} }

func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	kept := l.failures[key][:0]
	for _, t := range l.failures[key] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
	} else {
		l.failures[key] = kept
	}
	return kept
}

func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, time.Now())) < loginMaxFailures
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.failures[key] = append(l.recent(key, now), now)
	// Bound memory if someone sprays many usernames.
	if len(l.failures) > 10000 {
		for k := range l.failures {
			l.recent(k, now)
		}
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// HTTP: cookie handling, middleware and handlers
// ---------------------------------------------------------------------------

// secureCookie decides whether the session cookie gets the Secure flag:
// "true"/"false" force it; "auto" sets it when the request arrived over TLS
// directly or via a reverse proxy that sets X-Forwarded-Proto.
func (a *App) secureCookie(r *http.Request) bool {
	switch a.CookieSecure {
	case "true":
		return true
	case "false":
		return false
	}
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *App) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// requireAuth rejects requests without a valid session with 401.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.SessionUser(r.Context(), sessionToken(r))
		if err != nil {
			writeErr(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in LoginRequest
	if !decodeJSON(w, r, &in, true) {
		return
	}
	key := clientIP(r) + "|" + normalizeUsername(in.Username)
	if !a.logins.allowed(key) {
		writeErr(w, r, ErrTooManyLogins)
		return
	}
	u, token, err := a.Login(r.Context(), in.Username, in.Password)
	if err != nil {
		if errors.Is(err, ErrBadCredentials) {
			a.logins.fail(key)
		}
		writeErr(w, r, err)
		return
	}
	a.logins.reset(key)
	a.setSessionCookie(w, r, token, int(sessionMaxAge.Seconds()))
	writeJSON(w, http.StatusOK, u)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		if err := a.Logout(r.Context(), token); err != nil {
			writeErr(w, r, err)
			return
		}
	}
	a.setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r.Context()))
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var in PasswordChange
	if !decodeJSON(w, r, &in, true) {
		return
	}
	if err := a.ChangePassword(r.Context(), currentUser(r.Context()), sessionToken(r), in); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
