package exposure

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wizz666/nestward/internal/cloudflare/cftest"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/notify"
	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

// fakeNet answers every host name from one test server, choosing the
// page by the Host header, like the internet would through a tunnel.
type fakeNet struct {
	mu    sync.Mutex
	pages map[string]http.HandlerFunc
	srv   *httptest.Server
}

func newNet(t *testing.T) *fakeNet {
	n := &fakeNet{pages: map[string]http.HandlerFunc{}}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		n.mu.Lock()
		h := n.pages[host]
		n.mu.Unlock()
		if h == nil {
			w.WriteHeader(530) // like Cloudflare when nothing is behind the route
			return
		}
		h(w, r)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNet) set(host string, h http.HandlerFunc) {
	n.mu.Lock()
	n.pages[host] = h
	n.mu.Unlock()
}

func (n *fakeNet) prober() *Prober {
	addr := n.srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	return &Prober{Scheme: "http", Client: &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func page(title, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("<html><head><title>" + title + "</title></head><body>" + body + "</body></html>"))
	}
}

func redirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
}

func TestProbeVerdicts(t *testing.T) {
	n := newNet(t)
	n.set("locked.test", redirect("https://home.cloudflareaccess.com/cdn-cgi/access/login/locked.test?kid=1"))
	n.set("code.test", page("", `<script>performance.mark('code/didStartRenderer');</script>`))
	n.set("ollama.test", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"llama"}]}`))
			return
		}
		w.Write([]byte("Ollama is running"))
	})
	n.set("pve.test", page("pve - Proxmox Virtual Environment", "<div id=app></div>"))
	n.set("app.test", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/users/sign_in", http.StatusFound)
			return
		}
		w.Write([]byte(`<title>Dawarich</title><a href="https://immich.app">Immich</a><form><input type="password"></form>`))
	})
	n.set("coolify.test", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Write([]byte(`<title>Coolify</title><input type="password">`))
	})
	n.set("basic.test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	n.set("site.test", page("My blog", "<p>Hello</p>"))
	n.set("move.test", redirect("https://other.test/"))
	n.set("vault.test", page("Vaultwarden Web", `<input type="password">`))
	n.set("photos.test", page("c43bb448eb22", `<script src="/_app/immich.js"></script>`))

	p := n.prober()
	ctx := context.Background()
	cases := map[string]struct {
		v   Verdict
		app string
	}{
		"locked.test":  {Protected, ""},
		"code.test":    {Open, "VS Code"},
		"ollama.test":  {Open, "Ollama"},
		"pve.test":     {AdminLogin, "Proxmox"},
		"app.test":     {OwnLogin, "Dawarich"},
		"coolify.test": {AdminLogin, "Coolify"},
		"basic.test":   {OwnLogin, ""},
		"site.test":    {Website, ""},
		"move.test":    {Website, ""},
		"vault.test":   {OwnLogin, "Vaultwarden"},
		"photos.test":  {OwnLogin, "Immich"},
		"gone.test":    {Dead, ""},
	}
	for host, want := range cases {
		got := p.Check(ctx, host)
		if got.Verdict != want.v || got.App != want.app {
			t.Errorf("%s: got %s/%q (%s), want %s/%q", host, got.Verdict, got.App, got.Detail, want.v, want.app)
		}
	}
	if SeverityOf(Open) != Critical || SeverityOf(AdminLogin) != Warn || SeverityOf(Protected) != OK || SeverityOf(Website) != Info {
		t.Error("severity mapping")
	}
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []notify.Message
	b    *i18n.Bundle
}

func (f *fakeNotifier) Notify(_ context.Context, m []notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m...)
	return nil
}

func (f *fakeNotifier) Lang(context.Context) i18n.T { return f.b.For("sv") }

func (f *fakeNotifier) take() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

func TestScannerAlertsOnceAndAccept(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	set := &config.Settings{Store: st, Box: box}
	ctx := context.Background()

	cf := cftest.New("cf-token")
	cf.Configs["tun1"]["ingress"] = []any{
		map[string]any{"hostname": "code.example.com", "service": "http://192.168.1.5:8066"},
		map[string]any{"hostname": "pve.example.com", "service": "https://192.168.1.10:8006"},
		map[string]any{"hostname": "locked.example.com", "service": "http://192.168.1.5:3080"},
		map[string]any{"hostname": "*.example.com", "service": "http://192.168.1.5:80"},
		map[string]any{"hostname": "code.example.com", "path": "/api", "service": "http://192.168.1.5:9"},
		map[string]any{"service": "http_status:404"},
	}
	cfs := cf.Server()
	t.Cleanup(cfs.Close)
	set.Set(ctx, config.CloudflareToken, "cf-token")
	set.Set(ctx, config.ExposureHosts, "https://blog.example.com/, code.example.com")

	n := newNet(t)
	n.set("code.example.com", page("", "vscode-remote-resource"))
	n.set("pve.example.com", page("Proxmox Virtual Environment", ""))
	n.set("locked.example.com", redirect("https://x.cloudflareaccess.com/cdn-cgi/access/login/locked.example.com"))
	n.set("blog.example.com", page("Blog", "hi"))

	bundle, err := i18n.Load()
	if err != nil {
		t.Fatal(err)
	}
	nt := &fakeNotifier{b: bundle}
	sc := &Scanner{Settings: set, Store: st, Prober: n.prober(), Notifier: nt, CloudflareBase: cfs.URL}

	r, err := sc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 4 || r.Error != "" {
		t.Fatalf("findings: %+v err %q", r.Findings, r.Error)
	}
	if r.Findings[0].Host != "code.example.com" || r.Findings[0].Verdict != Open || r.Findings[0].Source != "tunnel:home" {
		t.Errorf("worst first: %+v", r.Findings[0])
	}
	if r.Counts.Critical != 1 || r.Counts.Warn != 1 || r.Counts.OK != 1 || r.Counts.Info != 1 {
		t.Errorf("counts %+v", r.Counts)
	}
	msgs := nt.take()
	if len(msgs) != 2 || !strings.Contains(msgs[0].Title, "code.example.com") || msgs[0].Priority != 5 {
		t.Fatalf("first alerts: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Title, "Öppen") {
		t.Errorf("alert not in Swedish: %q", msgs[0].Title)
	}

	// The same problems are not sent again.
	if _, err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if m := nt.take(); len(m) != 0 {
		t.Fatalf("repeated alerts: %+v", m)
	}

	// Accepting the admin page keeps it quiet and counts it as fine.
	if _, err := sc.Accept(ctx, "PVE.example.com", true); err != nil {
		t.Fatal(err)
	}
	r, _ = sc.Report(ctx)
	if r.Counts.Warn != 0 || r.Counts.OK != 2 {
		t.Errorf("after accept %+v", r.Counts)
	}

	// Fixed, then opened again: a new alert.
	n.set("code.example.com", redirect("https://x.cloudflareaccess.com/cdn-cgi/access/login/code"))
	sc.RunOnce(ctx)
	if m := nt.take(); len(m) != 0 {
		t.Fatalf("alert after fix: %+v", m)
	}
	n.set("code.example.com", page("", "vscode-remote-resource"))
	sc.RunOnce(ctx)
	if m := nt.take(); len(m) != 1 {
		t.Fatalf("want one new alert, got %+v", m)
	}

	// A removed route is forgotten.
	set.Set(ctx, config.ExposureHosts, "")
	r, _ = sc.RunOnce(ctx)
	for _, f := range r.Findings {
		if f.Host == "blog.example.com" {
			t.Error("removed address still listed")
		}
	}
	if _, err := sc.Accept(ctx, "nope.example.com", true); err != ErrUnknownHost {
		t.Errorf("unknown host: %v", err)
	}
}

func TestComposeLimitsMessages(t *testing.T) {
	bundle, _ := i18n.Load()
	var list []Finding
	for _, h := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		list = append(list, Finding{Host: h + ".test", Probe: Probe{Verdict: AdminLogin}, Severity: Warn})
	}
	msgs := Compose(list, bundle.For("en"))
	if len(msgs) != 6 || !strings.Contains(msgs[5].Title, "2 more") {
		t.Fatalf("got %d: %+v", len(msgs), msgs[len(msgs)-1])
	}
}

func TestNoSourceNoFindings(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "x.db"))
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	sc := &Scanner{Settings: &config.Settings{Store: st, Box: box}, Store: st, Prober: &Prober{}}
	r, err := sc.RunOnce(context.Background())
	if err != nil || len(r.Findings) != 0 || len(r.Sources) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
