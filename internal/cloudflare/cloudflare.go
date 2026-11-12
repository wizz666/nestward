// Package cloudflare is a small client for the parts of the Cloudflare
// API Nestward uses: tunnels (remote-managed), DNS records and Access.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBase is the Cloudflare API address.
const DefaultBase = "https://api.cloudflare.com/client/v4"

// Client calls the API with one token.
type Client struct {
	Token string
	Base  string // DefaultBase when empty (tests use a fake server)
	HTTP  *http.Client
}

// APIError is an error answer from Cloudflare.
type APIError struct {
	Status   int
	Messages []string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare %d: %s", e.Status, strings.Join(e.Messages, "; "))
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMessage    `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	base := c.Base
	if base == "" {
		base = DefaultBase
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{Status: resp.StatusCode, Messages: []string{strings.TrimSpace(string(raw[:min(len(raw), 200)]))}}
	}
	if !env.Success || resp.StatusCode >= 300 {
		e := &APIError{Status: resp.StatusCode}
		for _, m := range env.Errors {
			e.Messages = append(e.Messages, fmt.Sprintf("%s (%d)", m.Message, m.Code))
		}
		return e
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Account is a Cloudflare account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the accounts the token can see.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	return out, c.do(ctx, http.MethodGet, "/accounts?per_page=50", nil, &out)
}

// Zone is a domain.
type Zone struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Account Account `json:"account"`
}

// AccountFromZones finds the account through the domains the token can
// read. A token limited to the rights Nestward asks for cannot list
// accounts, but every zone says which account it belongs to.
func (c *Client) AccountFromZones(ctx context.Context) (string, error) {
	var zones []Zone
	if err := c.do(ctx, http.MethodGet, "/zones?per_page=50", nil, &zones); err != nil {
		return "", err
	}
	ids := map[string]bool{}
	for _, z := range zones {
		if z.Account.ID != "" {
			ids[z.Account.ID] = true
		}
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("the token's domains belong to %d accounts; set cloudflare.account", len(ids))
	}
	for id := range ids {
		return id, nil
	}
	return "", nil
}

// Zones lists the domains of an account.
func (c *Client) Zones(ctx context.Context, account string) ([]Zone, error) {
	var out []Zone
	return out, c.do(ctx, http.MethodGet, "/zones?per_page=50&account.id="+url.QueryEscape(account), nil, &out)
}

// Tunnel is a Cloudflare Tunnel.
type Tunnel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`     // healthy, degraded, down, inactive
	ConfigSrc string `json:"config_src"` // "cloudflare" = managed from the dashboard/API
}

// Remote reports whether the tunnel's routes are managed through the API
// (only those can be changed by Nestward).
func (t Tunnel) Remote() bool { return t.ConfigSrc == "cloudflare" }

// Tunnels lists the tunnels of an account.
func (c *Client) Tunnels(ctx context.Context, account string) ([]Tunnel, error) {
	var out []Tunnel
	return out, c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel?is_deleted=false&per_page=100", nil, &out)
}

// TunnelConfig is a remote tunnel configuration. Fields Nestward does not
// know are kept as they are, so saving never drops anything.
type TunnelConfig struct {
	Ingress []map[string]any           `json:"ingress"`
	Other   map[string]json.RawMessage `json:"-"`
}

// MarshalJSON writes the config back with every field it came with.
func (tc TunnelConfig) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range tc.Other {
		m[k] = v
	}
	m["ingress"] = tc.Ingress
	return json.Marshal(m)
}

// UnmarshalJSON keeps unknown fields.
func (tc *TunnelConfig) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	if raw, ok := all["ingress"]; ok {
		if err := json.Unmarshal(raw, &tc.Ingress); err != nil {
			return err
		}
		delete(all, "ingress")
	}
	tc.Other = all
	return nil
}

// TunnelConfiguration reads a tunnel's routes.
func (c *Client) TunnelConfiguration(ctx context.Context, account, tunnel string) (TunnelConfig, error) {
	var out struct {
		Config TunnelConfig `json:"config"`
	}
	err := c.do(ctx, http.MethodGet, "/accounts/"+account+"/cfd_tunnel/"+tunnel+"/configurations", nil, &out)
	return out.Config, err
}

// PutTunnelConfiguration saves a tunnel's routes.
func (c *Client) PutTunnelConfiguration(ctx context.Context, account, tunnel string, cfg TunnelConfig) error {
	return c.do(ctx, http.MethodPut, "/accounts/"+account+"/cfd_tunnel/"+tunnel+"/configurations",
		map[string]any{"config": cfg}, nil)
}

// DNSRecord is one DNS record.
type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

// DNSRecords lists records with exactly this name.
func (c *Client) DNSRecords(ctx context.Context, zone, name string) ([]DNSRecord, error) {
	var out []DNSRecord
	return out, c.do(ctx, http.MethodGet, "/zones/"+zone+"/dns_records?per_page=100&name="+url.QueryEscape(name), nil, &out)
}

// CreateDNSRecord adds a record.
func (c *Client) CreateDNSRecord(ctx context.Context, zone string, r DNSRecord) (DNSRecord, error) {
	var out DNSRecord
	return out, c.do(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", r, &out)
}

// DeleteDNSRecord removes a record.
func (c *Client) DeleteDNSRecord(ctx context.Context, zone, id string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+id, nil, nil)
}

// AccessApp is a Cloudflare Access application (a login in front).
type AccessApp struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// CreateAccessApp puts a login page in front of a hostname and allows
// only the given e-mail addresses (one-time code by e-mail).
func (c *Client) CreateAccessApp(ctx context.Context, account, name, domain string, emails []string) (AccessApp, error) {
	var app AccessApp
	err := c.do(ctx, http.MethodPost, "/accounts/"+account+"/access/apps", map[string]any{
		"name":                 name,
		"domain":               domain,
		"type":                 "self_hosted",
		"session_duration":     "24h",
		"app_launcher_visible": false,
	}, &app)
	if err != nil {
		return app, err
	}
	include := make([]map[string]any, 0, len(emails))
	for _, e := range emails {
		include = append(include, map[string]any{"email": map[string]string{"email": e}})
	}
	err = c.do(ctx, http.MethodPost, "/accounts/"+account+"/access/apps/"+app.ID+"/policies", map[string]any{
		"name":       "Nestward: allowed people",
		"decision":   "allow",
		"include":    include,
		"precedence": 1,
	}, nil)
	if err != nil {
		// Do not leave an app without a rule behind.
		_ = c.DeleteAccessApp(ctx, account, app.ID)
		return AccessApp{}, err
	}
	return app, nil
}

// DeleteAccessApp removes an Access application.
func (c *Client) DeleteAccessApp(ctx context.Context, account, id string) error {
	return c.do(ctx, http.MethodDelete, "/accounts/"+account+"/access/apps/"+id, nil, nil)
}
