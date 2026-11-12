package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/store"
)

// world is a fake home: machines, their containers and which web pages
// answer.
type world struct {
	mu       sync.Mutex
	now      time.Time
	machines []Machine
	webDown  map[string]bool   // url -> failing
	webErr   map[string]string // url -> this error instead
	probed   []string
}

func (w *world) svc(machine int, name string) *discovery.Service {
	for i := range w.machines[machine].Discovery.Services {
		if w.machines[machine].Discovery.Services[i].Name == name {
			return &w.machines[machine].Discovery.Services[i]
		}
	}
	return nil
}

func newWorld() *world {
	return &world{
		now:     time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
		webDown: map[string]bool{},
		webErr:  map[string]string{},
		machines: []Machine{
			{ID: "tower", Name: "Tower", Local: true, Discovery: &discovery.Result{Available: true, Services: []discovery.Service{
				{Name: "plex", AppName: "Plex", Kind: "app", State: "running", Web: &discovery.WebLink{Scheme: "http", Port: 32400, Path: "/web"}},
				{Name: "immich-postgres", AppName: "PostgreSQL", Kind: "support", State: "running"},
				{Name: "newt", AppName: "Newt", Kind: "app", State: "exited"},
				{Name: "nestward", AppID: "nestward", AppName: "Nestward", Kind: "app", State: "running", Web: &discovery.WebLink{Scheme: "http", Port: 8470}},
				{Name: "jf", AppName: "Jellyfin", Kind: "app", State: "running", Web: &discovery.WebLink{Scheme: "http", Host: "192.168.1.50", Port: 8096}},
			}}},
			{ID: "pve", Name: "pve", Address: "192.168.1.180", Discovery: &discovery.Result{Available: true, Services: []discovery.Service{
				{Name: "paperless", AppName: "Paperless-ngx", Kind: "app", State: "running", Web: &discovery.WebLink{Scheme: "http", Port: 8000}},
			}}},
		},
	}
}

func newMonitor(t *testing.T, w *world) (*Monitor, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Monitor{
		Store: st,
		Machines: func(context.Context) ([]Machine, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			out := make([]Machine, len(w.machines))
			for i, mc := range w.machines {
				if !mc.Local {
					mc.LastSeen = w.now.Add(-30 * time.Second)
				}
				out[i] = mc
			}
			// machine "pve" can be made silent by setting its LastSeen in the world
			for i := range out {
				if !w.machines[i].LastSeen.IsZero() {
					out[i].LastSeen = w.machines[i].LastSeen
				}
			}
			return out, nil
		},
		Probe: func(_ context.Context, url string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.probed = append(w.probed, url)
			if e, ok := w.webErr[url]; ok {
				return errors.New(e)
			}
			if w.webDown[url] {
				return errors.New("connection refused")
			}
			return nil
		},
		LocalHost: func(context.Context) string { return "host.docker.internal" },
		Now:       func() time.Time { return w.now },
	}
	return m, st
}

func run(t *testing.T, m *Monitor, w *world) []Event {
	t.Helper()
	w.now = w.now.Add(time.Minute)
	evs, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func alerts(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		if e.Alert {
			out = append(out, e)
		}
	}
	return out
}

func TestFirstRunIsQuiet(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	if evs := run(t, m, w); len(evs) != 0 {
		t.Errorf("first run should not raise events: %+v", evs)
	}
	checks, _ := m.Checks(context.Background())
	st := map[string]Status{}
	for _, c := range checks {
		st[c.ID] = c.Status
	}
	if st["container/tower/newt"] != Stopped {
		t.Errorf("already stopped container = %q, want stopped", st["container/tower/newt"])
	}
	if st["container/tower/plex"] != Up || st["web/tower/plex"] != Up || st["machine/pve"] != Up {
		t.Errorf("statuses = %v", st)
	}
	if _, ok := st["web/tower/immich-postgres"]; ok {
		t.Error("helpers get no web check")
	}
	if _, ok := st["web/tower/nestward"]; ok {
		t.Error("Nestward must not check its own page")
	}
	want := []string{"http://host.docker.internal:32400/web", "http://192.168.1.50:8096", "http://192.168.1.180:8000"}
	for _, u := range want {
		found := false
		for _, p := range w.probed {
			found = found || p == u
		}
		if !found {
			t.Errorf("did not probe %s (probed %v)", u, w.probed)
		}
	}
}

func TestContainerStopsAndComesBack(t *testing.T) {
	w := newWorld()
	m, st := newMonitor(t, w)
	run(t, m, w)
	w.svc(0, "plex").State = "exited"
	failAt := w.now.Add(time.Minute)
	if a := alerts(run(t, m, w)); len(a) != 0 {
		t.Fatalf("one failure must not alert yet: %+v", a)
	}
	a := alerts(run(t, m, w))
	if len(a) != 1 || a[0].CheckID != "container/tower/plex" || a[0].To != Down || !a[0].Since.Equal(failAt) {
		t.Fatalf("down alert = %+v", a)
	}
	if a := alerts(run(t, m, w)); len(a) != 0 {
		t.Errorf("no repeat alerts while down: %+v", a)
	}
	w.svc(0, "plex").State = "running"
	a = alerts(run(t, m, w))
	if len(a) != 1 || a[0].To != Up || !a[0].Since.Equal(failAt) {
		t.Fatalf("recovery alert = %+v", a)
	}
	evs, _ := st.Events(context.Background(), 10)
	if len(evs) != 2 {
		t.Errorf("stored events = %d, want 2", len(evs))
	}
}

func TestUnhealthyCounts(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	w.svc(0, "plex").Health = "unhealthy"
	run(t, m, w)
	if a := alerts(run(t, m, w)); len(a) != 1 || a[0].Detail != "running (unhealthy)" {
		t.Errorf("unhealthy alert = %+v", a)
	}
}

func TestWebPageDown(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	w.webDown["http://192.168.1.180:8000"] = true
	run(t, m, w)
	a := alerts(run(t, m, w))
	if len(a) != 1 || a[0].CheckID != "web/pve/paperless" || !strings.Contains(a[0].Detail, "refused") {
		t.Errorf("web alert = %+v", a)
	}
}

func TestStoppedContainerStartingIsNotAnAlert(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	w.svc(0, "newt").State = "running"
	evs := run(t, m, w)
	if len(evs) != 1 || evs[0].Alert || evs[0].To != Up {
		t.Errorf("events = %+v", evs)
	}
}

func TestSilentMachine(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	w.machines[1].LastSeen = w.now // stops reporting from now
	w.now = w.now.Add(2 * time.Minute)
	a := alerts(run(t, m, w)) // 3 minutes of silence
	if len(a) != 1 || a[0].Kind != KindMachine || a[0].To != Down {
		t.Fatalf("machine alert = %+v", a)
	}
	// Its services are kept, not alerted and not forgotten.
	checks, _ := m.Checks(context.Background())
	kept := false
	for _, c := range checks {
		kept = kept || c.ID == "container/pve/paperless"
	}
	if !kept {
		t.Error("services of a silent machine must be kept")
	}
	w.machines[1].LastSeen = time.Time{}
	a = alerts(run(t, m, w))
	if len(a) != 1 || a[0].Kind != KindMachine || a[0].To != Up {
		t.Errorf("machine back = %+v", a)
	}
}

func TestRemovedContainerIsForgotten(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	svcs := w.machines[0].Discovery.Services
	w.machines[0].Discovery.Services = svcs[1:] // plex removed
	if evs := run(t, m, w); len(evs) != 0 {
		t.Errorf("removing a container is not an outage: %+v", evs)
	}
	checks, _ := m.Checks(context.Background())
	for _, c := range checks {
		if c.Service == "plex" {
			t.Errorf("plex check still there: %s", c.ID)
		}
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	w := newWorld()
	m, st := newMonitor(t, w)
	run(t, m, w)
	w.svc(0, "plex").State = "exited"
	run(t, m, w)
	run(t, m, w) // plex down
	m2 := &Monitor{Store: st, Machines: m.Machines, Probe: m.Probe, LocalHost: m.LocalHost, Now: m.Now}
	if a := alerts(run(t, m2, w)); len(a) != 0 {
		t.Errorf("a restart must not repeat the alert: %+v", a)
	}
	w.svc(0, "plex").State = "running"
	if a := alerts(run(t, m2, w)); len(a) != 1 || a[0].To != Up {
		t.Errorf("recovery after restart = %+v", a)
	}
}

func TestHTTPProbe(t *testing.T) {
	codes := map[string]int{"/ok": 200, "/login": 401, "/redirect": 302, "/broken": 503}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/somewhere-else", http.StatusFound)
			return
		}
		w.WriteHeader(codes[r.URL.Path])
	}))
	defer srv.Close()
	for path, code := range codes {
		err := HTTPProbe(context.Background(), srv.URL+path)
		if (code >= 500) != (err != nil) {
			t.Errorf("%s (%d): err = %v", path, code, err)
		}
	}
	if err := HTTPProbe(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Error("closed port must fail")
	}
}

func TestFinishedContainerIsNotAnOutage(t *testing.T) {
	w := newWorld()
	m, _ := newMonitor(t, w)
	run(t, m, w)
	// A backup job ran and exited cleanly, like nextcloud-aio-borgbackup.
	p := w.svc(0, "plex")
	p.State, p.Finished = "exited", true
	for i := 0; i < 3; i++ {
		if a := alerts(run(t, m, w)); len(a) != 0 {
			t.Fatalf("clean exit raised alerts: %+v", a)
		}
	}
	checks, _ := m.Checks(context.Background())
	for _, c := range checks {
		if c.ID == "container/tower/plex" && c.Status != Stopped {
			t.Errorf("finished container = %s, want stopped", c.Status)
		}
	}
	// A crash (non-zero exit) still counts.
	p.Finished = false
	run(t, m, w)
	p.State = "running"
	run(t, m, w)
	p.State = "exited"
	run(t, m, w)
	if a := alerts(run(t, m, w)); len(a) != 1 {
		t.Errorf("crash alerts = %+v", a)
	}
}

func TestGuessedPageThatNeverAnswersIsNotAProblem(t *testing.T) {
	w := newWorld()
	w.webErr["http://192.168.1.180:8000"] = "dial tcp: connection refused"
	w.webErr["http://host.docker.internal:32400/web"] = "answered 500 Internal Server Error"
	m, _ := newMonitor(t, w)
	for i := 0; i < 3; i++ {
		run(t, m, w)
	}
	st := map[string]Status{}
	checks, _ := m.Checks(context.Background())
	for _, c := range checks {
		st[c.ID] = c.Status
	}
	if st["web/pve/paperless"] != Stopped {
		t.Errorf("never answered = %s, want stopped (not checked)", st["web/pve/paperless"])
	}
	if st["web/tower/plex"] != Down {
		t.Errorf("answers with a server error = %s, want down", st["web/tower/plex"])
	}
	// Once it has worked, losing it is an outage with an alert.
	delete(w.webErr, "http://192.168.1.180:8000")
	run(t, m, w)
	w.webDown["http://192.168.1.180:8000"] = true
	run(t, m, w)
	if a := alerts(run(t, m, w)); len(a) != 1 || a[0].CheckID != "web/pve/paperless" {
		t.Errorf("after working once: %+v", a)
	}
}
