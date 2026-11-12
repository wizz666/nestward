// Package alert turns monitor events into short messages for the phone
// and sends them through ntfy. It also pings an outside address every
// minute, so that someone notices when the whole home goes quiet.
package alert

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/notify"
)

// Sender sends one message.
type Sender interface {
	Send(ctx context.Context, m notify.Message) error
}

// Alerter sends alerts with the current settings (read on every send,
// so changes apply without a restart).
type Alerter struct {
	Settings *config.Settings
	I18n     *i18n.Bundle
	Client   *http.Client
	// NewSender overrides the ntfy sender (tests).
	NewSender func(url, topic, token string) Sender
	Now       func() time.Time

	mu       sync.Mutex
	lastSent time.Time
	lastErr  string
	lastTest time.Time
}

// ErrNotConfigured means no ntfy topic is set.
var ErrNotConfigured = errors.New("alerts are not set up")

// ErrTooSoon limits test alerts.
var ErrTooSoon = errors.New("wait a little before sending another test")

func (a *Alerter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Alerter) sender(ctx context.Context) (Sender, error) {
	url, err := a.Settings.Get(ctx, config.NtfyURL)
	if err != nil {
		return nil, err
	}
	topic, err := a.Settings.Get(ctx, config.NtfyTopic)
	if err != nil {
		return nil, err
	}
	if topic == "" {
		return nil, ErrNotConfigured
	}
	token, err := a.Settings.Get(ctx, config.NtfyToken)
	if err != nil {
		return nil, err
	}
	if a.NewSender != nil {
		return a.NewSender(url, topic, token), nil
	}
	return notify.Ntfy{URL: url, Topic: topic, Token: token, Client: a.Client}, nil
}

// Lang returns the translator for alert messages.
func (a *Alerter) Lang(ctx context.Context) i18n.T {
	l, err := a.Settings.Get(ctx, config.AlertsLang)
	if err != nil {
		l = i18n.Default
	}
	return a.I18n.For(l)
}

func (a *Alerter) send(ctx context.Context, msgs []notify.Message) error {
	s, err := a.sender(ctx)
	if err == nil {
		for _, m := range msgs {
			if err = s.Send(ctx, m); err != nil {
				break
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.lastErr = err.Error()
		return err
	}
	a.lastSent, a.lastErr = a.now(), ""
	return nil
}

// Notify sends ready-made messages (used by other checks, such as the
// exposure check). The result shows on the status page like any alert.
func (a *Alerter) Notify(ctx context.Context, msgs []notify.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	return a.send(ctx, msgs)
}

// HandleEvents sends alerts for the events that deserve one. It is
// meant as the monitor's OnEvents.
func (a *Alerter) HandleEvents(ctx context.Context, evs []monitor.Event) {
	msgs := Compose(evs, a.Lang(ctx), a.now())
	if len(msgs) == 0 {
		return
	}
	_ = a.send(ctx, msgs) // the error is kept for the status page
}

// SendTest sends a test alert, at most every 20 seconds.
func (a *Alerter) SendTest(ctx context.Context) error {
	a.mu.Lock()
	if !a.lastTest.IsZero() && a.now().Sub(a.lastTest) < 20*time.Second {
		a.mu.Unlock()
		return ErrTooSoon
	}
	a.lastTest = a.now()
	a.mu.Unlock()
	t := a.Lang(ctx)
	return a.send(ctx, []notify.Message{{Title: t.Get("alert.test.title"), Body: t.Get("alert.test.body"),
		Priority: 3, Tags: []string{"wave"}}})
}

// Status says where alerts go and how the last one went.
type Status struct {
	Configured bool      `json:"configured"`
	Target     string    `json:"target,omitempty"` // server and topic, token never shown
	LastSent   time.Time `json:"last_sent,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

// Status reports the alert setup.
func (a *Alerter) Status(ctx context.Context) Status {
	st := Status{}
	topic, _ := a.Settings.Get(ctx, config.NtfyTopic)
	url, _ := a.Settings.Get(ctx, config.NtfyURL)
	if topic != "" {
		st.Configured = true
		st.Target = config.MaskURL(url) + " · " + maskTopic(topic)
	}
	a.mu.Lock()
	st.LastSent, st.LastError = a.lastSent, a.lastErr
	a.mu.Unlock()
	return st
}

// maskTopic keeps the start of a topic: on public ntfy servers the topic
// works like a password.
func maskTopic(t string) string {
	if len(t) <= 4 {
		return t
	}
	return t[:3] + "…"
}

// Compose writes the messages for a batch of events. Problems on the
// same machine are joined into one message, and when a whole machine is
// silent its services are not listed one by one.
func Compose(evs []monitor.Event, t i18n.T, now time.Time) []notify.Message {
	type key struct {
		machine string
		down    bool
	}
	groups := map[key][]monitor.Event{}
	machineEvent := map[key]monitor.Event{}
	var order []key
	for _, e := range evs {
		if !e.Alert {
			continue
		}
		k := key{e.MachineID, e.To == monitor.Down}
		if !contains(order, k) {
			order = append(order, k)
		}
		if e.Kind == monitor.KindMachine {
			machineEvent[k] = e
		} else {
			groups[k] = append(groups[k], e)
		}
	}
	var out []notify.Message
	for _, k := range order {
		if me, ok := machineEvent[k]; ok {
			out = append(out, machineMessage(me, t, now))
			continue
		}
		list := dedupe(groups[k])
		if len(list) == 0 {
			continue
		}
		out = append(out, serviceMessage(list, k.down, t, now))
	}
	return out
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// dedupe keeps one event per service: a stopped container also makes its
// web page fail, and that is one problem, not two.
func dedupe(evs []monitor.Event) []monitor.Event {
	best := map[string]monitor.Event{}
	var names []string
	for _, e := range evs {
		cur, ok := best[e.Service]
		if !ok {
			names = append(names, e.Service)
		}
		if !ok || (e.Kind == monitor.KindContainer && cur.Kind != monitor.KindContainer) {
			best[e.Service] = e
		}
	}
	sort.Strings(names)
	out := make([]monitor.Event, 0, len(names))
	for _, n := range names {
		out = append(out, best[n])
	}
	return out
}

func name(e monitor.Event) string {
	if e.App != "" {
		return e.App
	}
	return e.Service
}

func machineMessage(e monitor.Event, t i18n.T, now time.Time) notify.Message {
	if e.To == monitor.Down {
		return notify.Message{
			Title:    t.Get("alert.machine.down.title", "machine", e.Machine),
			Body:     t.Get("alert.machine.down.body", "machine", e.Machine, "time", Clock(e.Since, now, t)),
			Priority: 4, Tags: []string{"warning"},
		}
	}
	return notify.Message{
		Title:    t.Get("alert.machine.up.title", "machine", e.Machine),
		Body:     t.Get("alert.up.body", "duration", Duration(e.At.Sub(e.Since), t)),
		Priority: 3, Tags: []string{"white_check_mark"},
	}
}

func reason(e monitor.Event, t i18n.T, now time.Time) string {
	at := Clock(e.Since, now, t)
	switch {
	case e.Kind == monitor.KindWeb:
		return t.Get("alert.down.body.web", "time", at)
	case e.Detail == "running (unhealthy)":
		return t.Get("alert.down.body.unhealthy", "time", at)
	default:
		return t.Get("alert.down.body.container", "time", at)
	}
}

func serviceMessage(list []monitor.Event, down bool, t i18n.T, now time.Time) notify.Message {
	machine := list[0].Machine
	if down {
		m := notify.Message{Priority: 4, Tags: []string{"warning"}}
		if len(list) == 1 {
			m.Title = t.Get("alert.down.title", "app", name(list[0]), "machine", machine)
			m.Body = reason(list[0], t, now)
			return m
		}
		m.Title = t.Get("alert.down.many.title", "count", strconv.Itoa(len(list)), "machine", machine)
		for _, e := range list {
			m.Body += "• " + name(e) + ": " + reason(e, t, now) + "\n"
		}
		return m
	}
	m := notify.Message{Priority: 3, Tags: []string{"white_check_mark"}}
	if len(list) == 1 {
		m.Title = t.Get("alert.up.title", "app", name(list[0]), "machine", machine)
		m.Body = t.Get("alert.up.body", "duration", Duration(list[0].At.Sub(list[0].Since), t))
		return m
	}
	m.Title = t.Get("alert.up.many.title", "count", strconv.Itoa(len(list)), "machine", machine)
	for _, e := range list {
		m.Body += "• " + name(e) + ": " + t.Get("alert.up.body", "duration", Duration(e.At.Sub(e.Since), t)) + "\n"
	}
	return m
}

// Clock formats a time as "14:05", or with the date when it is not today.
func Clock(at, now time.Time, t i18n.T) string {
	at, now = at.Local(), now.Local()
	if at.Year() == now.Year() && at.YearDay() == now.YearDay() {
		return at.Format("15:04")
	}
	return at.Format("2006-01-02 15:04")
}

// Duration formats a length of time in plain words.
func Duration(d time.Duration, t i18n.T) string {
	switch {
	case d < time.Minute:
		return t.Get("duration.under_minute")
	case d < time.Hour:
		return t.Get("duration.minutes", "n", strconv.Itoa(int(d.Minutes())))
	default:
		h := int(d.Hours())
		return t.Get("duration.hours", "h", strconv.Itoa(h), "m", strconv.Itoa(int(d.Minutes())-h*60))
	}
}
