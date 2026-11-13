package server

import (
	"context"
	"strconv"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/notify"
)

// alertRestoreTests sends one alert for each new failed restore test in
// an agent's report. What was already sent is remembered in the settings.
func (s *Server) alertRestoreTests(ctx context.Context, machineID, machine string, d *discovery.Result) {
	if s.Alerter == nil || d == nil {
		return
	}
	t := s.Alerter.Lang(ctx)
	for _, svc := range d.Services {
		g := svc.Guest
		if g == nil || g.RestoreTest == nil || g.RestoreTest.Result != discovery.RestoreFailed {
			continue
		}
		rt := g.RestoreTest
		if time.Since(rt.TestedAt) > 7*24*time.Hour {
			continue
		}
		key := "restore.alerted." + machineID + "." + strconv.Itoa(g.VMID)
		stamp := rt.TestedAt.UTC().Format(time.RFC3339)
		if old, ok, err := s.Store.Setting(ctx, key); err != nil || (ok && old == stamp) {
			continue
		}
		name := g.Name
		if name == "" {
			name = svc.Name
		}
		msg := notify.Message{
			Title:    t.Get("alert.restore_failed.title", "name", name),
			Body:     t.Get("alert.restore_failed.body", "name", name, "machine", machine, "why", t.Get("restore.stage."+rt.Stage)),
			Priority: 4,
			Tags:     []string{"floppy_disk", "warning"},
		}
		if err := s.Alerter.Notify(ctx, []notify.Message{msg}); err != nil {
			s.logger().Warn("restore test alert", "err", err)
			continue // try again with the next report
		}
		s.Store.SetSetting(ctx, key, stamp)
	}
}
