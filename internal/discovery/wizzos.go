package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// WizzOS is what a WizzOS panel on this machine says about itself. WizzOS
// (an app panel that installs apps from CasaOS app stores) writes a small
// status file; Nestward reads it so that its apps get their real names,
// its backups count, and apps on trial or stopped on purpose do not alert.
type WizzOS struct {
	Version string      `json:"version"`
	Updated time.Time   `json:"updated"`
	Apps    []WizzOSApp `json:"apps"`
}

// WizzOSApp is one app installed through WizzOS.
type WizzOSApp struct {
	Name        string `json:"name"` // compose project and label wizzos.app
	Title       string `json:"title"`
	Status      string `json:"status"`
	Trial       bool   `json:"trial,omitempty"`
	UserStopped bool   `json:"user_stopped,omitempty"`
}

// wizzosFile is the status file as WizzOS writes it.
type wizzosFile struct {
	Product string      `json:"product"`
	Version string      `json:"version"`
	Updated int64       `json:"updated"`
	Apps    []WizzOSApp `json:"apps"`
	Backup  *struct {
		Configured bool   `json:"configured"`
		Target     string `json:"target"`
		LastRun    *struct {
			Time  int64  `json:"time"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"last_run"`
		LastTest *struct {
			Time int64  `json:"time"`
			OK   bool   `json:"ok"`
			App  string `json:"app"`
		} `json:"last_test"`
	} `json:"backup"`
}

// wizzosStale is how old the status file may be before it is ignored
// (WizzOS rewrites it every minute while it runs).
const wizzosStale = 30 * time.Minute

// WizzOSStatusPath is where WizzOS keeps its status file, relative to the
// host root.
const WizzOSStatusPath = "DATA/WizzOS/status.json"

// ReadWizzOS reads WizzOS's status file under root (the host's file
// system) and folds it into r. A missing, unreadable or old file changes
// nothing.
func ReadWizzOS(root string, r *Result) {
	if root == "" {
		root = "/"
	}
	p := os.Getenv("NESTWARD_WIZZOS_STATUS")
	if p == "" {
		p = filepath.Join(root, filepath.FromSlash(WizzOSStatusPath))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var f wizzosFile
	if json.Unmarshal(b, &f) != nil || f.Product != "WizzOS" || f.Updated == 0 {
		return
	}
	updated := time.Unix(f.Updated, 0).UTC()
	now := r.At
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(updated) > wizzosStale {
		return
	}
	r.WizzOS = &WizzOS{Version: f.Version, Updated: updated, Apps: f.Apps}

	apps := map[string]WizzOSApp{}
	for _, a := range f.Apps {
		apps[a.Name] = a
	}
	for i := range r.Services {
		s := &r.Services[i]
		a, ok := apps[s.WizzOS]
		if !ok {
			continue
		}
		if a.Title != "" && (s.Kind == KindUnknown || s.Kind == KindOwn) {
			s.AppName = a.Title
		}
		s.Quiet = a.Trial || a.UserStopped
	}

	if bk := f.Backup; bk != nil && bk.Configured {
		b := Backup{Kind: "wizzos", Name: "WizzOS", Path: bk.Target}
		if lr := bk.LastRun; lr != nil && lr.Time > 0 {
			at := time.Unix(lr.Time, 0).UTC()
			b.Last = &at
			if !lr.OK {
				b.Failed = true
				b.Detail = lr.Error
			}
		}
		if lt := bk.LastTest; lt != nil && lt.Time > 0 {
			at := time.Unix(lt.Time, 0).UTC()
			b.Tested, b.TestOK, b.TestedApp = &at, lt.OK, lt.App
		}
		r.Backups = append(r.Backups, b)
	}
}
