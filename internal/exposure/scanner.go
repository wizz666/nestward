package exposure

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/cloudflare"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/notify"
)

// Finding is one public address and what a stranger meets there.
type Finding struct {
	Host      string    `json:"host"`
	Source    string    `json:"source"`           // "tunnel:<name>" or "manual"
	Target    string    `json:"target,omitempty"` // where the tunnel sends it
	Probe               // verdict, app, status, detail
	Severity  Severity  `json:"severity"`
	Accepted  bool      `json:"accepted"` // the owner says this is fine as it is
	FirstSeen time.Time `json:"first_seen"`
	CheckedAt time.Time `json:"checked_at"`
	// Alerted is the verdict last sent to the phone, so the same
	// problem is not sent again on every run.
	Alerted Verdict `json:"alerted,omitempty"`
}

// Report is the last run.
type Report struct {
	CheckedAt time.Time `json:"checked_at"`
	Running   bool      `json:"running"`
	Sources   []string  `json:"sources"`         // where addresses came from
	Error     string    `json:"error,omitempty"` // e.g. Cloudflare could not be read
	Findings  []Finding `json:"findings"`
	Counts    struct {
		Critical int `json:"critical"`
		Warn     int `json:"warn"`
		Info     int `json:"info"`
		OK       int `json:"ok"`
	} `json:"counts"`
}

// Store keeps findings between runs.
type Store interface {
	SaveExposure(ctx context.Context, host string, v any) error
	DeleteExposure(ctx context.Context, host string) error
	Exposures(ctx context.Context) ([]json.RawMessage, error)
}

// Notifier sends messages to the phone.
type Notifier interface {
	Notify(ctx context.Context, msgs []notify.Message) error
	Lang(ctx context.Context) i18n.T
}

// Scanner runs the check now and then.
type Scanner struct {
	Settings *config.Settings
	Store    Store
	Prober   *Prober
	Notifier Notifier // nil: no alerts
	Log      *slog.Logger
	// CloudflareBase points at a fake API in tests.
	CloudflareBase string
	Now            func() time.Time

	mu      sync.Mutex
	running bool
	last    Report
	haveRun bool
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scanner) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Run checks after a short wait and then every interval.
func (s *Scanner) Run(ctx context.Context, first, every time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("exposure check", "err", err)
		}
		t.Reset(every)
	}
}

// ErrBusy means a check is already running.
var ErrBusy = fmt.Errorf("exposure check already running")

type target struct {
	host, source, service string
}

// hosts lists every public address to check.
func (s *Scanner) hosts(ctx context.Context) ([]target, []string, error) {
	var out []target
	var sources []string
	var cfErr error
	token, _ := s.Settings.Get(ctx, config.CloudflareToken)
	if token != "" {
		ts, err := s.tunnelHosts(ctx, token)
		if err != nil {
			cfErr = err
		} else {
			out = append(out, ts...)
			sources = append(sources, "cloudflare")
		}
	}
	extra, _ := s.Settings.Get(ctx, config.ExposureHosts)
	for _, h := range splitHosts(extra) {
		out = append(out, target{host: h, source: "manual"})
	}
	if extra != "" {
		sources = append(sources, "manual")
	}
	// One entry per address; a tunnel route wins over a manual entry.
	seen := map[string]bool{}
	uniq := out[:0]
	for _, t := range out {
		if seen[t.host] {
			continue
		}
		seen[t.host] = true
		uniq = append(uniq, t)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].host < uniq[j].host })
	return uniq, sources, cfErr
}

func splitHosts(v string) []string {
	var out []string
	for _, h := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
		h = strings.TrimSuffix(h, "/")
		if h != "" && !strings.ContainsAny(h, "/ ") {
			out = append(out, h)
		}
	}
	return out
}

func (s *Scanner) tunnelHosts(ctx context.Context, token string) ([]target, error) {
	cf := &cloudflare.Client{Token: token, Base: s.CloudflareBase}
	account, _ := s.Settings.Get(ctx, config.CloudflareAccount)
	if account == "" {
		a, err := cf.AccountFromZones(ctx)
		if err != nil {
			return nil, err
		}
		account = a
	}
	tunnels, err := cf.Tunnels(ctx, account)
	if err != nil {
		return nil, err
	}
	var out []target
	for _, t := range tunnels {
		if !t.Remote() {
			continue // routes kept in a file on the machine are not visible
		}
		cfg, err := cf.TunnelConfiguration(ctx, account, t.ID)
		if err != nil {
			return nil, fmt.Errorf("tunnel %s: %w", t.Name, err)
		}
		for _, in := range cfg.Ingress {
			h, _ := in["hostname"].(string)
			svc, _ := in["service"].(string)
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || strings.HasPrefix(h, "*") {
				continue
			}
			if p, _ := in["path"].(string); p != "" {
				continue // only part of a site; the main address is checked
			}
			out = append(out, target{host: h, source: "tunnel:" + t.Name, service: svc})
		}
	}
	return out, nil
}

func (s *Scanner) previous(ctx context.Context) (map[string]Finding, error) {
	raws, err := s.Store.Exposures(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Finding{}
	for _, r := range raws {
		var f Finding
		if json.Unmarshal(r, &f) == nil && f.Host != "" {
			out[f.Host] = f
		}
	}
	return out, nil
}

// RunOnce checks every address now.
func (s *Scanner) RunOnce(ctx context.Context) (Report, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return Report{}, ErrBusy
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	targets, sources, cfErr := s.hosts(ctx)
	if cfErr != nil && len(targets) == 0 {
		s.setLast(Report{CheckedAt: s.now(), Error: cfErr.Error(), Findings: []Finding{}})
		return s.Report(ctx)
	}
	prev, err := s.previous(ctx)
	if err != nil {
		return Report{}, err
	}
	now := s.now()
	results := make([]Finding, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			pr := s.Prober.Check(cctx, t.host)
			f := Finding{Host: t.host, Source: t.source, Target: t.service, Probe: pr,
				Severity: SeverityOf(pr.Verdict), FirstSeen: now, CheckedAt: now}
			if p, ok := prev[t.host]; ok {
				f.FirstSeen, f.Accepted, f.Alerted = p.FirstSeen, p.Accepted, p.Alerted
			}
			results[i] = f
		}(i, t)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}

	// Alerts: new or worse problems that the owner has not accepted.
	var alertList []Finding
	for i := range results {
		f := &results[i]
		bad := (f.Severity == Critical || f.Severity == Warn) && !f.Accepted
		if bad && f.Alerted != f.Verdict {
			alertList = append(alertList, *f)
			f.Alerted = f.Verdict
		}
		if !bad {
			f.Alerted = ""
		}
	}
	if len(alertList) > 0 && s.Notifier != nil {
		msgs := Compose(alertList, s.Notifier.Lang(ctx))
		if err := s.Notifier.Notify(ctx, msgs); err != nil {
			s.log().Warn("exposure alert", "err", err)
			// Try again next run.
			for i := range results {
				for _, a := range alertList {
					if results[i].Host == a.Host {
						results[i].Alerted = prev[a.Host].Alerted
					}
				}
			}
		}
	}

	keep := map[string]bool{}
	for _, f := range results {
		keep[f.Host] = true
		if err := s.Store.SaveExposure(ctx, f.Host, f); err != nil {
			return Report{}, err
		}
	}
	// Addresses that are gone from the list are forgotten.
	for h := range prev {
		if !keep[h] {
			_ = s.Store.DeleteExposure(ctx, h)
		}
	}
	r := Report{CheckedAt: now, Sources: sources}
	if cfErr != nil {
		r.Error = cfErr.Error()
	}
	s.setLast(r)
	s.log().Info("exposure check", "addresses", len(results), "alerts", len(alertList))
	return s.Report(ctx)
}

func (s *Scanner) setLast(r Report) {
	s.mu.Lock()
	s.last, s.haveRun = r, true
	s.mu.Unlock()
}

// Report returns the stored findings with the last run's details.
func (s *Scanner) Report(ctx context.Context) (Report, error) {
	s.mu.Lock()
	r := s.last
	r.Running = s.running
	s.mu.Unlock()
	prev, err := s.previous(ctx)
	if err != nil {
		return Report{}, err
	}
	r.Findings = make([]Finding, 0, len(prev))
	for _, f := range prev {
		if r.CheckedAt.IsZero() || f.CheckedAt.After(r.CheckedAt) {
			r.CheckedAt = f.CheckedAt
		}
		r.Findings = append(r.Findings, f)
	}
	sort.Slice(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if rank(a) != rank(b) {
			return rank(a) > rank(b)
		}
		return a.Host < b.Host
	})
	for _, f := range r.Findings {
		switch {
		case f.Accepted || f.Severity == OK:
			r.Counts.OK++
		case f.Severity == Critical:
			r.Counts.Critical++
		case f.Severity == Warn:
			r.Counts.Warn++
		default:
			r.Counts.Info++
		}
	}
	if r.Sources == nil {
		r.Sources = []string{}
	}
	return r, nil
}

func rank(f Finding) int {
	if f.Accepted {
		return 0
	}
	switch f.Severity {
	case Critical:
		return 4
	case Warn:
		return 3
	case Info:
		return 2
	}
	return 1
}

// ErrUnknownHost means the address is not in the list.
var ErrUnknownHost = fmt.Errorf("unknown address")

// Accept marks an address as fine as it is (or undoes that).
func (s *Scanner) Accept(ctx context.Context, host string, accepted bool) (Finding, error) {
	prev, err := s.previous(ctx)
	if err != nil {
		return Finding{}, err
	}
	f, ok := prev[strings.ToLower(host)]
	if !ok {
		return Finding{}, ErrUnknownHost
	}
	f.Accepted = accepted
	if accepted {
		f.Alerted = f.Verdict // no alert for what the owner accepted
	}
	return f, s.Store.SaveExposure(ctx, f.Host, f)
}

// Compose writes the phone messages: one per address, at most five, then
// one line for the rest.
func Compose(list []Finding, t i18n.T) []notify.Message {
	sort.SliceStable(list, func(i, j int) bool { return rank(list[i]) > rank(list[j]) })
	var msgs []notify.Message
	for i, f := range list {
		if i == 5 {
			rest := make([]string, 0, len(list)-5)
			for _, g := range list[5:] {
				rest = append(rest, g.Host)
			}
			msgs = append(msgs, notify.Message{Title: t.Get("exp.alert.more_title", "count", fmt.Sprint(len(rest))),
				Body: strings.Join(rest, ", "), Priority: 3, Tags: []string{"warning"}})
			break
		}
		app := f.App
		if app == "" {
			app = f.Host
		}
		m := notify.Message{
			Title: t.Get("exp.alert."+string(f.Verdict)+".title", "host", f.Host, "app", app),
			Body:  t.Get("exp.alert."+string(f.Verdict)+".body", "host", f.Host, "app", app),
			Click: "https://" + f.Host,
		}
		if f.Severity == Critical {
			m.Priority, m.Tags = 5, []string{"rotating_light"}
		} else {
			m.Priority, m.Tags = 3, []string{"warning"}
		}
		msgs = append(msgs, m)
	}
	return msgs
}
