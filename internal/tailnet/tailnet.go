// Package tailnet publishes a home service inside the user's Tailscale
// network. Every published service becomes its own small Tailscale node
// (for example "jellyfin"), reachable only from the user's own devices,
// with an https address like https://jellyfin.tail1234.ts.net.
package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

// Runner keeps the running nodes.
type Runner struct {
	Dir string // where each node keeps its identity, one folder per node
	Log *slog.Logger
	// APIKey returns an optional Tailscale API access token. With it,
	// undoing a publication deletes the device from the tailnet instead
	// of only switching it off.
	APIKey  func(ctx context.Context) string
	APIBase string // DefaultAPIBase when empty (tests use a fake)

	mu    sync.Mutex
	nodes map[string]*node
}

type node struct {
	srv     *tsnet.Server
	servers []*http.Server
	url     string
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// ErrNoKey means a node must join the tailnet but no auth key is set.
var ErrNoKey = errors.New("tailscale_no_key")

func proxyTo(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = &http.Transport{
		// Home apps often use self-signed certificates inside the home.
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	base := p.Director
	p.Director = func(req *http.Request) {
		base(req)
		req.Host = u.Host
	}
	return p, nil
}

// Start joins the tailnet as a node called name and forwards its web
// traffic to target. authKey is only needed the first time; afterwards
// the node's identity is kept in Dir. It returns the address to use.
func (r *Runner) Start(ctx context.Context, name, target, authKey string) (string, error) {
	r.mu.Lock()
	if r.nodes == nil {
		r.nodes = map[string]*node{}
	}
	if n, ok := r.nodes[name]; ok {
		r.mu.Unlock()
		return n.url, nil
	}
	r.mu.Unlock()

	dir := filepath.Join(r.Dir, name)
	if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err != nil && authKey == "" {
		return "", ErrNoKey
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	handler, err := proxyTo(target)
	if err != nil {
		return "", err
	}
	srv := &tsnet.Server{
		Hostname: name,
		Dir:      dir,
		AuthKey:  authKey,
		Logf:     func(string, ...any) {}, // tsnet is very chatty
		UserLogf: func(format string, args ...any) { r.log().Info(fmt.Sprintf(format, args...), "node", name) },
	}
	upCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	st, err := srv.Up(upCtx)
	if err != nil {
		srv.Close()
		return "", fmt.Errorf("tailscale: %w", err)
	}
	n := &node{srv: srv}

	// Plain http on port 80: traffic inside the tailnet is already
	// encrypted end to end, and it works even without HTTPS certificates.
	ln80, err := srv.Listen("tcp", ":80")
	if err != nil {
		srv.Close()
		return "", err
	}
	n.serve(ln80, handler)
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	n.url = "http://" + host

	// https on port 443 when the tailnet has HTTPS certificates turned on.
	if domains := srv.CertDomains(); len(domains) > 0 {
		ln443, err := srv.ListenTLS("tcp", ":443")
		if err == nil {
			n.serve(ln443, handler)
			n.url = "https://" + domains[0]
		}
	}
	r.mu.Lock()
	r.nodes[name] = n
	r.mu.Unlock()
	return n.url, nil
}

func (n *node) serve(ln net.Listener, h http.Handler) {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second}
	n.servers = append(n.servers, hs)
	go hs.Serve(ln)
}

// Stop shuts a node down. With forget it also logs the node out and
// deletes its identity, so the name is free again; with an API key the
// device is also deleted from the tailnet (removed reports that).
func (r *Runner) Stop(ctx context.Context, name string, forget bool) (removed bool, err error) {
	r.mu.Lock()
	n := r.nodes[name]
	delete(r.nodes, name)
	r.mu.Unlock()
	dir := filepath.Join(r.Dir, name)
	if n == nil && forget {
		// Not running (e.g. after a restart): start it briefly so it can
		// log out properly.
		if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err == nil {
			srv := &tsnet.Server{Hostname: name, Dir: dir, Logf: func(string, ...any) {}}
			upCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			_, err := srv.Up(upCtx)
			cancel()
			if err == nil {
				n = &node{srv: srv}
			} else {
				srv.Close()
			}
		}
	}
	var errs []error
	nodeID := ""
	if n != nil {
		for _, hs := range n.servers {
			hs.Close()
		}
		if forget {
			if lc, err := n.srv.LocalClient(); err == nil {
				if st, err := lc.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
					nodeID = string(st.Self.ID)
				}
				if err := lc.Logout(ctx); err != nil {
					errs = append(errs, fmt.Errorf("tailscale logout: %w", err))
				}
			}
		}
		n.srv.Close()
	}
	if forget && len(errs) == 0 {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
		if key := r.apiKey(ctx); key != "" && nodeID != "" {
			if err := DeleteDevice(ctx, r.APIBase, key, nodeID); err != nil {
				// The device is already off; only the list entry remains.
				r.log().Warn("could not delete tailscale device", "node", name, "err", err)
			} else {
				removed = true
			}
		}
	}
	return removed, errors.Join(errs...)
}

func (r *Runner) apiKey(ctx context.Context) string {
	if r.APIKey == nil {
		return ""
	}
	return r.APIKey(ctx)
}

// Close stops every node but keeps them in the tailnet (used at
// shutdown; they come back on the next start).
func (r *Runner) Close() {
	r.mu.Lock()
	names := make([]string, 0, len(r.nodes))
	for name := range r.nodes {
		names = append(names, name)
	}
	r.mu.Unlock()
	for _, name := range names {
		_, _ = r.Stop(context.Background(), name, false)
	}
}
