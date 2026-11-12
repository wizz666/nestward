package publish

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"github.com/wizz666/nestward/internal/config"
)

// Providers.
const (
	Cloudflare = "cloudflare"
	Tailscale  = "tailscale"
)

// ErrTailscaleNotConfigured means no Tailscale auth key is set.
var ErrTailscaleNotConfigured = errors.New("publish_tailscale_not_configured")

// Tailnet runs one Tailscale node per published service.
type Tailnet interface {
	Start(ctx context.Context, name, target, authKey string) (string, error)
	Stop(ctx context.Context, name string, forget bool) (removed bool, err error)
}

func (e *Engine) publishTailscale(ctx context.Context, req Request, name, target string) (Publication, error) {
	if e.Tailnet == nil {
		return Publication{}, ErrTailscaleNotConfigured
	}
	key, err := e.Settings.Get(ctx, config.TailscaleAuthKey)
	if err != nil {
		return Publication{}, err
	}
	if key == "" {
		return Publication{}, ErrTailscaleNotConfigured
	}
	all, err := e.List(ctx)
	if err != nil {
		return Publication{}, err
	}
	for _, p := range all {
		if p.Provider == Tailscale && p.Status == Active && p.TunnelName == name {
			return Publication{}, ErrNameTaken
		}
	}
	p := Publication{
		ID: newID(), Provider: Tailscale, TunnelName: name, Target: target, MachineID: req.MachineID,
		Service: req.Service, App: req.App, Status: Active, Created: e.now(),
	}
	u, err := e.Tailnet.Start(ctx, name, target, key)
	s := Step{At: e.now(), Action: "tailscale_node_added", OK: err == nil, Detail: name + " → " + target}
	if err != nil {
		s.Detail = err.Error()
		p.Steps = append(p.Steps, s)
		p.Status, p.Error = Failed, err.Error()
		// A half-joined node must not stay behind in the tailnet.
		_, _ = e.Tailnet.Stop(ctx, name, true)
		_ = e.Store.SavePublication(ctx, p.ID, p)
		return p, err
	}
	p.Steps = append(p.Steps, s)
	p.URL = u
	if pu, perr := url.Parse(u); perr == nil {
		p.Hostname = pu.Host
	}
	return p, e.Store.SavePublication(ctx, p.ID, p)
}

func (e *Engine) undoTailscale(ctx context.Context, p *Publication) error {
	if e.Tailnet == nil {
		return ErrTailscaleNotConfigured
	}
	removed, err := e.Tailnet.Stop(ctx, p.TunnelName, true)
	// Logging out switches the device off for good. Without an API key
	// Tailscale keeps it in the admin list until someone removes it there.
	action := "tailscale_node_logged_out"
	if removed {
		action = "tailscale_node_removed"
	}
	s := Step{At: e.now(), Action: action, OK: err == nil, Detail: p.TunnelName}
	if err != nil {
		s.Detail = err.Error()
	}
	p.Steps = append(p.Steps, s)
	return err
}

// Restore starts the Tailscale nodes of active publications again after
// Nestward restarts. Each node keeps its identity, so no key is needed.
func (e *Engine) Restore(ctx context.Context, log *slog.Logger) {
	if e.Tailnet == nil {
		return
	}
	all, err := e.List(ctx)
	if err != nil {
		log.Warn("restore publications", "err", err)
		return
	}
	for _, p := range all {
		if p.Provider != Tailscale || p.Status != Active {
			continue
		}
		key, _ := e.Settings.Get(ctx, config.TailscaleAuthKey)
		if _, err := e.Tailnet.Start(ctx, p.TunnelName, p.Target, key); err != nil {
			log.Warn("could not start tailscale node", "node", p.TunnelName, "err", strings.TrimSpace(err.Error()))
		} else {
			log.Info("tailscale node started", "node", p.TunnelName)
		}
	}
}
