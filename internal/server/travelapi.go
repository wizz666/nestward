package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/health"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/notify"
	"github.com/wizz666/nestward/internal/travel"
)

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (s *Server) setting(ctx context.Context, key string) string {
	if s.Store == nil {
		return ""
	}
	v, _, _ := s.Store.Setting(ctx, key)
	return v
}

func (s *Server) travelHour(ctx context.Context) int {
	if h, err := strconv.Atoi(s.setting(ctx, travel.KeyHour)); err == nil && h >= 0 && h <= 23 {
		return h
	}
	return 8
}

// travelReport builds the travel check.
func (s *Server) travelReport(ctx context.Context, t i18n.T) (travel.Report, health.Input, health.Report, error) {
	in, err := s.healthInput(ctx)
	if err != nil {
		return travel.Report{}, in, health.Report{}, err
	}
	h := health.Compute(in, t)
	bios := map[string]string{}
	if s.Store != nil {
		all, _ := s.Store.Settings(ctx)
		for k, v := range all {
			if id, ok := strings.CutPrefix(k, travel.KeyBIOS); ok {
				bios[id] = v
			}
		}
	}
	rep := travel.Build(h, in.Machines, bios, t)
	rep.On = s.setting(ctx, travel.KeyOn) == "1"
	rep.Until = s.setting(ctx, travel.KeyUntil)
	rep.Hour = s.travelHour(ctx)
	return rep, in, h, nil
}

func (s *Server) registerTravel(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/travel", func(w http.ResponseWriter, r *http.Request) {
		rep, _, _, err := s.travelReport(r.Context(), s.lang(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	// Turn travel mode on or off.
	mux.HandleFunc("POST /api/v1/travel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On    bool   `json:"on"`
			Until string `json:"until"`
			Hour  *int   `json:"hour"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil ||
			(req.Until != "" && !dateRE.MatchString(req.Until)) || (req.Hour != nil && (*req.Hour < 0 || *req.Hour > 23)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
			return
		}
		ctx := r.Context()
		on := "0"
		if req.On {
			on = "1"
		}
		s.Store.SetSetting(ctx, travel.KeyOn, on)
		s.Store.SetSetting(ctx, travel.KeyUntil, req.Until)
		if req.Hour != nil {
			s.Store.SetSetting(ctx, travel.KeyHour, strconv.Itoa(*req.Hour))
		}
		if req.On {
			// The first daily message comes at the next hour, not right away.
			s.Store.SetSetting(ctx, travel.KeySent, time.Now().Format("2006-01-02"))
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	})
	// Confirm (or undo) that a machine switches itself on after a power cut.
	mux.HandleFunc("POST /api/v1/travel/bios", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Machine string `json:"machine"`
			Done    bool   `json:"done"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Machine == "" || len(req.Machine) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
			return
		}
		ctx := r.Context()
		key := travel.KeyBIOS + req.Machine
		if req.Done {
			s.Store.SetSetting(ctx, key, time.Now().Format("2006-01-02"))
		} else {
			s.Store.DeleteSetting(ctx, key)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	})
	// Send today's message now, to see what it looks like.
	mux.HandleFunc("POST /api/v1/travel/test", func(w http.ResponseWriter, r *http.Request) {
		if err := s.sendTravelDaily(r.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "send_failed", "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	})
}

func (s *Server) sendTravelDaily(ctx context.Context) error {
	if s.Alerter == nil {
		return nil
	}
	t := s.Alerter.Lang(ctx)
	_, in, h, err := s.travelReport(ctx, t)
	if err != nil {
		return err
	}
	up, down := 0, 0
	for _, c := range in.Checks {
		switch c.Status {
		case monitor.Up:
			up++
		case monitor.Down:
			down++
		}
	}
	title, body, prio := travel.Daily(h, len(in.Machines), up, down, t)
	return s.Alerter.Notify(ctx, []notify.Message{{Title: title, Body: body, Priority: prio, Tags: []string{"house"}}})
}

// RunTravel sends the daily message while travel mode is on, and turns
// travel mode off the day after the trip ends.
func (s *Server) RunTravel(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if s.Store == nil {
			continue
		}
		now := time.Now()
		on := s.setting(ctx, travel.KeyOn) == "1"
		until := s.setting(ctx, travel.KeyUntil)
		if on && travel.Expired(until, now) {
			s.Store.SetSetting(ctx, travel.KeyOn, "0")
			continue
		}
		if !travel.Due(on, until, s.travelHour(ctx), s.setting(ctx, travel.KeySent), now) {
			continue
		}
		if err := s.sendTravelDaily(ctx); err != nil {
			s.logger().Warn("travel daily message", "err", err)
			continue // try again next minute
		}
		s.Store.SetSetting(ctx, travel.KeySent, now.Format("2006-01-02"))
	}
}
