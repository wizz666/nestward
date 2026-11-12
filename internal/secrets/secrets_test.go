package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRoundTripAndKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.key")
	b, err := OpenBox(p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Seal("tk_hemligt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "hemligt") || !IsSealed(s) {
		t.Errorf("sealed value leaks or lacks prefix: %q", s)
	}
	// Same key file again: can still open.
	b2, err := OpenBox(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Open(s); err != nil || got != "tk_hemligt" {
		t.Errorf("open = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v", fi.Mode().Perm())
		}
	}
	// Another key cannot open it.
	other, _ := OpenBox(filepath.Join(t.TempDir(), "other.key"))
	if _, err := other.Open(s); err == nil {
		t.Error("wrong key must fail")
	}
	if _, err := b.Open("plain"); err == nil {
		t.Error("unsealed value must fail")
	}
	// Two seals of the same text differ (random nonce).
	s2, _ := b.Seal("tk_hemligt")
	if s2 == s {
		t.Error("nonce reuse")
	}
}

func TestBadKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "short.key")
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := OpenBox(p); err == nil {
		t.Error("short key must be rejected")
	}
}
