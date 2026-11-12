package publish

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wizz666/nestward/internal/cloudflare/cftest"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

func setup(t *testing.T) (*Engine, *cftest.Fake) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	s := &config.Settings{Store: st, Box: box}
	f := cftest.New("cf-token")
	srv := f.Server()
	t.Cleanup(srv.Close)
	ctx := context.Background()
	s.Set(ctx, config.CloudflareToken, "cf-token")
	s.Set(ctx, config.PublishEmails, "alex@example.com")
	return &Engine{Settings: s, Store: st, CloudflareBase: srv.URL}, f
}

func glances() Request {
	return Request{Name: "glances", ZoneID: "zone1", TunnelID: "tun1", Target: "http://192.168.1.92:61208",
		App: "Glances", AppID: "glances", Login: "none", Service: "glances", MachineID: "tower"}
}

func TestPublishAndUndo(t *testing.T) {
	e, f := setup(t)
	ctx := context.Background()

	o := e.Options(ctx)
	if !o.Configured || o.Error != "" || len(o.Zones) != 1 || len(o.Tunnels) != 2 || o.Emails[0] != "alex@example.com" {
		t.Fatalf("options = %+v", o)
	}

	p, err := e.Publish(ctx, glances())
	if err != nil {
		t.Fatal(err)
	}
	if p.Hostname != "glances.example.com" || !p.Access || p.AccessAppID == "" || p.DNSRecordID == "" || p.Status != Active {
		t.Fatalf("publication = %+v", p)
	}
	// The new route sits before the catch-all; the old route is untouched.
	want := []string{"existing.example.com", "glances.example.com", "(catch-all)"}
	if got := f.Hostnames("tun1"); !reflect.DeepEqual(got, want) {
		t.Errorf("ingress = %v, want %v", got, want)
	}
	if f.DNS[p.DNSRecordID]["content"] != "tun1.cfargotunnel.com" || f.DNS[p.DNSRecordID]["proxied"] != true {
		t.Errorf("dns = %v", f.DNS[p.DNSRecordID])
	}
	if len(f.Policy[p.AccessAppID]) != 1 {
		t.Error("login rule missing")
	}

	// Same name again: refused, nothing changed.
	if _, err := e.Publish(ctx, glances()); !errors.Is(err, ErrNameTaken) {
		t.Errorf("second publish: %v", err)
	}

	u, err := e.Undo(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != Removed || len(f.Apps) != 0 || f.DNS[p.DNSRecordID] != nil {
		t.Errorf("after undo: %+v apps %v", u, f.Apps)
	}
	if got := f.Hostnames("tun1"); !reflect.DeepEqual(got, []string{"existing.example.com", "(catch-all)"}) {
		t.Errorf("ingress after undo = %v", got)
	}
	if _, ok := f.DNS["rec0"]; !ok {
		t.Error("undo must never touch records Nestward did not add")
	}
	if _, err := e.Undo(ctx, p.ID); !errors.Is(err, ErrAlreadyUndone) {
		t.Errorf("double undo: %v", err)
	}
	list, _ := e.List(ctx)
	if len(list) != 1 || len(list[0].Steps) != 6 {
		t.Errorf("history = %+v", list)
	}
}

func TestExistingAddressIsNeverTaken(t *testing.T) {
	e, f := setup(t)
	r := glances()
	r.Name = "existing"
	if _, err := e.Publish(context.Background(), r); !errors.Is(err, ErrNameTaken) {
		t.Errorf("err = %v", err)
	}
	if len(f.DNS) != 1 || len(f.Hostnames("tun1")) != 2 {
		t.Error("nothing may change when the address is taken")
	}
}

func TestFailureRollsBack(t *testing.T) {
	e, f := setup(t)
	f.FailOn = "POST /accounts/acc1/access/apps"
	p, err := e.Publish(context.Background(), glances())
	if err == nil || p.Status != Failed {
		t.Fatalf("expected failure, got %+v %v", p, err)
	}
	if len(f.DNS) != 1 || len(f.Hostnames("tun1")) != 2 {
		t.Errorf("rollback incomplete: dns %v ingress %v", f.DNS, f.Hostnames("tun1"))
	}
}

func TestRules(t *testing.T) {
	e, _ := setup(t)
	ctx := context.Background()
	no := false
	cases := map[string]struct {
		mod  func(*Request)
		want error
	}{
		"bad name":         {func(r *Request) { r.Name = "Glances!" }, ErrBadName},
		"loopback target":  {func(r *Request) { r.Target = "http://127.0.0.1:61208" }, ErrLoopback},
		"docker host":      {func(r *Request) { r.Target = "http://host.docker.internal:61208" }, ErrLoopback},
		"no port":          {func(r *Request) { r.Target = "http://192.168.1.92" }, ErrBadTarget},
		"login off, none":  {func(r *Request) { r.Access = &no }, ErrLoginRequired},
		"login off, ?":     {func(r *Request) { r.Access = &no; r.Login = "" }, ErrLoginRequired},
		"unknown zone":     {func(r *Request) { r.ZoneID = "nope" }, ErrNoZone},
		"local tunnel":     {func(r *Request) { r.TunnelID = "tun2" }, ErrLocalTunnel},
		"video, no accept": {func(r *Request) { r.AppID = "jellyfin" }, ErrVideoWarning},
	}
	for name, c := range cases {
		r := glances()
		c.mod(&r)
		if _, err := e.Publish(ctx, r); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// An app with its own login may skip the login in front.
	r := glances()
	r.Login, r.Access, r.Name = "own", &no, "vault"
	p, err := e.Publish(ctx, r)
	if err != nil || p.Access || p.AccessAppID != "" {
		t.Errorf("own login: %+v %v", p, err)
	}
}

func TestNotConfigured(t *testing.T) {
	e, _ := setup(t)
	ctx := context.Background()
	e.Settings.Set(ctx, config.CloudflareToken, "")
	if o := e.Options(ctx); o.Configured {
		t.Error("no token = not configured")
	}
	if _, err := e.Publish(ctx, glances()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestCheckOutside(t *testing.T) {
	e, _ := setup(t)
	ctx := context.Background()
	p, _ := e.Publish(ctx, glances())
	e.Probe = func(_ context.Context, u string) Check {
		if u != "https://glances.example.com/" {
			t.Errorf("probed %s", u)
		}
		return Check{Result: "protected", Status: 302}
	}
	ch, err := e.CheckOutside(ctx, p.ID)
	if err != nil || ch.Result != "protected" {
		t.Errorf("check = %+v %v", ch, err)
	}
	list, _ := e.List(ctx)
	if list[0].LastCheck == nil || list[0].LastCheck.Result != "protected" {
		t.Error("check result not stored")
	}
}

// A token with only the rights Nestward asks for cannot list accounts;
// the account is found through the domains instead and remembered.
func TestAccountFromZones(t *testing.T) {
	e, f := setup(t)
	f.HideAccounts = true
	ctx := context.Background()
	o := e.Options(ctx)
	if o.Error != "" || len(o.Zones) != 1 {
		t.Fatalf("options = %+v", o)
	}
	if acc, _ := e.Settings.Get(ctx, config.CloudflareAccount); acc != "acc1" {
		t.Errorf("account not remembered: %q", acc)
	}
	if _, err := e.Publish(ctx, glances()); err != nil {
		t.Errorf("publish: %v", err)
	}
}
