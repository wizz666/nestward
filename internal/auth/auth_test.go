package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

// RFC 6238 test vector (SHA1, secret "12345678901234567890").
func TestTOTPVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	got, err := totpAt(secret, 59/30)
	if err != nil || got != "287082" {
		t.Errorf("code at 59s = %q, %v; want 287082", got, err)
	}
	got, _ = totpAt(secret, 1111111109/30)
	if got != "081804" {
		t.Errorf("code at 1111111109 = %q, want 081804", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("rätt lösenord 123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h, "lösenord") || !strings.HasPrefix(h, "$argon2id$") {
		t.Errorf("hash = %q", h)
	}
	if ok, _ := CheckPassword("rätt lösenord 123", h); !ok {
		t.Error("right password rejected")
	}
	if ok, _ := CheckPassword("fel", h); ok {
		t.Error("wrong password accepted")
	}
	if CheckStrength("kort", "x") == nil || CheckStrength("alexalexalex", "AlexAlexAlex") == nil {
		t.Error("weak passwords must be refused")
	}
}

func newManager(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	return &Manager{Store: st, Box: box, Now: func() time.Time { return *now }}
}

func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totpAt(secret, Step(at))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSetupAndLogin(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	m := newManager(t, &now)
	ctx := context.Background()

	if need, _ := m.NeedsSetup(ctx); !need {
		t.Fatal("new install needs setup")
	}
	if _, err := m.SetupStart(ctx, "WRONG-CODE", "1.2.3.4"); !errors.Is(err, ErrBadSetupCode) {
		t.Errorf("wrong setup code: %v", err)
	}
	secret, err := m.SetupStart(ctx, strings.ToLower(m.SetupCode()), "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	setup := m.SetupCode()
	if _, err := m.SetupFinish(ctx, setup, "alex", "ett-bra-lösenord", "000000", "1.2.3.4"); !errors.Is(err, ErrBadTOTP) {
		t.Errorf("wrong totp at setup: %v", err)
	}
	tok, err := m.SetupFinish(ctx, setup, "alex", "ett-bra-lösenord", code(t, secret, now), "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Valid(ctx, tok) {
		t.Error("session after setup is not valid")
	}
	if _, err := m.SetupStart(ctx, setup, "1.2.3.4"); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second setup must be refused: %v", err)
	}

	// The same code cannot be used again.
	if _, err := m.Login(ctx, "alex", "ett-bra-lösenord", code(t, secret, now), "5.6.7.8"); !errors.Is(err, ErrBadTOTP) {
		t.Errorf("reused code: %v", err)
	}
	now = now.Add(time.Minute)
	tok2, err := m.Login(ctx, "alex", "ett-bra-lösenord", code(t, secret, now), "5.6.7.8")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := m.Logout(ctx, tok2); err != nil || m.Valid(ctx, tok2) {
		t.Error("logout must end the session")
	}
	if m.Valid(ctx, strings.Repeat("a", 64)) {
		t.Error("made-up token accepted")
	}
	now = now.Add(SessionLife + time.Hour)
	if m.Valid(ctx, tok) {
		t.Error("expired session accepted")
	}
}

func TestBruteForceLimit(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	m := newManager(t, &now)
	ctx := context.Background()
	for i := 0; i < maxFailsPerIP; i++ {
		m.Login(ctx, "x", "y", "123456", "9.9.9.9")
	}
	if _, err := m.Login(ctx, "x", "y", "123456", "9.9.9.9"); !errors.Is(err, ErrTooMany) {
		t.Errorf("after %d failures: %v", maxFailsPerIP, err)
	}
	if _, err := m.SetupStart(ctx, "nope", "9.9.9.9"); !errors.Is(err, ErrTooMany) {
		t.Errorf("setup attempts count too: %v", err)
	}
	now = now.Add(failWindow + time.Second)
	if _, err := m.Login(ctx, "x", "y", "123456", "9.9.9.9"); errors.Is(err, ErrTooMany) {
		t.Error("the limit must expire")
	}
}

func TestQR(t *testing.T) {
	uri := OTPAuthURI("JBSWY3DPEHPK3PXP", "alex")
	if !strings.HasPrefix(uri, "otpauth://totp/Nestward:alex?") || !strings.Contains(uri, "secret=JBSWY3DPEHPK3PXP") {
		t.Errorf("uri = %q", uri)
	}
	img, err := QRDataURI(uri)
	if err != nil || !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Errorf("qr = %.40q, %v", img, err)
	}
}
