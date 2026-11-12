package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/analysis"
	"github.com/wizz666/nestward/internal/catalog"
	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/store"
	"github.com/wizz666/nestward/profiles"
)

func fakeHW(id string) hardware.Report {
	return hardware.Report{
		Hostname: id, MachineID: id,
		OS:     hardware.OSInfo{Kind: hardware.OSLinux, Name: "Debian"},
		CPU:    hardware.CPUInfo{Arch: "amd64", Cores: 8, AVX2: true, FlagsKnown: true},
		Memory: hardware.MemoryInfo{TotalBytes: 32 << 30, AvailableBytes: 20 << 30},
		GPUs:   []hardware.GPU{}, Disks: []hardware.Disk{}, Filesystems: []hardware.Filesystem{{Mount: "/", TotalBytes: 500 << 30, FreeBytes: 300 << 30}},
	}
}

func newServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	profs, err := catalog.Load(profiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		Store: st, Profiles: profs, I18n: i18n.MustLoad(),
		Detect: func(context.Context) hardware.Report { return fakeHW("core") },
		Discover: func(context.Context) discovery.Result {
			return discovery.Result{Available: true, Stats: discovery.Stats{Recognized: 1}}
		},
	}, st
}

func TestReportJSON(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/report?lang=sv", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var r analysis.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Lang != "sv" || len(r.Apps) != 7 || r.Summary == "" {
		t.Errorf("report = lang %s, %d apps, summary %q", r.Lang, len(r.Apps), r.Summary)
	}
}

func TestAcceptLanguage(t *testing.T) {
	s, _ := newServer(t)
	req := httptest.NewRequest("GET", "/api/v1/i18n", nil)
	req.Header.Set("Accept-Language", "sv-SE,sv;q=0.9")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out struct {
		Lang    string            `json:"lang"`
		Strings map[string]string `json:"strings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Lang != "sv" || out.Strings["ui.title"] != "Hemvakten" {
		t.Errorf("got %s / %q", out.Lang, out.Strings["ui.title"])
	}
}

func TestWebUIServed(t *testing.T) {
	s, _ := newServer(t)
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s: status %d", p, rec.Code)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: security headers missing", p)
		}
	}
}

func TestStatusAndTestAlertGuard(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"checks": []`) {
		t.Errorf("status without monitor: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/events", nil))
	if rec.Code != 200 {
		t.Errorf("events: %d", rec.Code)
	}
	// Another web site cannot make Nestward send alerts.
	cases := []struct {
		header, origin string
		want           int
	}{
		{"", "", http.StatusForbidden},
		{"1", "https://evil.example", http.StatusForbidden},
		{"1", "http://example.com", http.StatusNotFound}, // same origin, but alerts are off in this test
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/v1/alerts/test", nil)
		if c.header != "" {
			req.Header.Set("X-Nestward", c.header)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("header %q origin %q: %d, want %d", c.header, c.origin, rec.Code, c.want)
		}
	}
}

func TestMonitorMachines(t *testing.T) {
	s, st := newServer(t)
	ctx := context.Background()
	st.SaveMachine(ctx, "agent", "192.168.1.180", fakeHW("pve"), &discovery.Result{Available: true})
	ms, err := s.MonitorMachines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || !ms[0].Local || ms[1].Address != "192.168.1.180" || ms[1].Local {
		t.Errorf("machines = %+v", ms)
	}
}

func TestHealthEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/health?lang=sv", nil))
	var out struct {
		Score     int    `json:"score"`
		GradeText string `json:"grade_text"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	// No alerts (−15) and no outside watch (−10) in this test server.
	if rec.Code != 200 || out.Score != 75 || out.GradeText != "Bra" {
		t.Errorf("health = %d %s", rec.Code, rec.Body.String())
	}
}
