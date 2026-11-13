// Package monitor watches every machine and service and notices when
// something stops working or starts working again.
package monitor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
)

// Status of a check.
type Status string

const (
	Up      Status = "up"
	Down    Status = "down"
	Stopped Status = "stopped" // was already stopped when first seen: not an outage
)

// Kinds of check.
const (
	KindMachine   = "machine"   // an agent machine keeps reporting
	KindContainer = "container" // a container is running and healthy
	KindWeb       = "web"       // a web page answers
)

// Check is the current state of one thing being watched.
type Check struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	MachineID string    `json:"machine_id"`
	Machine   string    `json:"machine"`
	Service   string    `json:"service,omitempty"` // container name
	App       string    `json:"app,omitempty"`     // app name for display
	Target    string    `json:"target,omitempty"`  // checked address (web)
	Status    Status    `json:"status"`
	Since     time.Time `json:"since"` // when the current status began
	Checked   time.Time `json:"checked"`
	Fails     int       `json:"fails"`             // failures in a row while still counted as up
	FailSince time.Time `json:"fail_since"`        // first failure of the current run
	Detail    string    `json:"detail,omitempty"`  // last error, technical
	EverUp    bool      `json:"ever_up,omitempty"` // has worked at least once
}

// Event is a change of status.
type Event struct {
	At        time.Time `json:"at"`
	CheckID   string    `json:"check_id"`
	Kind      string    `json:"kind"`
	MachineID string    `json:"machine_id"`
	Machine   string    `json:"machine"`
	Service   string    `json:"service,omitempty"`
	App       string    `json:"app,omitempty"`
	From      Status    `json:"from"`
	To        Status    `json:"to"`
	Since     time.Time `json:"since"` // when the problem started (for recoveries: when it went down)
	Detail    string    `json:"detail,omitempty"`
	Alert     bool      `json:"alert"` // worth a message to the phone
}

// Machine is what the monitor needs to know about one machine.
type Machine struct {
	ID        string
	Name      string
	Address   string // empty for the local machine
	Local     bool
	LastSeen  time.Time
	Discovery *discovery.Result
}

// Store keeps checks and events.
type Store interface {
	SaveCheck(ctx context.Context, id, machineID string, v any) error
	DeleteCheck(ctx context.Context, id string) error
	Checks(ctx context.Context) ([]json.RawMessage, error)
	AddEvent(ctx context.Context, at time.Time, machineID string, v any) error
}

// Monitor runs the checks.
type Monitor struct {
	Store    Store
	Machines func(ctx context.Context) ([]Machine, error)
	// Probe checks one web address; nil uses an HTTP request.
	Probe func(ctx context.Context, url string) error
	// LocalHost is the address used for web pages on the local machine.
	LocalHost func(ctx context.Context) string
	// OnEvents receives every change (e.g. to send alerts). May be nil.
	OnEvents   func(ctx context.Context, evs []Event)
	Threshold  int           // failures in a row before "down"; default 2
	StaleAfter time.Duration // agent silence before its machine is "down"; default 2 min
	Now        func() time.Time
	Log        *slog.Logger

	runMu  sync.Mutex // one run at a time
	mu     sync.Mutex // guards checks
	checks map[string]*Check
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) threshold() int {
	if m.Threshold > 0 {
		return m.Threshold
	}
	return 2
}

func (m *Monitor) stale() time.Duration {
	if m.StaleAfter > 0 {
		return m.StaleAfter
	}
	return 2 * time.Minute
}

func (m *Monitor) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Monitor) load(ctx context.Context) error {
	if m.checks != nil {
		return nil
	}
	m.checks = map[string]*Check{}
	raws, err := m.Store.Checks(ctx)
	if err != nil {
		return err
	}
	for _, raw := range raws {
		var c Check
		if err := json.Unmarshal(raw, &c); err == nil && c.ID != "" {
			// Older versions called a web page "down" when it never answered
			// at all; that only meant Nestward guessed the wrong address.
			if c.Kind == KindWeb && c.Status == Down && !c.EverUp && !answered(c.Detail) {
				c.Status = Stopped
			}
			m.checks[c.ID] = &c
		}
	}
	return nil
}

// Checks returns a copy of the current checks.
func (m *Monitor) Checks(ctx context.Context) ([]Check, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(ctx); err != nil {
		return nil, err
	}
	out := make([]Check, 0, len(m.checks))
	for _, c := range m.checks {
		out = append(out, *c)
	}
	return out, nil
}

// Run checks every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := m.RunOnce(ctx); err != nil && ctx.Err() == nil {
			m.log().Warn("monitor run failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type webJob struct {
	check *Check
	url   string
}

// RunOnce runs every check once and returns the changes.
func (m *Monitor) RunOnce(ctx context.Context) ([]Event, error) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	machines, err := m.Machines(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if err := m.load(ctx); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	now := m.now()
	seen := map[string]bool{}
	var events []Event
	var jobs []webJob

	get := func(id, kind string, mc Machine, svc discovery.Service) *Check {
		seen[id] = true
		c, ok := m.checks[id]
		if !ok {
			c = &Check{ID: id, Kind: kind}
			m.checks[id] = c
		}
		c.MachineID, c.Machine, c.Service = mc.ID, mc.Name, svc.Name
		c.App = svc.AppName
		return c
	}
	keepMachine := func(id string) {
		for cid, c := range m.checks {
			if c.MachineID == id {
				seen[cid] = true
			}
		}
	}

	for _, mc := range machines {
		if !mc.Local {
			c := get("machine/"+mc.ID, KindMachine, mc, discovery.Service{})
			silent := now.Sub(mc.LastSeen)
			up := silent <= m.stale()
			detail := ""
			if !up {
				detail = "no report for " + silent.Round(time.Second).String()
			}
			// The silence already waited StaleAfter, so no extra threshold.
			if ev := m.apply(c, up, detail, 1, now); ev != nil {
				events = append(events, *ev)
			}
			if !up {
				keepMachine(mc.ID) // its services are not checked, and not forgotten
				continue
			}
		}
		d := mc.Discovery
		if d == nil || !d.Available {
			keepMachine(mc.ID)
			continue
		}
		host := mc.Address
		if mc.Local && m.LocalHost != nil {
			host = m.LocalHost(ctx)
		}
		for _, s := range d.Services {
			c := get("container/"+mc.ID+"/"+s.Name, KindContainer, mc, s)
			if s.Finished || (s.Quiet && s.State != "running") {
				// Ran to the end on its own (backup jobs and the like) or was
				// stopped on purpose: not an outage, and no alert.
				if c.Status != Stopped {
					c.Status, c.Since = Stopped, now
				}
				detail := "exited (0)"
				if !s.Finished {
					detail = "stopped on purpose"
				}
				c.Checked, c.Fails, c.FailSince, c.Detail = now, 0, time.Time{}, detail
				continue
			}
			up := s.State == "running" && s.Health != "unhealthy"
			detail := s.State
			if s.Health != "" {
				detail += " (" + s.Health + ")"
			}
			if ev := m.apply(c, up, detail, m.threshold(), now); ev != nil {
				events = append(events, *ev)
			}
			// Nestward's own page is often published on 127.0.0.1 only and
			// cannot be reached from inside its container; it is up anyway
			// if this code runs.
			if s.Web == nil || s.Kind == discovery.KindSupport || s.AppID == "nestward" || s.Quiet {
				continue
			}
			wc := get("web/"+mc.ID+"/"+s.Name, KindWeb, mc, s)
			h := s.Web.Host
			if h == "" {
				h = host
			}
			if s.State != "running" || h == "" {
				continue // the container check covers it
			}
			wc.Target = webURL(s.Web, h)
			jobs = append(jobs, webJob{check: wc, url: wc.Target})
		}
	}
	m.mu.Unlock()

	// Web pages are checked in parallel, without holding the lock.
	results := m.probeAll(ctx, jobs)

	m.mu.Lock()
	defer m.mu.Unlock()
	for i, j := range jobs {
		err := results[i]
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		// A page that never answered at all, not even with an error page,
		// most likely lives at another address than Nestward guessed. That
		// is not a problem with the app: it is shown as not checked.
		if err != nil && !j.check.EverUp && !answered(detail) && (j.check.Status == "" || j.check.Status == Stopped) {
			j.check.Status, j.check.Checked, j.check.Detail = Stopped, now, detail
			if j.check.Since.IsZero() {
				j.check.Since = now
			}
			continue
		}
		if ev := m.apply(j.check, err == nil, detail, m.threshold(), now); ev != nil {
			events = append(events, *ev)
		}
	}
	// Forget containers that no longer exist.
	for id := range m.checks {
		if !seen[id] {
			delete(m.checks, id)
			if err := m.Store.DeleteCheck(ctx, id); err != nil {
				return events, err
			}
		}
	}
	for id, c := range m.checks {
		if err := m.Store.SaveCheck(ctx, id, c.MachineID, c); err != nil {
			return events, err
		}
	}
	for _, ev := range events {
		if err := m.Store.AddEvent(ctx, ev.At, ev.MachineID, ev); err != nil {
			return events, err
		}
	}
	if len(events) > 0 && m.OnEvents != nil {
		m.OnEvents(ctx, events)
	}
	return events, nil
}

// apply records one observation and returns an event when the status
// changes. Alerts are only raised for real changes: something that was
// up went down, or came back after being down.
func (m *Monitor) apply(c *Check, up bool, detail string, threshold int, now time.Time) *Event {
	c.Checked = now
	prev := c.Status
	if up {
		c.Fails, c.Detail, c.FailSince, c.EverUp = 0, "", time.Time{}, true
		if prev == Up {
			return nil
		}
		downSince := c.Since
		c.Status, c.Since = Up, now
		if prev == "" {
			return nil // first look
		}
		return &Event{At: now, CheckID: c.ID, Kind: c.Kind, MachineID: c.MachineID, Machine: c.Machine,
			Service: c.Service, App: c.App, From: prev, To: Up, Since: downSince, Alert: prev == Down}
	}
	c.Detail = detail
	switch prev {
	case "":
		// Already not working when first seen. Containers that are
		// stopped on purpose must not cause alarms.
		c.Status, c.Since = Down, now
		if c.Kind == KindContainer {
			c.Status = Stopped
		}
		return nil
	case Up:
		if c.Fails == 0 {
			c.FailSince = now
		}
		c.Fails++
		if c.Fails < threshold {
			return nil
		}
		c.Status, c.Since = Down, c.FailSince
		return &Event{At: now, CheckID: c.ID, Kind: c.Kind, MachineID: c.MachineID, Machine: c.Machine,
			Service: c.Service, App: c.App, From: Up, To: Down, Since: c.FailSince, Detail: detail, Alert: true}
	}
	return nil
}

func webURL(w *discovery.WebLink, host string) string {
	return w.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(w.Port)) + w.Path
}

const maxParallel = 8

func (m *Monitor) probeAll(ctx context.Context, jobs []webJob) []error {
	out := make([]error, len(jobs))
	probe := m.Probe
	if probe == nil {
		probe = HTTPProbe
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, url string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = probe(ctx, url)
		}(i, j.url)
	}
	wg.Wait()
	return out
}

var probeClient = &http.Client{
	Timeout: 10 * time.Second,
	// A redirect means the page answers; do not follow it.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		// Home apps often use self-signed certificates. This is a check
		// that the page answers, not a secure connection: nothing is sent.
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
	},
}

// HTTPProbe says a page is up when the server answers with anything
// below 500. A login page (401/403) or a redirect still means the app runs.
// answered is true for a probe error where the page did answer, but with
// a server error: the app is there and broken.
func answered(detail string) bool { return strings.HasPrefix(detail, "answered ") }

func HTTPProbe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nestward-monitor")
	resp, err := probeClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}
