package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

func newSettings(t *testing.T) *Settings {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secrets.OpenBox(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Settings{Store: st, Box: box}
}

func TestSecretsAreEncryptedAndMasked(t *testing.T) {
	s := newSettings(t)
	ctx := context.Background()
	if err := s.Set(ctx, NtfyToken, "tk_supersecret"); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s.Store.Setting(ctx, NtfyToken)
	if strings.Contains(raw, "supersecret") {
		t.Errorf("token stored in clear text: %q", raw)
	}
	if got, err := s.Get(ctx, NtfyToken); err != nil || got != "tk_supersecret" {
		t.Errorf("get = %q, %v", got, err)
	}
	list, _ := s.List(ctx)
	for _, e := range list {
		if strings.Contains(e.Value, "supersecret") {
			t.Errorf("%s shows the secret: %q", e.Name, e.Value)
		}
	}
}

func TestDefaultsValidationAndDelete(t *testing.T) {
	s := newSettings(t)
	ctx := context.Background()
	if v, _ := s.Get(ctx, NtfyURL); v != "https://ntfy.sh" {
		t.Errorf("default = %q", v)
	}
	bad := map[string]string{
		NtfyURL:     "ftp://x",
		NtfyTopic:   "a/b",
		AlertsLang:  "de",
		HostAddress: "http://x/",
		"nope":      "x",
	}
	for k, v := range bad {
		if err := s.Set(ctx, k, v); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
	if err := s.Set(ctx, NtfyTopic, "alerts"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, NtfyTopic, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(ctx, NtfyTopic); v != "" {
		t.Errorf("empty value should remove the setting, got %q", v)
	}
}

func TestMaskURL(t *testing.T) {
	cases := map[string]string{
		"https://hc-ping.com/abc-123":     "https://hc-ping.com/…",
		"https://ntfy.sh":                 "https://ntfy.sh",
		"http://vps:8471/ping/secret?x=1": "http://vps:8471/…",
		"not a url":                       "…",
	}
	for in, want := range cases {
		if got := MaskURL(in); got != want {
			t.Errorf("MaskURL(%q) = %q, want %q", in, got, want)
		}
	}
}
