package alert

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/notify"
	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/store"
)

var noon = time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)

func ev(kind, machine, svc, app string, to monitor.Status, since time.Time) monitor.Event {
	from := monitor.Up
	if to == monitor.Up {
		from = monitor.Down
	}
	return monitor.Event{At: noon, Kind: kind, MachineID: machine, Machine: machine, Service: svc, App: app,
		From: from, To: to, Since: since, Alert: true}
}

func TestComposeSingleAndGrouped(t *testing.T) {
	sv := i18n.MustLoad().For("sv")
	at := noon.Add(-3 * time.Minute)
	msgs := Compose([]monitor.Event{
		ev(monitor.KindContainer, "Tower", "plex", "Plex", monitor.Down, at),
		ev(monitor.KindWeb, "Tower", "plex", "Plex", monitor.Down, at), // same problem, one line
	}, sv, noon)
	if len(msgs) != 1 || msgs[0].Title != "Plex på Tower fungerar inte" || msgs[0].Body != "Den slutade köra 11:57." {
		t.Fatalf("single = %+v", msgs)
	}
	msgs = Compose([]monitor.Event{
		ev(monitor.KindWeb, "Tower", "plex", "Plex", monitor.Down, at),
		ev(monitor.KindContainer, "Tower", "immich", "Immich", monitor.Down, at),
		ev(monitor.KindWeb, "pve", "paperless", "Paperless-ngx", monitor.Up, noon.Add(-75*time.Minute)),
	}, sv, noon)
	if len(msgs) != 2 {
		t.Fatalf("grouped = %+v", msgs)
	}
	if msgs[0].Title != "2 tjänster på Tower fungerar inte" || !strings.Contains(msgs[0].Body, "• Plex: Webbsidan har inte svarat sedan 11:57.") {
		t.Errorf("group = %+v", msgs[0])
	}
	if msgs[1].Title != "Paperless-ngx på pve fungerar igen" || msgs[1].Body != "Den var nere i 1 tim 15 min." {
		t.Errorf("recovery = %+v", msgs[1])
	}
}

func TestComposeMachineHidesServices(t *testing.T) {
	en := i18n.MustLoad().For("en")
	msgs := Compose([]monitor.Event{
		ev(monitor.KindMachine, "pve", "", "", monitor.Down, noon.Add(-2*time.Minute)),
		ev(monitor.KindWeb, "pve", "paperless", "Paperless-ngx", monitor.Down, noon),
		{Kind: monitor.KindContainer, MachineID: "Tower", To: monitor.Up, From: monitor.Stopped}, // not an alert
	}, en, noon)
	if len(msgs) != 1 || msgs[0].Title != "pve is not reporting" || !strings.Contains(msgs[0].Body, "since 11:58") {
		t.Errorf("machine = %+v", msgs)
	}
}

func TestDuration(t *testing.T) {
	sv := i18n.MustLoad().For("sv")
	cases := map[time.Duration]string{30 * time.Second: "mindre än en minut", 7 * time.Minute: "7 min", 125 * time.Minute: "2 tim 5 min"}
	for d, want := range cases {
		if got := Duration(d, sv); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := Clock(noon.Add(-25*time.Hour), noon, sv); !strings.HasPrefix(got, "2026-11-06") {
		t.Errorf("yesterday = %q", got)
	}
}

type fakeSender struct {
	sent []notify.Message
	err  error
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func newAlerter(t *testing.T) (*Alerter, *fakeSender, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.OpenBox(filepath.Join(dir, "k"))
	f := &fakeSender{}
	now := noon
	a := &Alerter{Settings: &config.Settings{Store: st, Box: box}, I18n: i18n.MustLoad(),
		NewSender: func(url, topic, token string) Sender { return f }, Now: func() time.Time { return now }}
	return a, f, &now
}

func TestAlerterSendsAndReports(t *testing.T) {
	a, f, now := newAlerter(t)
	ctx := context.Background()
	if err := a.SendTest(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("unconfigured: %v", err)
	}
	a.Settings.Set(ctx, config.NtfyTopic, "hemvakten-larm")
	a.Settings.Set(ctx, config.NtfyToken, "tk_secret")
	a.Settings.Set(ctx, config.AlertsLang, "sv")
	*now = now.Add(time.Minute)
	if err := a.SendTest(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].Title != "Test från Hemvakten" {
		t.Errorf("sent = %+v", f.sent)
	}
	if err := a.SendTest(ctx); !errors.Is(err, ErrTooSoon) {
		t.Errorf("second test right away: %v", err)
	}
	a.HandleEvents(ctx, []monitor.Event{ev(monitor.KindContainer, "Tower", "plex", "Plex", monitor.Down, noon)})
	if len(f.sent) != 2 {
		t.Errorf("event alert not sent: %+v", f.sent)
	}
	st := a.Status(ctx)
	if !st.Configured || strings.Contains(st.Target, "tk_") || strings.Contains(st.Target, "larm") || st.LastError != "" {
		t.Errorf("status = %+v (topic and token must be hidden)", st)
	}
	f.err = errors.New("ntfy answered 403")
	a.HandleEvents(ctx, []monitor.Event{ev(monitor.KindContainer, "Tower", "plex", "Plex", monitor.Up, noon)})
	if a.Status(ctx).LastError == "" {
		t.Error("send error must show on the status page")
	}
}

func TestHeartbeat(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	a, _, _ := newAlerter(t)
	h := &Heartbeat{Settings: a.Settings}
	ctx := context.Background()
	if err := h.Beat(ctx); err != nil || hits != 0 {
		t.Errorf("no address set: err %v hits %d", err, hits)
	}
	a.Settings.Set(ctx, config.HeartbeatURL, srv.URL+"/ping/secret-token")
	if err := h.Beat(ctx); err != nil || hits != 1 || h.Status(ctx).LastOK.IsZero() {
		t.Errorf("beat: err %v hits %d", err, hits)
	}
	a.Settings.Set(ctx, config.HeartbeatURL, "http://127.0.0.1:1/ping/secret-token/fail")
	h.Beat(ctx)
	st := h.Status(ctx)
	if st.LastError == "" || strings.Contains(st.LastError, "secret-token") || strings.Contains(st.Target, "secret") {
		t.Errorf("status leaks the token or lacks the error: %+v", st)
	}
}
