package watchdog

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/notify"
)

type sink struct{ msgs []notify.Message }

func (s *sink) Send(_ context.Context, m notify.Message) error {
	s.msgs = append(s.msgs, m)
	return nil
}

func TestWatchdog(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	s := &sink{}
	w := &Watchdog{Name: "Hemma", Token: "abc123", Timeout: 3 * time.Minute, Sender: s,
		T: i18n.MustLoad().For("sv"), Now: func() time.Time { return now }}
	ctx := context.Background()

	now = now.Add(time.Hour)
	w.Check(ctx)
	if len(s.msgs) != 0 {
		t.Fatal("no alert before the first ping")
	}

	h := w.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ping/wrong", nil))
	if rec.Code != 404 {
		t.Errorf("wrong token: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ping/abc123", nil))
	if rec.Code != 200 {
		t.Fatalf("ping: %d", rec.Code)
	}
	lastPing := now

	now = now.Add(2 * time.Minute)
	w.Check(ctx)
	if len(s.msgs) != 0 {
		t.Fatal("alert too early")
	}
	now = now.Add(2 * time.Minute)
	w.Check(ctx)
	w.Check(ctx) // only once
	if len(s.msgs) != 1 || s.msgs[0].Title != "Hemma svarar inte" || !strings.Contains(s.msgs[0].Body, lastPing.Format("15:04")) {
		t.Fatalf("down alert = %+v", s.msgs)
	}
	now = now.Add(10 * time.Minute)
	w.Ping(ctx)
	if len(s.msgs) != 2 || s.msgs[1].Title != "Hemma är tillbaka på nätet" || s.msgs[1].Body != "Det var tyst i 14 min." {
		t.Errorf("back alert = %+v", s.msgs)
	}
}
