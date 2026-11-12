package alert

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/config"
)

// Heartbeat calls an outside address every minute ("I am alive"). The
// outside service (Nestward's watchdog on a VPS, or healthchecks.io)
// sends the alert when the calls stop, which is the one alert the home
// itself cannot send: when its power or internet is gone.
type Heartbeat struct {
	Settings *config.Settings
	Client   *http.Client
	Log      *slog.Logger

	mu      sync.Mutex
	lastOK  time.Time
	lastErr string
}

// HeartbeatStatus is shown on the status page.
type HeartbeatStatus struct {
	Configured bool      `json:"configured"`
	Target     string    `json:"target,omitempty"`
	LastOK     time.Time `json:"last_ok,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

// Beat sends one heartbeat. It does nothing when no address is set.
func (h *Heartbeat) Beat(ctx context.Context) error {
	url, err := h.Settings.Get(ctx, config.HeartbeatURL)
	if err != nil || url == "" {
		return err
	}
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		req.Header.Set("User-Agent", "nestward-heartbeat")
		var resp *http.Response
		resp, err = c.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("answered %s", resp.Status)
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		// Never log the address itself: it often holds a secret token.
		h.lastErr = scrub(err.Error(), url)
		return err
	}
	h.lastOK, h.lastErr = time.Now(), ""
	return nil
}

// scrub removes a secret address from an error message.
func scrub(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, config.MaskURL(secret))
}

// Run beats every interval until ctx ends.
func (h *Heartbeat) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := h.Beat(ctx); err != nil && ctx.Err() == nil && h.Log != nil {
			h.mu.Lock()
			msg := h.lastErr
			h.mu.Unlock()
			h.Log.Warn("heartbeat failed", "err", msg)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Status reports the heartbeat setup.
func (h *Heartbeat) Status(ctx context.Context) HeartbeatStatus {
	st := HeartbeatStatus{}
	if url, _ := h.Settings.Get(ctx, config.HeartbeatURL); url != "" {
		st.Configured, st.Target = true, config.MaskURL(url)
	}
	h.mu.Lock()
	st.LastOK, st.LastError = h.lastOK, h.lastErr
	h.mu.Unlock()
	return st
}
