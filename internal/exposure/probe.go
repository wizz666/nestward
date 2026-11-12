// Package exposure checks what the home shows to the whole internet.
// It lists every public address (Cloudflare Tunnel routes plus any extra
// addresses the owner adds), opens each one from the outside like a
// stranger would, and sorts the answers: behind a login in front, own
// login page, a normal web site, or wide open. Open admin tools and
// shells are the dangerous ones, and they raise an alert.
package exposure

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Verdict is what a stranger meets at an address.
type Verdict string

const (
	// Protected: a login in front (Cloudflare Access) before the app.
	Protected Verdict = "protected"
	// OwnLogin: the app asks for its own login.
	OwnLogin Verdict = "own_login"
	// AdminLogin: an admin tool (Proxmox, Portainer …) shows its login
	// page to everyone. It has a password, but admin tools are a favourite
	// target and should sit behind a login in front.
	AdminLogin Verdict = "admin_login"
	// Website: an ordinary page with no login, which is fine for a site.
	Website Verdict = "website"
	// Open: a tool that must never be open answers without any login
	// (a code editor with a terminal, an AI model server …).
	Open Verdict = "open"
	// Dead: the address does not answer (removed app, broken route).
	Dead Verdict = "dead"
)

// Severity orders verdicts by how urgent they are.
type Severity string

const (
	OK       Severity = "ok"
	Info     Severity = "info"
	Warn     Severity = "warn"
	Critical Severity = "critical"
)

// SeverityOf maps a verdict to its severity.
func SeverityOf(v Verdict) Severity {
	switch v {
	case Protected:
		return OK
	case Open:
		return Critical
	case AdminLogin, Dead:
		return Warn
	default:
		return Info
	}
}

// Probe is the result of opening one address.
type Probe struct {
	Verdict Verdict `json:"verdict"`
	App     string  `json:"app,omitempty"`    // recognised tool, e.g. "Proxmox"
	Status  int     `json:"status,omitempty"` // HTTP status of the first answer
	Detail  string  `json:"detail,omitempty"` // short technical reason
}

// sign is a recognisable tool.
type sign struct {
	app   string
	match func(title, body string) bool
	// open means the tool is dangerous when it answers without login.
	open bool
}

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// signs are checked in order; the first match wins. Title and body are
// already lower case, so every pattern must be lower case too.
var signs = []sign{
	{app: "VS Code", open: true, match: func(t, b string) bool {
		return has(b, "code/didstartrenderer", "vscode-remote-resource", "vs/workbench/workbench.web")
	}},
	{app: "Jupyter", open: true, match: func(t, b string) bool { return has(t, "jupyter") && !has(b, "type=\"password\"") }},
	{app: "Proxmox", match: func(t, b string) bool { return has(t, "proxmox virtual environment", "proxmox backup server") }},
	{app: "Portainer", match: func(t, b string) bool { return has(t, "portainer") }},
	{app: "Unraid", match: func(t, b string) bool { return has(t, "/login") && has(b, "unraid") }},
	{app: "QNAP", match: func(t, b string) bool { return has(b, "qnap", "qts") && has(b, "redirect.html") }},
	{app: "CasaOS", match: func(t, b string) bool { return has(b, "casaos") }},
	{app: "Coolify", match: func(t, b string) bool { return has(t, "coolify") || has(b, "coolify") }},
	{app: "Pangolin", match: func(t, b string) bool { return has(t, "pangolin") }},
	{app: "Nginx Proxy Manager", match: func(t, b string) bool { return has(t, "nginx proxy manager") }},
	{app: "Home Assistant", match: func(t, b string) bool { return has(t, "home assistant") }},
	{app: "Cockpit", match: func(t, b string) bool { return has(b, "cockpit") && has(t, "login") }},
	{app: "Webmin", match: func(t, b string) bool { return has(t, "webmin") }},
	{app: "phpMyAdmin", match: func(t, b string) bool { return has(t, "phpmyadmin") }},
	{app: "Adminer", match: func(t, b string) bool { return has(t, "adminer") }},
	{app: "Traefik", match: func(t, b string) bool { return has(t, "traefik") }},
	{app: "TrueNAS", match: func(t, b string) bool { return has(t, "truenas") }},
	{app: "Synology", match: func(t, b string) bool { return has(t, "synology") }},
	{app: "Grafana", match: func(t, b string) bool { return has(t, "grafana") }},
	{app: "Uptime Kuma", match: func(t, b string) bool { return has(t, "uptime kuma") }},
	// Apps whose login is drawn by JavaScript: the first page has no
	// password field, but they do ask for one.
	{app: "Vaultwarden", match: func(t, b string) bool { return has(t, "vaultwarden", "bitwarden") }},
	// Named by title before Immich, whose check only looks at the page
	// body (other apps link to Immich).
	{app: "Dawarich", match: func(t, b string) bool { return has(t, "dawarich") }},
	{app: "Paperless", match: func(t, b string) bool { return has(t, "paperless") }},
	{app: "Karakeep", match: func(t, b string) bool { return has(t, "karakeep") }},
	{app: "WordPress", match: func(t, b string) bool { return has(b, "wp-submit", "/wp-login.php") }},
	{app: "Immich", match: func(t, b string) bool { return has(b, "immich") }},
	{app: "Jellyfin", match: func(t, b string) bool { return has(t, "jellyfin") }},
	{app: "Emby", match: func(t, b string) bool { return has(t, "emby") || has(b, "emby-") }},
	{app: "Plex", match: func(t, b string) bool { return has(t, "plex") }},
	{app: "ntfy", match: func(t, b string) bool { return has(t, "ntfy") }},
	{app: "Nextcloud", match: func(t, b string) bool { return has(t, "nextcloud") }},
}

// adminApps are tools whose login page should not face the internet.
var adminApps = map[string]bool{
	"Proxmox": true, "Portainer": true, "Unraid": true, "QNAP": true, "CasaOS": true, "Coolify": true,
	"Pangolin": true, "Nginx Proxy Manager": true, "Cockpit": true, "Webmin": true, "phpMyAdmin": true,
	"Adminer": true, "Traefik": true, "TrueNAS": true, "Synology": true,
}

// Prober opens addresses from the outside.
type Prober struct {
	Client *http.Client
	// Scheme is "https" (default); tests use "http".
	Scheme string
}

const maxBody = 256 << 10

func (p *Prober) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 8 * time.Second}).DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 12 * time.Second,
			// Some home apps use their own certificates behind the tunnel;
			// the check is about what answers, not about the certificate.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (p *Prober) scheme() string {
	if p.Scheme != "" {
		return p.Scheme
	}
	return "https"
}

type answer struct {
	status   int
	location string
	title    string
	body     string
	auth     string // WWW-Authenticate
}

func (p *Prober) get(ctx context.Context, raw string) (answer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("User-Agent", "Nestward exposure check")
	resp, err := p.client().Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	body := strings.ToLower(string(b))
	return answer{status: resp.StatusCode, location: resp.Header.Get("Location"), body: body,
		title: titleOf(body), auth: resp.Header.Get("WWW-Authenticate")}, nil
}

func titleOf(body string) string {
	i := strings.Index(body, "<title")
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], ">")
	if j < 0 {
		return ""
	}
	rest := body[i+j+1:]
	k := strings.Index(rest, "</title")
	if k < 0 {
		k = min(len(rest), 120)
	}
	return strings.TrimSpace(rest[:k])
}

func accessLogin(loc string) bool {
	return strings.Contains(loc, ".cloudflareaccess.com/") || strings.Contains(loc, "/cdn-cgi/access/login")
}

func loginPath(loc string) bool {
	l := strings.ToLower(loc)
	return has(l, "login", "signin", "sign_in", "sign-in", "/auth", "/account", "oauth", "sso")
}

// Check opens https://host/ and decides what a stranger meets.
func (p *Prober) Check(ctx context.Context, host string) Probe {
	base := p.scheme() + "://" + host
	cur := base + "/"
	var first int
	for hop := 0; hop < 4; hop++ {
		a, err := p.get(ctx, cur)
		if err != nil {
			return Probe{Verdict: Dead, Detail: shortErr(err)}
		}
		if hop == 0 {
			first = a.status
		}
		switch {
		case a.status >= 300 && a.status < 400 && a.location != "":
			if accessLogin(a.location) {
				return Probe{Verdict: Protected, Status: first, Detail: "login in front"}
			}
			next, err := url.Parse(cur)
			if err != nil {
				return Probe{Verdict: Dead, Status: first, Detail: "bad redirect"}
			}
			to, err := next.Parse(a.location)
			if err != nil {
				return Probe{Verdict: Dead, Status: first, Detail: "bad redirect"}
			}
			if to.Host != "" && !strings.EqualFold(to.Hostname(), next.Hostname()) {
				// Sent to another address (e.g. a site moving to its main
				// domain). That address is checked on its own if listed.
				return Probe{Verdict: Website, Status: first, Detail: "redirects to " + to.Hostname()}
			}
			if loginPath(to.Path) {
				// Follow once more so the login page can be recognised.
				b, err := p.get(ctx, to.String())
				pr := Probe{Verdict: OwnLogin, Status: first, Detail: "login page " + to.Path}
				if err == nil {
					if app := recognise(b.title, b.body); app != nil {
						pr.App = app.app
						if adminApps[app.app] {
							pr.Verdict = AdminLogin
						}
					}
				}
				return pr
			}
			cur = to.String()
			continue
		case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden && a.auth != "":
			return Probe{Verdict: OwnLogin, Status: first, Detail: "asks for a password"}
		case a.status >= 500 || a.status == 404 && a.body == "":
			return Probe{Verdict: Dead, Status: first, Detail: http.StatusText(a.status)}
		}
		return p.classifyPage(ctx, base, a, first)
	}
	return Probe{Verdict: Dead, Status: first, Detail: "too many redirects"}
}

func (p *Prober) classifyPage(ctx context.Context, base string, a answer, first int) Probe {
	pr := Probe{Status: first}
	app := recognise(a.title, a.body)
	if app != nil {
		pr.App = app.app
		if app.open {
			pr.Verdict, pr.Detail = Open, "answers without any login"
			return pr
		}
		if adminApps[app.app] {
			pr.Verdict, pr.Detail = AdminLogin, "admin login page"
			return pr
		}
	}
	// An API that hands out its AI models to anyone.
	if o, ok := p.openAPI(ctx, base); ok {
		pr.App, pr.Verdict, pr.Detail = o, Open, "API answers without a key"
		return pr
	}
	if has(a.body, `type="password"`, `type='password'`, `type=password`) {
		pr.Verdict, pr.Detail = OwnLogin, "login form"
		return pr
	}
	if app != nil {
		pr.Verdict, pr.Detail = OwnLogin, "app page"
		return pr
	}
	pr.Verdict, pr.Detail = Website, "web page"
	return pr
}

// openAPI looks for model servers that answer without a key.
func (p *Prober) openAPI(ctx context.Context, base string) (string, bool) {
	if a, err := p.get(ctx, base+"/api/tags"); err == nil && a.status == 200 && strings.Contains(a.body, `"models"`) {
		return "Ollama", true
	}
	if a, err := p.get(ctx, base+"/v1/models"); err == nil && a.status == 200 &&
		strings.Contains(a.body, `"data"`) && strings.Contains(a.body, `"object"`) {
		return "AI API", true
	}
	return "", false
}

func recognise(title, body string) *sign {
	for i := range signs {
		if signs[i].match(title, body) {
			return &signs[i]
		}
	}
	return nil
}

func shortErr(err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns):
		return "name not found"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout"):
		return "no answer (timeout)"
	}
	s := err.Error()
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
