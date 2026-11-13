// Package server is the Nestward core: the web interface and its API.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/agent"
	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/analysis"
	"github.com/wizz666/nestward/internal/auth"
	"github.com/wizz666/nestward/internal/catalog"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/exposure"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/publish"
	"github.com/wizz666/nestward/internal/store"
	"github.com/wizz666/nestward/internal/version"
)

//go:embed web
var webFS embed.FS

// Server serves the web UI and API.
type Server struct {
	Store    *store.Store
	Profiles []catalog.Profile
	I18n     *i18n.Bundle
	Detect   func(ctx context.Context) hardware.Report
	// Discover lists this machine's services; nil turns discovery off.
	Discover func(ctx context.Context) discovery.Result
	// Agents runs the encrypted agent port; nil turns agents off.
	Agents *AgentGate
	Log    *slog.Logger
	// Monitoring; any of these may be nil.
	Monitor   *monitor.Monitor
	Alerter   *alert.Alerter
	Heartbeat *alert.Heartbeat
	// Auth protects the interface. Nil leaves it open (tests only).
	Auth     *auth.Manager
	Settings *config.Settings
	Publish  *publish.Engine
	// Exposure checks what the home shows to the internet; nil turns it off.
	Exposure *exposure.Scanner

	mu        sync.Mutex
	localAt   time.Time
	local     agent.Payload
	localHave bool
}

// cacheFor is how long a local hardware reading is reused.
const cacheFor = 30 * time.Second

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version.Version})
	})
	mux.HandleFunc("GET /api/v1/i18n", s.handleI18n)
	mux.HandleFunc("GET /api/v1/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/machines", s.handleMachines)
	mux.HandleFunc("GET /api/v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Profiles)
	})
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/alerts/test", s.handleTestAlert)
	mux.HandleFunc("POST /api/v1/check", s.handleCheckNow)
	s.registerAuth(mux)
	s.registerAgents(mux)
	s.registerPublish(mux)
	s.registerExposure(mux)
	s.registerTravel(mux)
	mux.HandleFunc("GET /api/v1/share/summary", s.handleShareSummary)
	return securityHeaders(s.guard(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) lang(r *http.Request) i18n.T {
	if l := r.URL.Query().Get("lang"); l != "" && s.I18n.Has(l) {
		return s.I18n.For(l)
	}
	return s.I18n.For(s.I18n.Match(r.Header.Get("Accept-Language")))
}

func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	t := s.lang(r)
	out := map[string]any{"lang": t.Lang, "languages": s.I18n.Languages(), "strings": map[string]string{}}
	strs := out["strings"].(map[string]string)
	for k, v := range s.I18n.Keys(i18n.Default) {
		strs[k] = v
	}
	for k, v := range s.I18n.Keys(t.Lang) {
		strs[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// Local reads this machine's hardware and services (cached briefly) and
// saves them.
func (s *Server) Local(ctx context.Context) agent.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localHave && time.Since(s.localAt) < cacheFor {
		return s.local
	}
	p := agent.Payload{Hardware: s.Detect(ctx)}
	if s.Discover != nil {
		d := s.Discover(ctx)
		p.Discovery = &d
	}
	s.local, s.localAt, s.localHave = p, time.Now(), true
	if s.Store != nil {
		if err := s.Store.SaveMachine(ctx, "local", "", p.Hardware, p.Discovery); err != nil {
			s.logger().Warn("could not save local report", "err", err)
		}
	}
	return p
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	t := s.lang(r)
	writeJSON(w, http.StatusOK, analysis.Analyze(s.Local(r.Context()).Hardware, s.Profiles, t))
}

// MachineView is one machine in the machines list.
type MachineView struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	Address   string            `json:"address,omitempty"` // empty: the address the browser used
	LastSeen  time.Time         `json:"last_seen"`
	Report    analysis.Report   `json:"report"`
	Discovery *discovery.Result `json:"discovery,omitempty"`
}

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	t := s.lang(r)
	lp := s.Local(r.Context())
	local := lp.Hardware
	machines := []store.Machine{{ID: local.MachineID, Hostname: local.Hostname, Source: "local", LastSeen: local.CollectedAt, Hardware: local, Discovery: lp.Discovery}}
	if s.Store != nil {
		all, err := s.Store.Machines(r.Context())
		if err != nil {
			s.logger().Error("list machines", "err", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		for _, m := range all {
			if m.ID != local.MachineID {
				machines = append(machines, m)
			}
		}
	}
	reports := make([]analysis.Report, len(machines))
	for i, m := range machines {
		reports[i] = analysis.Analyze(m.Hardware, s.Profiles, t)
	}
	analysis.PlaceElsewhere(reports, t)
	out := make([]MachineView, len(machines))
	for i, m := range machines {
		out[i] = MachineView{ID: m.ID, Source: m.Source, Address: m.Address, LastSeen: m.LastSeen, Report: reports[i], Discovery: m.Discovery}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("write json", "err", err)
	}
}
