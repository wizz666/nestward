package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/agent"
	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/pki"
	"github.com/wizz666/nestward/internal/store"
)

// agentPort starts the core's encrypted agent port for a test.
func agentPort(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ca, err := pki.EnsureCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Agents = &AgentGate{CA: ca, Port: 8472, Store: s.Store}
	cert, err := ca.ServerCert()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.AgentHandler())
	ts.TLS = ca.ServerConfig(cert)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func TestAgentEnrollAndReport(t *testing.T) {
	s, st := newServer(t)
	ts := agentPort(t, s)
	ctx := context.Background()
	fp := pki.Fingerprint(s.Agents.CA.Cert)

	// The web page makes a code and a ready-to-paste command.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/agents/code", nil)
	req.Host = "192.168.1.10:8470"
	req.Header.Set("X-Nestward", "1")
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	code, _ := out["code"].(string)
	if rec.Code != 200 || code == "" || !strings.Contains(out["command"].(string), "--core https://192.168.1.10:8472 --code "+code+" --fingerprint "+fp) {
		t.Fatalf("code = %d %v", rec.Code, out)
	}

	dir := t.TempDir()
	if err := agent.Enroll(ctx, dir, ts.URL, "WRONG-CODE", fp, "pve", "pve"); err == nil {
		t.Error("wrong code accepted")
	}
	if err := agent.Enroll(ctx, dir, ts.URL, code, "sha256:00", "pve", "pve"); err == nil {
		t.Error("wrong fingerprint accepted")
	}
	if err := agent.Enroll(ctx, dir, ts.URL, code, fp, "pve", "pve"); err != nil {
		t.Fatal(err)
	}
	if err := agent.Enroll(ctx, t.TempDir(), ts.URL, code, fp, "evil", "evil"); err == nil {
		t.Error("a code must work only once")
	}

	id, client, err := agent.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent.Agent{CoreURL: id.Core, Client: client, Collect: func(context.Context) agent.Payload {
		return agent.Payload{Hardware: fakeHW("pve"), Discovery: &discovery.Result{Available: true,
			Services: []discovery.Service{{Name: "kuma", Kind: "app", AppName: "Uptime Kuma", State: "running"}}}}
	}}
	if err := a.Send(ctx); err != nil {
		t.Fatal(err)
	}
	find := func() *store.Machine {
		ms, _ := st.Machines(ctx)
		for i := range ms {
			if ms[i].ID == "pve" {
				return &ms[i]
			}
		}
		return nil
	}
	if m := find(); m == nil || m.Source != "agent" || m.Address != "127.0.0.1" || m.Discovery == nil {
		t.Fatalf("pve = %+v", m)
	}

	// An agent cannot report as another machine.
	a.Collect = func(context.Context) agent.Payload { return agent.Payload{Hardware: fakeHW("tower")} }
	if err := a.Send(ctx); err == nil || !strings.Contains(err.Error(), "wrong_machine") {
		t.Errorf("impersonation: %v", err)
	}

	// Without a certificate: refused.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.PinnedConfig(fp)}}
	resp, err := plain.Post(ts.URL+"/api/v1/agent/report", "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != 401 {
		t.Errorf("no certificate: %v %v", resp, err)
	}

	// Removing the machine revokes the certificate at once.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/api/v1/agents/pve", nil)
	req.Header.Set("X-Nestward", "1")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("remove: %d", rec.Code)
	}
	a.Collect = func(context.Context) agent.Payload { return agent.Payload{Hardware: fakeHW("pve")} }
	if err := a.Send(ctx); !errors.Is(err, agent.ErrRemoved) {
		t.Errorf("after removal: %v", err)
	}
	if err := a.Run(ctx); !errors.Is(err, agent.ErrRemoved) {
		t.Errorf("Run must stop when removed: %v", err)
	}
	if m := find(); m != nil {
		t.Errorf("machine not forgotten: %+v", m)
	}

	// A new code brings the same machine back, in the same folder.
	if err := agent.MarkRemoved(dir); err != nil || !agent.Removed(dir) {
		t.Fatal("removal not remembered", err)
	}
	code2, _ := s.Agents.NewCode()
	if err := agent.Enroll(ctx, dir, ts.URL, code2, fp, "pve", "pve"); err != nil {
		t.Fatal(err)
	}
	if agent.Removed(dir) {
		t.Error("re-adding must clear the removed mark")
	}
	id, client, _ = agent.Load(dir)
	a.Client = client
	if err := a.Send(ctx); err != nil {
		t.Errorf("after re-adding: %v", err)
	}
}

func TestAgentPortRefusesTooManyBadCodes(t *testing.T) {
	g := &AgentGate{}
	good, _ := g.NewCode()
	for i := 0; i < 10; i++ {
		g.useCode("NOPE")
	}
	if g.useCode(good) {
		t.Error("after 10 wrong codes the door must stay shut for a while")
	}
}

func TestAgentCannotTakeCoreIdentity(t *testing.T) {
	s, _ := newServer(t)
	ts := agentPort(t, s)
	code, _ := s.Agents.NewCode()
	err := agent.Enroll(context.Background(), t.TempDir(), ts.URL, code, pki.Fingerprint(s.Agents.CA.Cert), "core", "core")
	if err == nil || !strings.Contains(err.Error(), "same_as_core") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckNow(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/check", nil)
	req.Header.Set("X-Nestward", "1")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "checked") {
		t.Errorf("check now = %d %s", rec.Code, rec.Body.String())
	}
}
