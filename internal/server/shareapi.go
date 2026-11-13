package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/health"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/version"
)

// ShareSummary is the little read-only view another program (WizzOS) may
// show: the health score, the top things to fix and how many checks are up.
// It never contains addresses, names of secrets or anything to log in with.
type ShareSummary struct {
	Version   string      `json:"version"`
	Score     int         `json:"score"`
	Grade     string      `json:"grade"`
	GradeText string      `json:"grade_text"`
	Items     []ShareItem `json:"items"`
	Good      []string    `json:"good"`
	Machines  int         `json:"machines"`
	Up        int         `json:"up"`
	Down      int         `json:"down"`
	Restored  int         `json:"restored"`
}

// ShareItem is one thing that costs points.
type ShareItem struct {
	Points int    `json:"points"`
	Text   string `json:"text"`
	Fix    string `json:"fix"`
}

func (s *Server) handleShareSummary(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "off"})
		return
	}
	want, err := s.Settings.Get(r.Context(), config.ShareToken)
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil || want == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "off"})
		return
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong_key"})
		return
	}
	in, err := s.healthInput(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	rep := health.Compute(in, s.lang(r))
	out := ShareSummary{Version: version.Version, Score: rep.Score, Grade: rep.Grade, GradeText: rep.GradeText,
		Good: rep.Good, Machines: len(in.Machines), Restored: rep.Restored, Items: []ShareItem{}}
	for i, it := range rep.Items {
		if i == 5 {
			break
		}
		out.Items = append(out.Items, ShareItem{Points: it.Points, Text: it.Text, Fix: it.Fix})
	}
	if out.Good == nil {
		out.Good = []string{}
	}
	if s.Monitor != nil {
		if cs, err := s.Monitor.Checks(r.Context()); err == nil {
			for _, c := range cs {
				switch c.Status {
				case monitor.Up:
					out.Up++
				case monitor.Down:
					out.Down++
				}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
