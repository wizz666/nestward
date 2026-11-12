package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/health"
	"github.com/wizz666/nestward/internal/monitor"
)

// MonitorMachines is the monitor's view of every machine: this one plus
// the agents.
func (s *Server) MonitorMachines(ctx context.Context) ([]monitor.Machine, error) {
	lp := s.Local(ctx)
	out := []monitor.Machine{{ID: lp.Hardware.MachineID, Name: lp.Hardware.Hostname, Local: true,
		LastSeen: lp.Hardware.CollectedAt, Discovery: lp.Discovery}}
	if s.Store == nil {
		return out, nil
	}
	all, err := s.Store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.ID == lp.Hardware.MachineID || m.Source != "agent" {
			continue
		}
		out = append(out, monitor.Machine{ID: m.ID, Name: m.Hostname, Address: m.Address,
			LastSeen: m.LastSeen, Discovery: m.Discovery})
	}
	return out, nil
}

// StatusView is the monitoring overview.
type StatusView struct {
	Summary struct {
		Total   int `json:"total"`
		Up      int `json:"up"`
		Down    int `json:"down"`
		Stopped int `json:"stopped"`
	} `json:"summary"`
	Checks    []monitor.Check        `json:"checks"`
	Alerts    *alert.Status          `json:"alerts,omitempty"`
	Heartbeat *alert.HeartbeatStatus `json:"heartbeat,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v := StatusView{Checks: []monitor.Check{}}
	if s.Monitor != nil {
		cs, err := s.Monitor.Checks(r.Context())
		if err != nil {
			s.logger().Error("checks", "err", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
		v.Checks = cs
		for _, c := range cs {
			v.Summary.Total++
			switch c.Status {
			case monitor.Up:
				v.Summary.Up++
			case monitor.Down:
				v.Summary.Down++
			case monitor.Stopped:
				v.Summary.Stopped++
			}
		}
	}
	if s.Alerter != nil {
		st := s.Alerter.Status(r.Context())
		v.Alerts = &st
	}
	if s.Heartbeat != nil {
		st := s.Heartbeat.Status(r.Context())
		v.Heartbeat = &st
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	raws, err := s.Store.Events(r.Context(), limit)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	out := make([]json.RawMessage, 0, len(raws))
	out = append(out, raws...)
	writeJSON(w, http.StatusOK, out)
}

// sameOrigin blocks requests sent by other web sites (CSRF). The page's
// own script sends a custom header, which a foreign form cannot do, and
// the Origin, when present, must be this server.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("X-Nestward") != "1" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// handleCheckNow runs every check at once: the "Check again" button.
func (s *Server) handleCheckNow(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.Monitor != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if _, err := s.Monitor.RunOnce(ctx); err != nil {
			s.logger().Error("check now", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "checked", "at": time.Now().UTC()})
}

func (s *Server) handleTestAlert(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.Alerter == nil {
		http.Error(w, "alerts are off", http.StatusNotFound)
		return
	}
	err := s.Alerter.SendTest(r.Context())
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	case errors.Is(err, alert.ErrTooSoon):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
	case errors.Is(err, alert.ErrNotConfigured):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in := health.Input{}
	ms, err := s.MonitorMachines(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	local := s.Local(ctx).Hardware
	for _, m := range ms {
		hw := local
		if !m.Local && s.Store != nil {
			all, _ := s.Store.Machines(ctx)
			for _, sm := range all {
				if sm.ID == m.ID {
					hw = sm.Hardware
				}
			}
		}
		in.Machines = append(in.Machines, health.Machine{ID: m.ID, Name: m.Name, Hardware: hw, Discovery: m.Discovery})
	}
	if s.Monitor != nil {
		in.Checks, _ = s.Monitor.Checks(ctx)
	}
	if s.Alerter != nil {
		in.Alerts = s.Alerter.Status(ctx)
	}
	if s.Heartbeat != nil {
		in.Heartbeat = s.Heartbeat.Status(ctx)
	}
	if s.Publish != nil {
		in.Publications, _ = s.Publish.List(ctx)
	}
	if s.Exposure != nil {
		if r, err := s.Exposure.Report(ctx); err == nil {
			in.Exposure = r.Findings
		}
	}
	writeJSON(w, http.StatusOK, health.Compute(in, s.lang(r)))
}
