// Package publish makes a home service reachable on the user's own
// domain through a Cloudflare Tunnel, with a login in front when the app
// has none, and can undo every step again.
package publish

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/cloudflare"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/store"
)

// Errors are translation keys shown to the user.
var (
	ErrNotConfigured = errors.New("publish_not_configured")
	ErrBadName       = errors.New("publish_bad_name")
	ErrBadTarget     = errors.New("publish_bad_target")
	ErrLoopback      = errors.New("publish_target_loopback")
	ErrNameTaken     = errors.New("publish_name_taken")
	ErrNoZone        = errors.New("publish_no_zone")
	ErrNoTunnel      = errors.New("publish_no_tunnel")
	ErrLocalTunnel   = errors.New("publish_tunnel_not_remote")
	ErrLoginRequired = errors.New("publish_login_required")
	ErrNoEmails      = errors.New("publish_no_emails")
	ErrVideoWarning  = errors.New("publish_video_warning")
	ErrNotFound      = errors.New("publish_not_found")
	ErrAlreadyUndone = errors.New("publish_already_undone")
)

// Statuses of a publication.
const (
	Active  = "active"
	Removed = "removed"
	Failed  = "failed"
)

// videoApps stream video; Cloudflare's terms do not allow serving
// mostly video through its free proxy. Tailscale is the better path.
var videoApps = map[string]bool{"plex": true, "jellyfin": true, "emby": true}

// Request is what the user asks for.
type Request struct {
	// Provider is "cloudflare" (default) or "tailscale".
	Provider string   `json:"provider"`
	Name     string   `json:"name"` // first part of the address, e.g. "glances"
	ZoneID   string   `json:"zone_id"`
	TunnelID string   `json:"tunnel_id"`
	Target   string   `json:"target"` // where the tunnel finds the app, e.g. http://192.168.1.92:61208
	Access   *bool    `json:"access"` // login in front; nil = Nestward decides
	Emails   []string `json:"emails"`
	// About the service being published (from discovery).
	MachineID string `json:"machine_id"`
	Service   string `json:"service"`
	App       string `json:"app"`
	AppID     string `json:"app_id"`
	Login     string `json:"login"` // own, none or ""
	// AcceptVideoWarning confirms the user read the video warning.
	AcceptVideoWarning bool `json:"accept_video_warning"`
}

// Step is one thing done (or undone), for the history.
type Step struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"` // tunnel_route_added, dns_added, access_added, ..._removed
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
}

// Check is the result of opening the address from the outside.
type Check struct {
	At     time.Time `json:"at"`
	Result string    `json:"result"` // protected, reachable, unreachable, error
	Status int       `json:"status,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Publication is one published service.
type Publication struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	Hostname    string    `json:"hostname"`
	ZoneID      string    `json:"zone_id"`
	ZoneName    string    `json:"zone_name"`
	TunnelID    string    `json:"tunnel_id"`
	TunnelName  string    `json:"tunnel_name"`
	Target      string    `json:"target"`
	URL         string    `json:"url,omitempty"` // full address to open
	MachineID   string    `json:"machine_id,omitempty"`
	Service     string    `json:"service,omitempty"`
	App         string    `json:"app,omitempty"`
	Access      bool      `json:"access"`
	Emails      []string  `json:"emails,omitempty"`
	DNSRecordID string    `json:"dns_record_id,omitempty"`
	AccessAppID string    `json:"access_app_id,omitempty"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	Created     time.Time `json:"created"`
	Removed     time.Time `json:"removed,omitempty"`
	Steps       []Step    `json:"steps"`
	LastCheck   *Check    `json:"last_check,omitempty"`
}

// Engine publishes and undoes.
type Engine struct {
	Settings *config.Settings
	Store    *store.Store
	// CloudflareBase overrides the API address (tests).
	CloudflareBase string
	HTTP           *http.Client
	// Probe opens an address from the outside; nil uses HTTP.
	Probe func(ctx context.Context, url string) Check
	// Tailnet publishes inside Tailscale; nil turns that off.
	Tailnet Tailnet
	Now     func() time.Time

	mu sync.Mutex // one change at a time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) client(ctx context.Context) (*cloudflare.Client, string, error) {
	tok, err := e.Settings.Get(ctx, config.CloudflareToken)
	if err != nil {
		return nil, "", err
	}
	if tok == "" {
		return nil, "", ErrNotConfigured
	}
	c := &cloudflare.Client{Token: tok, Base: e.CloudflareBase, HTTP: e.HTTP}
	acc, err := e.Settings.Get(ctx, config.CloudflareAccount)
	if err != nil {
		return nil, "", err
	}
	if acc == "" {
		accs, err := c.Accounts(ctx)
		if err == nil && len(accs) == 1 {
			acc = accs[0].ID
		} else {
			// The token Nestward asks for may not list accounts; the
			// domains say which account they belong to.
			if acc, err = c.AccountFromZones(ctx); err != nil {
				return nil, "", err
			}
		}
		// Remember it so the lookup is not repeated.
		_ = e.Settings.Set(ctx, config.CloudflareAccount, acc)
	}
	return c, acc, nil
}

// Options is what the publish form offers.
type Options struct {
	Tailscale  bool                `json:"tailscale"` // available in this build
	TSKey      bool                `json:"tailscale_configured"`
	Configured bool                `json:"configured"`
	Error      string              `json:"error,omitempty"`
	Zones      []cloudflare.Zone   `json:"zones"`
	Tunnels    []cloudflare.Tunnel `json:"tunnels"`
	Emails     []string            `json:"emails"`
	LANAddress string              `json:"lan_address,omitempty"`
}

// Options reads the zones and tunnels the token can use.
func (e *Engine) Options(ctx context.Context) Options {
	o := Options{Zones: []cloudflare.Zone{}, Tunnels: []cloudflare.Tunnel{}, Emails: []string{}}
	if v, _ := e.Settings.Get(ctx, config.PublishEmails); v != "" {
		o.Emails = splitEmails(v)
	}
	o.LANAddress, _ = e.Settings.Get(ctx, config.LANAddress)
	o.Tailscale = e.Tailnet != nil
	if k, _ := e.Settings.Get(ctx, config.TailscaleAuthKey); k != "" {
		o.TSKey = true
	}
	c, acc, err := e.client(ctx)
	if errors.Is(err, ErrNotConfigured) {
		return o
	}
	o.Configured = true
	if err != nil {
		o.Error = err.Error()
		return o
	}
	if o.Zones, err = c.Zones(ctx, acc); err != nil {
		o.Error = err.Error()
		return o
	}
	if o.Tunnels, err = c.Tunnels(ctx, acc); err != nil {
		o.Error = err.Error()
	}
	sort.Slice(o.Zones, func(i, j int) bool { return o.Zones[i].Name < o.Zones[j].Name })
	sort.Slice(o.Tunnels, func(i, j int) bool { return o.Tunnels[i].Name < o.Tunnels[j].Name })
	return o
}

func splitEmails(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, strings.ToLower(e))
		}
	}
	return out
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NeedsLogin reports whether an app must get a login in front: every app
// that does not have its own login, or where that is unknown.
func NeedsLogin(login string) bool { return login != "own" }

func checkTarget(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Port() == "" {
		return "", ErrBadTarget
	}
	h := u.Hostname()
	if h == "localhost" || h == "host.docker.internal" {
		return "", ErrLoopback
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return "", ErrLoopback
	}
	return u.Scheme + "://" + u.Host, nil
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Publish checks everything first, then adds the tunnel route, the DNS
// record and (when needed) the login. If a step fails, the earlier steps
// are undone again.
func (e *Engine) Publish(ctx context.Context, req Request) (Publication, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !labelRe.MatchString(name) {
		return Publication{}, ErrBadName
	}
	target, err := checkTarget(req.Target)
	if err != nil {
		return Publication{}, err
	}
	if req.Provider == Tailscale {
		// Only the user's own devices can reach it: the tailnet is the login.
		return e.publishTailscale(ctx, req, name, target)
	}
	if videoApps[req.AppID] && !req.AcceptVideoWarning {
		return Publication{}, ErrVideoWarning
	}
	access := NeedsLogin(req.Login)
	if req.Access != nil {
		if !*req.Access && NeedsLogin(req.Login) {
			return Publication{}, ErrLoginRequired
		}
		access = *req.Access
	}
	emails := req.Emails
	if len(emails) == 0 {
		v, _ := e.Settings.Get(ctx, config.PublishEmails)
		emails = splitEmails(v)
	}
	if access {
		if len(emails) == 0 {
			return Publication{}, ErrNoEmails
		}
		for _, em := range emails {
			if !strings.Contains(em, "@") || strings.ContainsAny(em, " <>,") {
				return Publication{}, ErrNoEmails
			}
		}
	}

	c, acc, err := e.client(ctx)
	if err != nil {
		return Publication{}, err
	}
	zones, err := c.Zones(ctx, acc)
	if err != nil {
		return Publication{}, err
	}
	var zone cloudflare.Zone
	for _, z := range zones {
		if z.ID == req.ZoneID {
			zone = z
		}
	}
	if zone.ID == "" {
		return Publication{}, ErrNoZone
	}
	tunnels, err := c.Tunnels(ctx, acc)
	if err != nil {
		return Publication{}, err
	}
	var tun cloudflare.Tunnel
	for _, t := range tunnels {
		if t.ID == req.TunnelID {
			tun = t
		}
	}
	if tun.ID == "" {
		return Publication{}, ErrNoTunnel
	}
	if !tun.Remote() {
		return Publication{}, ErrLocalTunnel
	}
	hostname := name + "." + zone.Name

	// Never take over an address that is already in use.
	recs, err := c.DNSRecords(ctx, zone.ID, hostname)
	if err != nil {
		return Publication{}, err
	}
	if len(recs) > 0 {
		return Publication{}, ErrNameTaken
	}
	cfg, err := c.TunnelConfiguration(ctx, acc, tun.ID)
	if err != nil {
		return Publication{}, err
	}
	for _, r := range cfg.Ingress {
		if h, _ := r["hostname"].(string); strings.EqualFold(h, hostname) {
			return Publication{}, ErrNameTaken
		}
	}

	p := Publication{
		ID: newID(), Provider: Cloudflare, Hostname: hostname, URL: "https://" + hostname, ZoneID: zone.ID, ZoneName: zone.Name,
		TunnelID: tun.ID, TunnelName: tun.Name, Target: target, MachineID: req.MachineID, Service: req.Service,
		App: req.App, Access: access, Status: Active, Created: e.now(),
	}
	if access {
		p.Emails = emails
	}
	step := func(action string, err error, detail string) {
		s := Step{At: e.now(), Action: action, OK: err == nil, Detail: detail}
		if err != nil {
			s.Detail = err.Error()
		}
		p.Steps = append(p.Steps, s)
	}
	fail := func(err error) (Publication, error) {
		p.Status, p.Error = Failed, err.Error()
		e.rollback(ctx, c, acc, &p, step)
		_ = e.Store.SavePublication(ctx, p.ID, p)
		return p, err
	}

	// 1. Route in the tunnel, placed before the final catch-all rule.
	rule := map[string]any{"hostname": hostname, "service": target}
	if strings.HasPrefix(target, "https://") {
		// Home apps often use self-signed certificates.
		rule["originRequest"] = map[string]any{"noTLSVerify": true}
	}
	cfg.Ingress = insertBeforeCatchAll(cfg.Ingress, rule)
	err = c.PutTunnelConfiguration(ctx, acc, tun.ID, cfg)
	step("tunnel_route_added", err, tun.Name+": "+hostname+" → "+target)
	if err != nil {
		return fail(err)
	}
	// 2. DNS record pointing at the tunnel.
	rec, err := c.CreateDNSRecord(ctx, zone.ID, cloudflare.DNSRecord{Type: "CNAME", Name: hostname,
		Content: tun.ID + ".cfargotunnel.com", Proxied: true, Comment: "Nestward " + p.ID})
	step("dns_added", err, hostname)
	if err != nil {
		return fail(err)
	}
	p.DNSRecordID = rec.ID
	// 3. Login in front.
	if access {
		label := req.App
		if label == "" {
			label = hostname
		}
		app, err := c.CreateAccessApp(ctx, acc, label+" (Nestward)", hostname, emails)
		step("access_added", err, strings.Join(emails, ", "))
		if err != nil {
			return fail(err)
		}
		p.AccessAppID = app.ID
	}
	if err := e.Store.SavePublication(ctx, p.ID, p); err != nil {
		return p, err
	}
	return p, nil
}

func insertBeforeCatchAll(rules []map[string]any, rule map[string]any) []map[string]any {
	n := len(rules)
	if n > 0 {
		if h, _ := rules[n-1]["hostname"].(string); h == "" {
			out := append([]map[string]any{}, rules[:n-1]...)
			out = append(out, rule)
			return append(out, rules[n-1])
		}
	}
	// No catch-all: add the rule and the catch-all Cloudflare requires.
	return append(append(rules, rule), map[string]any{"service": "http_status:404"})
}

// rollback undoes what a publication has done, newest first. "Not
// found" counts as done: someone may have removed it by hand.
func (e *Engine) rollback(ctx context.Context, c *cloudflare.Client, acc string, p *Publication, step func(string, error, string)) bool {
	ok := true
	gone := func(err error) error {
		var ae *cloudflare.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	if p.AccessAppID != "" {
		err := gone(c.DeleteAccessApp(ctx, acc, p.AccessAppID))
		step("access_removed", err, "")
		if err == nil {
			p.AccessAppID = ""
		} else {
			ok = false
		}
	}
	if p.DNSRecordID != "" {
		err := gone(c.DeleteDNSRecord(ctx, p.ZoneID, p.DNSRecordID))
		step("dns_removed", err, p.Hostname)
		if err == nil {
			p.DNSRecordID = ""
		} else {
			ok = false
		}
	}
	if hasStep(p.Steps, "tunnel_route_added") && !hasStep(p.Steps, "tunnel_route_removed") {
		cfg, err := c.TunnelConfiguration(ctx, acc, p.TunnelID)
		if err == nil {
			var keep []map[string]any
			removed := false
			for _, r := range cfg.Ingress {
				h, _ := r["hostname"].(string)
				s, _ := r["service"].(string)
				if strings.EqualFold(h, p.Hostname) && s == p.Target {
					removed = true
					continue
				}
				keep = append(keep, r)
			}
			if removed {
				cfg.Ingress = keep
				err = c.PutTunnelConfiguration(ctx, acc, p.TunnelID, cfg)
			}
		}
		step("tunnel_route_removed", err, p.Hostname)
		if err != nil {
			ok = false
		}
	}
	return ok
}

func hasStep(steps []Step, action string) bool {
	for _, s := range steps {
		if s.Action == action && s.OK {
			return true
		}
	}
	return false
}

// Undo removes a publication: the login, the DNS record and the tunnel
// route. Only what Nestward added is touched.
func (e *Engine) Undo(ctx context.Context, id string) (Publication, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, err := e.get(ctx, id)
	if err != nil {
		return p, err
	}
	if p.Status == Removed {
		return p, ErrAlreadyUndone
	}
	if p.Provider == Tailscale {
		if err := e.undoTailscale(ctx, &p); err != nil {
			p.Error = "undo_incomplete"
			_ = e.Store.SavePublication(ctx, p.ID, p)
			return p, err
		}
		p.Status, p.Removed, p.Error = Removed, e.now(), ""
		return p, e.Store.SavePublication(ctx, p.ID, p)
	}
	c, acc, err := e.client(ctx)
	if err != nil {
		return p, err
	}
	step := func(action string, err error, detail string) {
		s := Step{At: e.now(), Action: action, OK: err == nil, Detail: detail}
		if err != nil {
			s.Detail = err.Error()
		}
		p.Steps = append(p.Steps, s)
	}
	if e.rollback(ctx, c, acc, &p, step) {
		p.Status, p.Removed, p.Error = Removed, e.now(), ""
	} else {
		p.Error = "undo_incomplete"
	}
	if err := e.Store.SavePublication(ctx, p.ID, p); err != nil {
		return p, err
	}
	if p.Status != Removed {
		return p, errors.New("undo_incomplete")
	}
	return p, nil
}

func (e *Engine) get(ctx context.Context, id string) (Publication, error) {
	all, err := e.List(ctx)
	if err != nil {
		return Publication{}, err
	}
	for _, p := range all {
		if p.ID == id {
			return p, nil
		}
	}
	return Publication{}, ErrNotFound
}

// List returns every publication, newest first.
func (e *Engine) List(ctx context.Context) ([]Publication, error) {
	raws, err := e.Store.Publications(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Publication, 0, len(raws))
	for _, raw := range raws {
		var p Publication
		if json.Unmarshal(raw, &p) == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// CheckOutside opens the published address from the internet side, the
// way a visitor would, and stores the result.
func (e *Engine) CheckOutside(ctx context.Context, id string) (Check, error) {
	p, err := e.get(ctx, id)
	if err != nil {
		return Check{}, err
	}
	probe := e.Probe
	if probe == nil {
		probe = HTTPCheck
	}
	u := p.URL
	if u == "" {
		u = "https://" + p.Hostname
	}
	ch := probe(ctx, strings.TrimSuffix(u, "/")+"/")
	ch.At = e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	p, err = e.get(ctx, id)
	if err != nil {
		return ch, err
	}
	p.LastCheck = &ch
	return ch, e.Store.SavePublication(ctx, p.ID, p)
}

var checkClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
}

// HTTPCheck classifies what a visitor sees at an address.
func HTTPCheck(ctx context.Context, u string) Check {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Check{Result: "error", Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "nestward-publish-check")
	resp, err := checkClient.Do(req)
	if err != nil {
		return Check{Result: "unreachable", Detail: err.Error()}
	}
	resp.Body.Close()
	ch := Check{Status: resp.StatusCode}
	loc := resp.Header.Get("Location")
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400 && strings.Contains(loc, "cloudflareaccess.com"):
		ch.Result = "protected"
	case resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 || resp.StatusCode >= 520:
		ch.Result = "unreachable"
		ch.Detail = "the tunnel cannot reach the app"
	case resp.StatusCode < 500:
		ch.Result = "reachable"
	default:
		ch.Result = "unreachable"
	}
	return ch
}
