package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/auth"
	"github.com/wizz666/nestward/internal/cloudflare/cftest"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/publish"
	"github.com/wizz666/nestward/internal/secrets"
)

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("X-Nestward", "1")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLoginProtectsEverything(t *testing.T) {
	s, st := newServer(t)
	dir := t.TempDir()
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	settings := &config.Settings{Store: st, Box: box}
	f := cftest.New("cf-token")
	cf := f.Server()
	defer cf.Close()
	ctx := context.Background()
	settings.Set(ctx, config.CloudflareToken, "cf-token")
	settings.Set(ctx, config.PublishEmails, "alex@example.com")
	s.Auth = &auth.Manager{Store: st, Box: box}
	s.Settings = settings
	s.Publish = &publish.Engine{Settings: settings, Store: st, CloudflareBase: cf.URL}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}

	// Before setup: page loads, data does not.
	if resp, _ := http.Get(ts.URL + "/"); resp.StatusCode != 200 {
		t.Errorf("page: %d", resp.StatusCode)
	}
	for _, p := range []string{"/api/v1/machines", "/api/v1/report", "/api/v1/status", "/api/v1/settings", "/api/v1/publications"} {
		if code, out := c.do("GET", p, nil); code != 401 || out["error"] != "setup_required" {
			t.Errorf("%s before setup: %d %v", p, code, out)
		}
	}
	// Setup with the code from the log.
	if code, out := c.do("POST", "/api/v1/auth/setup/start", map[string]string{"code": "nope"}); code != 400 || out["error"] != "bad_setup_code" {
		t.Errorf("wrong code: %d %v", code, out)
	}
	setup := s.Auth.SetupCode()
	code, out := c.do("POST", "/api/v1/auth/setup/start", map[string]string{"code": setup, "username": "alex"})
	if code != 200 || !strings.HasPrefix(out["qr"].(string), "data:image/png") {
		t.Fatalf("start: %d %v", code, out)
	}
	secret := out["secret"].(string)
	totp, _ := auth.Code(secret, time.Now())
	code, out = c.do("POST", "/api/v1/auth/setup/finish", map[string]string{"code": setup, "username": "alex",
		"password": "ett-bra-lösenord", "totp": totp})
	if code != 200 {
		t.Fatalf("finish: %d %v", code, out)
	}
	if code, _ := c.do("GET", "/api/v1/machines", nil); code != 200 {
		t.Errorf("after setup machines: %d", code)
	}
	_, state := c.do("GET", "/api/v1/auth/state", nil)
	if state["logged_in"] != true || state["username"] != "alex" {
		t.Errorf("state = %v", state)
	}

	// Publish through the API and undo it.
	code, out = c.do("POST", "/api/v1/publications", map[string]any{"name": "glances", "zone_id": "zone1",
		"tunnel_id": "tun1", "target": "http://192.168.1.92:61208", "app": "Glances", "app_id": "glances", "login": "none"})
	if code != 200 || out["hostname"] != "glances.example.com" || out["access"] != true {
		t.Fatalf("publish: %d %v", code, out)
	}
	id := out["id"].(string)
	if code, out := c.do("POST", "/api/v1/publications", map[string]any{"name": "glances", "zone_id": "zone1",
		"tunnel_id": "tun1", "target": "http://192.168.1.92:61208", "login": "none"}); code != 400 || out["error"] != "publish_name_taken" {
		t.Errorf("taken: %d %v", code, out)
	}
	if code, out := c.do("DELETE", "/api/v1/publications/"+id, nil); code != 200 || out["status"] != "removed" {
		t.Errorf("undo: %d %v", code, out)
	}

	// Settings: secrets masked, only allowed names.
	if code, _ := c.do("POST", "/api/v1/settings", map[string]string{"name": "host.address", "value": "x"}); code != 400 {
		t.Errorf("non-UI setting accepted: %d", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/settings", nil)
	resp, _ := c.http.Do(req)
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, e := range list {
		if e["name"] == "cloudflare.token" && strings.Contains(e["value"].(string), "cf-token") {
			t.Error("token shown in settings")
		}
	}

	// A request from another site is refused even with a session.
	req, _ = http.NewRequest("POST", ts.URL+"/api/v1/publications", strings.NewReader("{}"))
	req.Header.Set("X-Nestward", "1")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := c.http.Do(req); resp.StatusCode != 403 {
		t.Errorf("cross-site publish: %d", resp.StatusCode)
	}

	// Logout ends access.
	c.do("POST", "/api/v1/auth/logout", nil)
	if code, out := c.do("GET", "/api/v1/machines", nil); code != 401 || out["error"] != "login_required" {
		t.Errorf("after logout: %d %v", code, out)
	}
}
