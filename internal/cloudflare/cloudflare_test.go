package cloudflare_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/cloudflare"
	"github.com/wizz666/nestward/internal/cloudflare/cftest"
)

func TestClientAgainstFake(t *testing.T) {
	f := cftest.New("tok")
	srv := f.Server()
	defer srv.Close()
	c := &cloudflare.Client{Token: "tok", Base: srv.URL}
	ctx := context.Background()

	accs, err := c.Accounts(ctx)
	if err != nil || len(accs) != 1 {
		t.Fatalf("accounts = %v, %v", accs, err)
	}
	tuns, _ := c.Tunnels(ctx, "acc1")
	remote := 0
	for _, tn := range tuns {
		if tn.Remote() {
			remote++
		}
	}
	if len(tuns) != 2 || remote != 1 {
		t.Errorf("tunnels = %+v", tuns)
	}
	cfg, err := c.TunnelConfiguration(ctx, "acc1", "tun1")
	if err != nil || len(cfg.Ingress) != 2 {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if _, ok := cfg.Other["warp-routing"]; !ok {
		t.Error("unknown fields must be kept")
	}
	if err := c.PutTunnelConfiguration(ctx, "acc1", "tun1", cfg); err != nil {
		t.Fatal(err)
	}
	if f.Configs["tun1"]["warp-routing"] == nil {
		t.Error("saving dropped warp-routing")
	}

	bad := &cloudflare.Client{Token: "wrong", Base: srv.URL}
	_, err = bad.Accounts(ctx)
	var apiErr *cloudflare.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 || !strings.Contains(err.Error(), "Authentication") {
		t.Errorf("bad token: %v", err)
	}

	app, err := c.CreateAccessApp(ctx, "acc1", "Glances", "glances.example.com", []string{"a@example.com"})
	if err != nil || app.ID == "" || len(f.Policy[app.ID]) != 1 {
		t.Errorf("access app = %+v, %v", app, err)
	}
}
