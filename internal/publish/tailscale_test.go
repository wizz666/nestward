package publish

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/wizz666/nestward/internal/config"
)

type fakeTailnet struct {
	started   map[string]string // name -> target
	stopped   []string
	failOn    string
	canDelete bool // as if a Tailscale API key were set
}

func (f *fakeTailnet) Start(_ context.Context, name, target, key string) (string, error) {
	if name == f.failOn {
		return "", errors.New("tailscale: invalid key")
	}
	if key == "" {
		return "", errors.New("no key")
	}
	if f.started == nil {
		f.started = map[string]string{}
	}
	f.started[name] = target
	return "https://" + name + ".tail1234.ts.net", nil
}

func (f *fakeTailnet) Stop(_ context.Context, name string, forget bool) (bool, error) {
	if forget {
		f.stopped = append(f.stopped, name)
	}
	delete(f.started, name)
	return f.canDelete && forget, nil
}

func jellyfin() Request {
	return Request{Provider: Tailscale, Name: "jellyfin", Target: "http://192.168.1.92:8096",
		App: "Jellyfin", AppID: "jellyfin", Login: "own"}
}

func TestTailscalePublishAndUndo(t *testing.T) {
	e, cf := setup(t)
	ts := &fakeTailnet{}
	e.Tailnet = ts
	ctx := context.Background()

	if _, err := e.Publish(ctx, jellyfin()); !errors.Is(err, ErrTailscaleNotConfigured) {
		t.Errorf("no key: %v", err)
	}
	if err := e.Settings.Set(ctx, config.TailscaleAuthKey, "not-a-key"); err == nil {
		t.Error("keys must start with tskey-")
	}
	e.Settings.Set(ctx, config.TailscaleAuthKey, "tskey-auth-test")
	if o := e.Options(ctx); !o.Tailscale || !o.TSKey {
		t.Errorf("options = %+v", o)
	}

	// Video apps need no Cloudflare warning here, and no Cloudflare call is made.
	calls := len(cf.Calls)
	p, err := e.Publish(ctx, jellyfin())
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://jellyfin.tail1234.ts.net" || p.Hostname != "jellyfin.tail1234.ts.net" || ts.started["jellyfin"] != "http://192.168.1.92:8096" {
		t.Errorf("publication = %+v", p)
	}
	if len(cf.Calls) != calls {
		t.Errorf("tailscale publishing called Cloudflare: %v", cf.Calls[calls:])
	}
	if _, err := e.Publish(ctx, jellyfin()); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same name twice: %v", err)
	}

	// After a restart the node comes back.
	delete(ts.started, "jellyfin")
	e.Restore(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if ts.started["jellyfin"] == "" {
		t.Error("restore did not start the node")
	}

	u, err := e.Undo(ctx, p.ID)
	if err != nil || u.Status != Removed || len(ts.stopped) != 1 {
		t.Errorf("undo = %+v %v stopped %v", u, err, ts.stopped)
	}
	// The name is free again.
	if _, err := e.Publish(ctx, jellyfin()); err != nil {
		t.Errorf("publish after undo: %v", err)
	}
}

func TestTailscaleFailureLeavesNothing(t *testing.T) {
	e, _ := setup(t)
	ts := &fakeTailnet{failOn: "jellyfin"}
	e.Tailnet = ts
	ctx := context.Background()
	e.Settings.Set(ctx, config.TailscaleAuthKey, "tskey-auth-test")
	p, err := e.Publish(ctx, jellyfin())
	if err == nil || p.Status != Failed {
		t.Fatalf("expected failure: %+v %v", p, err)
	}
	if len(ts.stopped) != 1 {
		t.Error("a half-joined node must be removed")
	}
}

func TestTailscaleLoopbackRefused(t *testing.T) {
	e, _ := setup(t)
	e.Tailnet = &fakeTailnet{}
	r := jellyfin()
	r.Target = "http://localhost:8096"
	if _, err := e.Publish(context.Background(), r); !errors.Is(err, ErrLoopback) {
		t.Errorf("err = %v", err)
	}
}

func TestTailscaleUndoStepNames(t *testing.T) {
	for _, canDelete := range []bool{false, true} {
		e, _ := setup(t)
		e.Tailnet = &fakeTailnet{canDelete: canDelete}
		ctx := context.Background()
		e.Settings.Set(ctx, config.TailscaleAuthKey, "tskey-auth-test")
		p, _ := e.Publish(ctx, jellyfin())
		u, err := e.Undo(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := "tailscale_node_logged_out"
		if canDelete {
			want = "tailscale_node_removed"
		}
		if last := u.Steps[len(u.Steps)-1].Action; last != want {
			t.Errorf("api key %v: last step %q, want %q", canDelete, last, want)
		}
	}
}
