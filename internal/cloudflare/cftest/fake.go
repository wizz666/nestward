// Package cftest is an in-memory fake of the Cloudflare API parts that
// Nestward uses, for tests. Nothing here talks to the real Cloudflare.
package cftest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Fake holds the fake account's state.
type Fake struct {
	mu      sync.Mutex
	Token   string
	Account string
	Zones   map[string]string           // id -> name
	Tunnels map[string]map[string]any   // id -> tunnel json
	Configs map[string]map[string]any   // tunnel id -> config
	DNS     map[string]map[string]any   // record id -> record (with "zone_id")
	Apps    map[string]map[string]any   // access app id -> app
	Policy  map[string][]map[string]any // app id -> policies
	FailOn  string                      // "METHOD /path-prefix" to answer with an error
	// HideAccounts makes /accounts empty, like a token without the
	// Account Settings Read right.
	HideAccounts bool
	Calls        []string
	next         int
}

// New returns a fake with one account, the zone example.com and the
// remote-managed tunnel "home" that already serves one hostname.
func New(token string) *Fake {
	return &Fake{
		Token:   token,
		Account: "acc1",
		Zones:   map[string]string{"zone1": "example.com"},
		Tunnels: map[string]map[string]any{
			"tun1": {"id": "tun1", "name": "home", "status": "healthy", "config_src": "cloudflare"},
			"tun2": {"id": "tun2", "name": "old", "status": "down", "config_src": "local"},
		},
		Configs: map[string]map[string]any{
			"tun1": {
				"ingress": []any{
					map[string]any{"hostname": "existing.example.com", "service": "http://192.168.1.5:80"},
					map[string]any{"service": "http_status:404"},
				},
				"warp-routing": map[string]any{"enabled": false},
			},
		},
		DNS: map[string]map[string]any{
			"rec0": {"id": "rec0", "zone_id": "zone1", "type": "CNAME", "name": "existing.example.com", "content": "tun1.cfargotunnel.com", "proxied": true},
		},
		Apps:   map[string]map[string]any{},
		Policy: map[string][]map[string]any{},
	}
}

// Server starts an HTTP server; use its URL as the client's Base.
func (f *Fake) Server() *httptest.Server { return httptest.NewServer(f) }

func (f *Fake) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s%d", prefix, f.next)
}

func write(w http.ResponseWriter, code int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	ok := code < 300
	body := map[string]any{"success": ok, "errors": []any{}, "result": result}
	if !ok {
		body["errors"] = []any{map[string]any{"code": code, "message": fmt.Sprint(result)}}
		body["result"] = nil
	}
	json.NewEncoder(w).Encode(body)
}

// ServeHTTP answers like the Cloudflare API.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.Calls = append(f.Calls, call)
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		write(w, 403, "Authentication error")
		return
	}
	if f.FailOn != "" && strings.HasPrefix(call, f.FailOn) {
		write(w, 500, "simulated failure")
		return
	}
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/accounts":
		if f.HideAccounts {
			write(w, 200, []any{})
			return
		}
		write(w, 200, []any{map[string]any{"id": f.Account, "name": "Test account"}})
	case r.Method == "GET" && r.URL.Path == "/zones":
		var out []any
		for id, n := range f.Zones {
			out = append(out, map[string]any{"id": id, "name": n, "account": map[string]any{"id": f.Account, "name": "Test account"}})
		}
		write(w, 200, out)
	case len(p) == 3 && p[0] == "accounts" && p[2] == "cfd_tunnel" && r.Method == "GET":
		var out []any
		for _, t := range f.Tunnels {
			out = append(out, t)
		}
		write(w, 200, out)
	case len(p) == 5 && p[2] == "cfd_tunnel" && p[4] == "configurations":
		if r.Method == "GET" {
			write(w, 200, map[string]any{"tunnel_id": p[3], "config": f.Configs[p[3]]})
			return
		}
		cfg, _ := body["config"].(map[string]any)
		f.Configs[p[3]] = cfg
		write(w, 200, map[string]any{"tunnel_id": p[3], "config": cfg})
	case len(p) == 3 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "GET":
		name := r.URL.Query().Get("name")
		var out []any
		for _, rec := range f.DNS {
			if rec["zone_id"] == p[1] && rec["name"] == name {
				out = append(out, rec)
			}
		}
		write(w, 200, out)
	case len(p) == 3 && p[0] == "zones" && p[2] == "dns_records" && r.Method == "POST":
		id := f.id("rec")
		body["id"], body["zone_id"] = id, p[1]
		f.DNS[id] = body
		write(w, 200, body)
	case len(p) == 4 && p[2] == "dns_records" && r.Method == "DELETE":
		if _, ok := f.DNS[p[3]]; !ok {
			write(w, 404, "Record not found")
			return
		}
		delete(f.DNS, p[3])
		write(w, 200, map[string]any{"id": p[3]})
	case len(p) == 4 && p[2] == "access" && p[3] == "apps" && r.Method == "POST":
		id := f.id("app")
		body["id"] = id
		f.Apps[id] = body
		write(w, 200, body)
	case len(p) == 6 && p[3] == "apps" && p[5] == "policies" && r.Method == "POST":
		f.Policy[p[4]] = append(f.Policy[p[4]], body)
		write(w, 200, body)
	case len(p) == 5 && p[3] == "apps" && r.Method == "DELETE":
		if _, ok := f.Apps[p[4]]; !ok {
			write(w, 404, "Application not found")
			return
		}
		delete(f.Apps, p[4])
		delete(f.Policy, p[4])
		write(w, 200, map[string]any{"id": p[4]})
	default:
		write(w, 404, "no route "+call)
	}
}

// Hostnames returns the hostnames in a tunnel's ingress, in order.
func (f *Fake) Hostnames(tunnel string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	ing, _ := f.Configs[tunnel]["ingress"].([]any)
	for _, r := range ing {
		m, _ := r.(map[string]any)
		h, _ := m["hostname"].(string)
		if h == "" {
			h = "(catch-all)"
		}
		out = append(out, h)
	}
	return out
}
