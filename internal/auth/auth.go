package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

// Errors shown to the user (as translation keys).
var (
	ErrSetupDone    = errors.New("setup_done")
	ErrBadSetupCode = errors.New("bad_setup_code")
	ErrBadLogin     = errors.New("bad_login")
	ErrBadTOTP      = errors.New("bad_totp")
	ErrTooMany      = errors.New("too_many_attempts")
	ErrNoPending    = errors.New("setup_not_started")
)

// SessionLife is how long a login lasts.
const SessionLife = 30 * 24 * time.Hour

// Manager handles the admin account, setup and sessions.
type Manager struct {
	Store *store.Store
	Box   *secrets.Box
	Now   func() time.Time

	mu        sync.Mutex
	setupCode string
	pending   string // TOTP secret shown during setup, not yet confirmed
	fails     map[string][]time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// NeedsSetup reports whether no account exists yet.
func (m *Manager) NeedsSetup(ctx context.Context) (bool, error) {
	_, err := m.Store.Admin(ctx)
	if errors.Is(err, store.ErrNoAdmin) {
		return true, nil
	}
	return false, err
}

// SetupCode returns the one-time code needed to create the account. It
// is printed in Nestward's log, so only someone who can read the server's
// log (its owner) can claim a new installation.
func (m *Manager) SetupCode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setupCode == "" {
		b := make([]byte, 5)
		rand.Read(b)
		s := strings.ToUpper(hex.EncodeToString(b))
		m.setupCode = s[:5] + "-" + s[5:]
	}
	return m.setupCode
}

func (m *Manager) checkCode(code string) bool {
	want := m.SetupCode()
	got := strings.ToUpper(strings.TrimSpace(code))
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// SetupStart checks the setup code and returns a new TOTP secret for the
// authenticator app.
func (m *Manager) SetupStart(ctx context.Context, code, ip string) (string, error) {
	if need, err := m.NeedsSetup(ctx); err != nil {
		return "", err
	} else if !need {
		return "", ErrSetupDone
	}
	if err := m.limit(ip); err != nil {
		return "", err
	}
	if !m.checkCode(code) {
		m.fail(ip)
		return "", ErrBadSetupCode
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.pending = secret
	m.mu.Unlock()
	return secret, nil
}

// SetupFinish creates the account once the user proves the authenticator
// app works by typing a code from it. It returns a session token.
func (m *Manager) SetupFinish(ctx context.Context, code, username, password, totp, ip string) (string, error) {
	if need, err := m.NeedsSetup(ctx); err != nil {
		return "", err
	} else if !need {
		return "", ErrSetupDone
	}
	if err := m.limit(ip); err != nil {
		return "", err
	}
	if !m.checkCode(code) {
		m.fail(ip)
		return "", ErrBadSetupCode
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 {
		return "", errors.New("bad_username")
	}
	if err := CheckStrength(password, username); err != nil {
		return "", err
	}
	m.mu.Lock()
	secret := m.pending
	m.mu.Unlock()
	if secret == "" {
		return "", ErrNoPending
	}
	step, ok := VerifyTOTP(secret, totp, m.now(), 0)
	if !ok {
		return "", ErrBadTOTP
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	sealed, err := m.Box.Seal(secret)
	if err != nil {
		return "", err
	}
	if err := m.Store.CreateAdmin(ctx, store.Admin{Username: username, PassHash: hash, TOTP: sealed}); err != nil {
		return "", err
	}
	if err := m.Store.SetTOTPLast(ctx, step); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.pending, m.setupCode = "", ""
	m.mu.Unlock()
	return m.newSession(ctx, ip)
}

// Login checks the password and the TOTP code and returns a session token.
func (m *Manager) Login(ctx context.Context, username, password, totp, ip string) (string, error) {
	if err := m.limit(ip); err != nil {
		return "", err
	}
	a, err := m.Store.Admin(ctx)
	if errors.Is(err, store.ErrNoAdmin) {
		m.fail(ip)
		return "", ErrBadLogin
	} else if err != nil {
		return "", err
	}
	okUser := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(username)), []byte(a.Username)) == 1
	okPass, err := CheckPassword(password, a.PassHash)
	if err != nil {
		return "", err
	}
	if !okUser || !okPass {
		m.fail(ip)
		return "", ErrBadLogin
	}
	secret, err := m.Box.Open(a.TOTP)
	if err != nil {
		return "", err
	}
	step, ok := VerifyTOTP(secret, totp, m.now(), a.TOTPLast)
	if !ok {
		m.fail(ip)
		return "", ErrBadTOTP
	}
	if err := m.Store.SetTOTPLast(ctx, step); err != nil {
		return "", err
	}
	m.clearFails(ip)
	return m.newSession(ctx, ip)
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (m *Manager) newSession(ctx context.Context, ip string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	_ = m.Store.PurgeSessions(ctx, m.now())
	if err := m.Store.CreateSession(ctx, hashToken(tok), m.now().Add(SessionLife), ip); err != nil {
		return "", err
	}
	return tok, nil
}

// Valid reports whether a session token is valid.
func (m *Manager) Valid(ctx context.Context, tok string) bool {
	if len(tok) != 64 {
		return false
	}
	ok, err := m.Store.SessionValid(ctx, hashToken(tok), m.now())
	return err == nil && ok
}

// Logout ends a session.
func (m *Manager) Logout(ctx context.Context, tok string) error {
	return m.Store.DeleteSession(ctx, hashToken(tok))
}

// Username returns the admin's name.
func (m *Manager) Username(ctx context.Context) string {
	a, err := m.Store.Admin(ctx)
	if err != nil {
		return ""
	}
	return a.Username
}

// Brute-force protection: at most 5 failures per 15 minutes per address,
// and 20 overall.
const (
	failWindow    = 15 * time.Minute
	maxFailsPerIP = 5
	maxFailsAll   = 20
)

func (m *Manager) prune(now time.Time) {
	for k, ts := range m.fails {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < failWindow {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(m.fails, k)
		} else {
			m.fails[k] = keep
		}
	}
}

func (m *Manager) limit(ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fails == nil {
		m.fails = map[string][]time.Time{}
	}
	m.prune(m.now())
	total := 0
	for _, ts := range m.fails {
		total += len(ts)
	}
	if len(m.fails[ip]) >= maxFailsPerIP || total >= maxFailsAll {
		return ErrTooMany
	}
	return nil
}

func (m *Manager) fail(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fails == nil {
		m.fails = map[string][]time.Time{}
	}
	m.fails[ip] = append(m.fails[ip], m.now())
}

func (m *Manager) clearFails(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.fails, ip)
}
