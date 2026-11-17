package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/hardware"
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

// shareOK checks the read-only share key. It writes the error itself.
func (s *Server) shareOK(w http.ResponseWriter, r *http.Request) bool {
	if s.Settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "off"})
		return false
	}
	want, err := s.Settings.Get(r.Context(), config.ShareToken)
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil || want == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "off"})
		return false
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong_key"})
		return false
	}
	return true
}

// ShareMachine is one machine on the home map: what it is, how full it is
// and whether it answers. Never addresses, secrets or service details.
type ShareMachine struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`            // unraid, proxmox, linux …
	OS        string    `json:"os,omitempty"`    // e.g. "Debian 12"
	Model     string    `json:"model,omitempty"` // board/product when known
	VM        bool      `json:"vm"`              // runs inside a virtual machine
	Local     bool      `json:"local"`           // the machine Nestward itself runs on
	Arch      string    `json:"arch"`
	Cores     int       `json:"cores"`
	MemTotal  uint64    `json:"mem_total"`
	MemFree   uint64    `json:"mem_free"`
	DiskTotal uint64    `json:"disk_total"` // biggest filesystem
	DiskFree  uint64    `json:"disk_free"`
	LastSeen  time.Time `json:"last_seen"`
	Online    bool      `json:"online"`
	Services  int       `json:"services"`
	Up        int       `json:"up"`
	Down      int       `json:"down"`
	WizzOS    bool      `json:"wizzos"`
	// Storage is a Proxmox host's room for guest disks (e.g. local-lvm).
	Storage []ShareStorage `json:"storage"`
}

// ShareStorage is one place for virtual disks, with its free room.
type ShareStorage struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// handleShareMachines is the read-only machine list for WizzOS's home map.
func (s *Server) handleShareMachines(w http.ResponseWriter, r *http.Request) {
	if !s.shareOK(w, r) {
		return
	}
	ctx := r.Context()
	ms, err := s.MonitorMachines(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	hw := map[string]hardware.Report{}
	local := s.Local(ctx).Hardware
	if s.Store != nil {
		if all, err := s.Store.Machines(ctx); err == nil {
			for _, m := range all {
				hw[m.ID] = m.Hardware
			}
		}
	}
	var checks []monitor.Check
	if s.Monitor != nil {
		checks, _ = s.Monitor.Checks(ctx)
	}
	out := []ShareMachine{}
	for _, m := range ms {
		h, ok := hw[m.ID]
		if m.Local || !ok {
			h = local
			if !m.Local && !ok {
				h = hardware.Report{}
			}
		}
		sm := ShareMachine{Name: m.Name, Kind: h.OS.Kind, OS: strings.TrimSpace(h.OS.Name + " " + h.OS.Version),
			Model: h.Model, VM: h.Virtualization.InsideVM, Local: m.Local, Arch: h.CPU.Arch, Cores: h.CPU.Cores,
			MemTotal: h.Memory.TotalBytes, MemFree: h.Memory.AvailableBytes, LastSeen: m.LastSeen,
			Online: m.Local || time.Since(m.LastSeen) < 10*time.Minute}
		for _, f := range h.Filesystems {
			if f.TotalBytes > sm.DiskTotal {
				sm.DiskTotal, sm.DiskFree = f.TotalBytes, f.FreeBytes
			}
		}
		sm.Storage = []ShareStorage{}
		if m.Discovery != nil {
			sm.Services = len(m.Discovery.Services)
			sm.WizzOS = m.Discovery.WizzOS != nil
			for _, st := range m.Discovery.Storage {
				sm.Storage = append(sm.Storage, ShareStorage{Name: st.Name, Type: st.Type, Total: st.TotalBytes, Free: st.FreeBytes})
			}
		}
		for _, c := range checks {
			if c.MachineID != m.ID {
				continue
			}
			switch c.Status {
			case monitor.Up:
				sm.Up++
			case monitor.Down:
				sm.Down++
			}
		}
		out = append(out, sm)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"version": version.Version, "machines": out})
}

func (s *Server) handleShareSummary(w http.ResponseWriter, r *http.Request) {
	if !s.shareOK(w, r) {
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
