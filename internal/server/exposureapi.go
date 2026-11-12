package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wizz666/nestward/internal/exposure"
)

func (s *Server) registerExposure(mux *http.ServeMux) {
	if s.Exposure == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/exposure", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.Exposure.Report(r.Context())
		if err != nil {
			s.logger().Error("exposure report", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	// "Check now" runs in the background; the page asks again for the result.
	mux.HandleFunc("POST /api/v1/exposure/check", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := s.Exposure.RunOnce(ctx); err != nil && !errors.Is(err, exposure.ErrBusy) {
				s.logger().Warn("exposure check", "err", err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
	})
	mux.HandleFunc("POST /api/v1/exposure/accept", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Host     string `json:"host"`
			Accepted bool   `json:"accepted"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		f, err := s.Exposure.Accept(r.Context(), in.Host, in.Accepted)
		if errors.Is(err, exposure.ErrUnknownHost) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "exposure_unknown"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		s.logger().Info("exposure accepted", "host", f.Host, "accepted", f.Accepted)
		writeJSON(w, http.StatusOK, f)
	})
}
