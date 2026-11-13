// Package travel is the travel mode: a check before leaving home that
// everything that must work while nobody is there does work, and, while
// away, one short message a day that says the home is fine (so a quiet
// phone means something).
package travel

import (
	"strconv"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/health"
	"github.com/wizz666/nestward/internal/i18n"
)

// Settings keys.
const (
	KeyOn    = "travel.on"    // "1" while away
	KeyUntil = "travel.until" // YYYY-MM-DD, last day away; "" = until turned off
	KeyHour  = "travel.hour"  // hour of the daily message, default 8
	KeyBIOS  = "travel.bios." // + machine id = date the power-on setting was confirmed
	KeySent  = "travel.sent"  // date the last daily message went out
)

// Status of one check.
const (
	OK     = "ok"
	Warn   = "warn"
	Bad    = "bad"
	Manual = "manual" // only a person can check this
)

// Check is one line of the list.
type Check struct {
	Key    string   `json:"key"`
	Status string   `json:"status"`
	Title  string   `json:"title"`
	Why    string   `json:"why"`              // why it matters while away
	Issues []string `json:"issues,omitempty"` // what is wrong, in plain words
	Fixes  []string `json:"fixes,omitempty"`
	// Machines lists the machines whose power-on setting must be confirmed.
	Machines []BIOS `json:"machines,omitempty"`
}

// BIOS is one physical machine and whether its "power on after a power
// cut" setting has been confirmed.
type BIOS struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Confirmed string `json:"confirmed,omitempty"` // date
}

// Report is the whole travel check.
type Report struct {
	Ready  bool    `json:"ready"` // nothing bad, no manual step left
	Checks []Check `json:"checks"`
	On     bool    `json:"on"`
	Until  string  `json:"until,omitempty"`
	Hour   int     `json:"hour"`
	Score  int     `json:"score"`
}

// groups says which health items belong to which travel check.
var groups = []struct {
	key   string
	items []string
	warn  bool // problems here are warnings, not blockers
}{
	{"alerts", []string{"alerts_missing", "alerts_failing"}, false},
	{"watch", []string{"heartbeat_missing", "heartbeat_failing"}, false},
	{"machines", []string{"machine_silent"}, false},
	{"services", []string{"service_down"}, true},
	{"power", []string{"boot_manual", "boot_array"}, false},
	{"backups", []string{"backup_missing", "backup_stale", "backup_failed", "backup_empty", "backup_none"}, true},
	{"restore", []string{"restore_failed"}, true},
	{"disks", []string{"disk_full", "disk_nearly_full", "guest_disk_full", "memory_low"}, true},
	{"exposure", []string{"exposed_open", "exposed_admin", "publish_broken"}, true},
}

// Build makes the travel check from the health report, the machines and
// the confirmed power-on settings (machine id -> date).
func Build(h health.Report, machines []health.Machine, bios map[string]string, t i18n.T) Report {
	byKey := map[string][]health.Item{}
	for _, it := range h.Items {
		byKey[it.Key] = append(byKey[it.Key], it)
	}
	rep := Report{Ready: true, Score: h.Score}
	for _, g := range groups {
		c := Check{Key: g.key, Status: OK, Title: t.Get("travel." + g.key), Why: t.Get("travel." + g.key + ".why")}
		seen := map[string]bool{}
		for _, k := range g.items {
			for _, it := range byKey[k] {
				c.Issues = append(c.Issues, it.Text)
				if it.Fix != "" && !seen[it.Fix] {
					seen[it.Fix] = true
					c.Fixes = append(c.Fixes, it.Fix)
				}
			}
		}
		if len(c.Issues) > 0 {
			c.Status = Bad
			if g.warn {
				c.Status = Warn
			}
		}
		if g.key == "restore" && len(c.Issues) == 0 && h.Restored == 0 {
			// No test restore yet: fine, but worth knowing.
			c.Status = Warn
			c.Issues = []string{t.Get("travel.restore.none")}
		}
		if c.Status == Bad {
			rep.Ready = false
		}
		rep.Checks = append(rep.Checks, c)
	}

	// The BIOS setting cannot be seen from the system: a person confirms it.
	c := Check{Key: "bios", Status: OK, Title: t.Get("travel.bios"), Why: t.Get("travel.bios.why")}
	for _, m := range machines {
		if m.Hardware.Virtualization.InsideVM {
			continue // a VM or VPS is switched on by its host
		}
		b := BIOS{ID: m.ID, Name: m.Name, Confirmed: bios[m.ID]}
		if b.Confirmed == "" {
			c.Status = Manual
			rep.Ready = false
		}
		c.Machines = append(c.Machines, b)
	}
	rep.Checks = append(rep.Checks, c)
	return rep
}

// Due reports whether the daily message should go out now: travel mode is
// on, the hour has come and today's message is not sent yet.
func Due(on bool, until string, hour int, sent string, now time.Time) bool {
	if !on {
		return false
	}
	today := now.Format("2006-01-02")
	if until != "" && today > until {
		return false
	}
	return now.Hour() >= hour && sent != today
}

// Expired reports whether the trip is over (the day after Until).
func Expired(until string, now time.Time) bool {
	return until != "" && now.Format("2006-01-02") > until
}

// Daily is the text of the daily message.
func Daily(h health.Report, machines, up, down int, t i18n.T) (title, body string, priority int) {
	n := strconv.Itoa
	if len(h.Items) == 0 {
		return t.Get("travel.daily.ok.title"), t.Get("travel.daily.ok.body", "machines", n(machines), "up", n(up), "score", n(h.Score)), 2
	}
	var lines []string
	for i, it := range h.Items {
		if i == 5 {
			lines = append(lines, t.Get("travel.daily.more", "count", n(len(h.Items)-5)))
			break
		}
		lines = append(lines, "• "+it.Text)
	}
	priority = 3
	if down > 0 {
		priority = 4
	}
	return t.Get("travel.daily.issues.title", "count", n(len(h.Items)), "score", n(h.Score)),
		t.Get("travel.daily.issues.body", "machines", n(machines), "up", n(up), "down", n(down)) + "\n" + strings.Join(lines, "\n"), priority
}
