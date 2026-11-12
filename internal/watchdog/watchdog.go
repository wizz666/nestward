// Package watchdog runs outside the home (on a VPS) and raises the alarm
// when the home's Nestward stops checking in: the alert the home cannot
// send itself when its power or internet is gone.
package watchdog

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/notify"
)

// Watchdog waits for pings and alerts when they stop.
type Watchdog struct {
	Name    string        // what is being watched, e.g. "Hemma"
	Token   string        // secret part of the ping address
	Timeout time.Duration // silence before the alert
	Sender  alert.Sender
	T       i18n.T
	Now     func() time.Time
	Log     *slog.Logger

	mu       sync.Mutex
	lastPing time.Time
	alerted  bool
	downAt   time.Time
}

func (w *Watchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watchdog) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Handler serves GET or POST /ping/<token> and /healthz.
func (w *Watchdog) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping/", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		tok := strings.TrimPrefix(r.URL.Path, "/ping/")
		if w.Token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(w.Token)) != 1 {
			http.NotFound(rw, r)
			return
		}
		w.Ping(r.Context())
		rw.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("ok\n"))
	})
	return mux
}

// Ping records a sign of life, and says so if the home was silent.
func (w *Watchdog) Ping(ctx context.Context) {
	w.mu.Lock()
	now := w.now()
	w.lastPing = now
	wasDown, since := w.alerted, w.downAt
	w.alerted = false
	w.mu.Unlock()
	if wasDown {
		w.send(ctx, notify.Message{
			Title:    w.T.Get("alert.home.up.title", "name", w.Name),
			Body:     w.T.Get("alert.home.up.body", "duration", alert.Duration(now.Sub(since), w.T)),
			Priority: 3, Tags: []string{"white_check_mark"},
		})
	}
}

// Check alerts once when the last ping is older than Timeout. Nothing
// happens before the first ping, so a new watchdog does not cry wolf.
func (w *Watchdog) Check(ctx context.Context) {
	w.mu.Lock()
	now := w.now()
	if w.lastPing.IsZero() || w.alerted || now.Sub(w.lastPing) <= w.Timeout {
		w.mu.Unlock()
		return
	}
	w.alerted, w.downAt = true, w.lastPing
	last := w.lastPing
	w.mu.Unlock()
	w.send(ctx, notify.Message{
		Title:    w.T.Get("alert.home.down.title", "name", w.Name),
		Body:     w.T.Get("alert.home.down.body", "name", w.Name, "time", alert.Clock(last, now, w.T)),
		Priority: 5, Tags: []string{"rotating_light"},
	})
}

func (w *Watchdog) send(ctx context.Context, m notify.Message) {
	if err := w.Sender.Send(ctx, m); err != nil {
		w.log().Error("could not send alert", "err", err)
		return
	}
	w.log().Info("alert sent", "title", m.Title)
}

// Run checks every 15 seconds until ctx ends.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}
