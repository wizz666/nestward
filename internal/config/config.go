// Package config holds Nestward's own settings (where to send alerts and
// so on). Secret values are encrypted before they reach the database and
// are never shown in full.
package config

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

// Key describes one setting.
type Key struct {
	Name     string
	Secret   bool
	Default  string
	Validate func(string) error
	Help     string
}

// Setting names.
const (
	NtfyURL      = "ntfy.url"
	NtfyTopic    = "ntfy.topic"
	NtfyToken    = "ntfy.token"
	AlertsLang   = "alerts.lang"
	HeartbeatURL = "heartbeat.url"
	HostAddress  = "host.address"

	LANAddress        = "lan.address"
	CloudflareToken   = "cloudflare.token"
	CloudflareAccount = "cloudflare.account"
	PublishEmails     = "publish.emails"
	TailscaleAuthKey  = "tailscale.authkey"
	TailscaleAPIKey   = "tailscale.apikey"
	ExposureHosts     = "exposure.hosts"
	ShareToken        = "share.token"
)

func httpURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an http:// or https:// address")
	}
	return nil
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		for _, x := range vals {
			if v == x {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(vals, ", "))
	}
}

func topic(v string) error {
	if v == "" || len(v) > 64 || strings.ContainsAny(v, "/?# ") {
		return fmt.Errorf("must be 1-64 characters without / ? # or spaces")
	}
	return nil
}

func shareToken(v string) error {
	if len(v) < 24 || strings.ContainsFunc(v, unicode.IsSpace) {
		return fmt.Errorf("must be at least 24 characters without spaces")
	}
	return nil
}

func host(v string) error {
	if v == "" || strings.ContainsAny(v, "/ ") {
		return fmt.Errorf("must be a host name or IP address")
	}
	return nil
}

// Keys lists every known setting.
var Keys = []Key{
	{Name: NtfyURL, Default: "https://ntfy.sh", Validate: httpURL, Help: "ntfy server that sends alerts to the phone"},
	{Name: NtfyTopic, Validate: topic, Help: "ntfy topic to publish to (the app subscribes to it)"},
	{Name: NtfyToken, Secret: true, Help: "ntfy access token, if the server needs one"},
	{Name: AlertsLang, Default: "en", Validate: oneOf("en", "sv"), Help: "language of alert messages"},
	{Name: HeartbeatURL, Secret: true, Validate: httpURL, Help: "address pinged every minute so an outside service notices when the home is down"},
	{Name: HostAddress, Validate: host, Help: "address of this machine as seen from Nestward (for checking web pages)"},
	{Name: LANAddress, Validate: host, Help: "this machine's address on the home network (used as the target when publishing)"},
	{Name: CloudflareToken, Secret: true, Help: "Cloudflare API token (Tunnel edit, DNS edit, Access apps edit)"},
	{Name: CloudflareAccount, Help: "Cloudflare account id (found automatically when the token sees only one)"},
	{Name: PublishEmails, Validate: emails, Help: "e-mail addresses allowed through the login in front of published apps (comma separated)"},
	{Name: TailscaleAuthKey, Secret: true, Validate: tsKey, Help: "Tailscale auth key (reusable) for publishing inside your tailnet"},
	{Name: TailscaleAPIKey, Secret: true, Validate: tsAPIKey, Help: "optional Tailscale API access token: undo then deletes the device from the tailnet"},
	{Name: ShareToken, Secret: true, Validate: shareToken, Help: "read-only key that lets another program (such as WizzOS) read the health summary at /api/v1/share/summary; empty turns it off"},
	{Name: ExposureHosts, Validate: hostList, Help: "extra public addresses to check from the outside (comma separated), besides the Cloudflare Tunnel routes"},
}

func hostList(v string) error {
	for _, h := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		h = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://"), "/")
		if h == "" || strings.ContainsAny(h, "/?#@") || !strings.Contains(h, ".") {
			return fmt.Errorf("%q is not a host name like app.example.com", h)
		}
	}
	return nil
}

func tsAPIKey(v string) error {
	if !strings.HasPrefix(v, "tskey-api-") {
		return fmt.Errorf("a Tailscale API access token starts with tskey-api-")
	}
	return nil
}

func tsKey(v string) error {
	if !strings.HasPrefix(v, "tskey-") {
		return fmt.Errorf("a Tailscale auth key starts with tskey-")
	}
	return nil
}

func emails(v string) error {
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		at := strings.LastIndex(e, "@")
		if at < 1 || at == len(e)-1 || strings.ContainsAny(e, " <>") || !strings.Contains(e[at:], ".") {
			return fmt.Errorf("%q is not an e-mail address", e)
		}
	}
	return nil
}

func lookup(name string) (Key, bool) {
	for _, k := range Keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// Settings reads and writes settings.
type Settings struct {
	Store *store.Store
	Box   *secrets.Box
}

// Get returns a setting, decrypted, or its default.
func (s *Settings) Get(ctx context.Context, name string) (string, error) {
	k, ok := lookup(name)
	if !ok {
		return "", fmt.Errorf("unknown setting %q", name)
	}
	v, found, err := s.Store.Setting(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return k.Default, nil
	}
	if k.Secret {
		return s.Box.Open(v)
	}
	return v, nil
}

// Set validates and stores a setting. An empty value removes it.
func (s *Settings) Set(ctx context.Context, name, value string) error {
	k, ok := lookup(name)
	if !ok {
		return fmt.Errorf("unknown setting %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return s.Store.DeleteSetting(ctx, name)
	}
	if k.Validate != nil {
		if err := k.Validate(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if k.Secret {
		sealed, err := s.Box.Seal(value)
		if err != nil {
			return err
		}
		value = sealed
	}
	return s.Store.SetSetting(ctx, name, value)
}

// Names returns all setting names, sorted.
func Names() []string {
	var out []string
	for _, k := range Keys {
		out = append(out, k.Name)
	}
	sort.Strings(out)
	return out
}

// Entry is one setting for display.
type Entry struct {
	Name   string `json:"name"`
	Value  string `json:"value"` // secrets masked
	IsSet  bool   `json:"is_set"`
	Secret bool   `json:"secret"`
	Help   string `json:"help"`
}

// List returns every setting with secrets masked.
func (s *Settings) List(ctx context.Context) ([]Entry, error) {
	stored, err := s.Store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, k := range Keys {
		e := Entry{Name: k.Name, Secret: k.Secret, Help: k.Help}
		v, set := stored[k.Name]
		e.IsSet = set
		switch {
		case !set:
			e.Value = k.Default
		case k.Secret:
			e.Value = "••••••"
		default:
			e.Value = v
		}
		out = append(out, e)
	}
	return out, nil
}

// MaskURL hides the path and query of an address, which often carry a
// token: "https://hc-ping.com/abc-123" -> "https://hc-ping.com/…".
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "…"
	}
	if u.Path == "" || u.Path == "/" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + "/…"
}
