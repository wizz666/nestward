// Package health turns what Nestward knows into one number from 0 to 100
// for the whole home, and a list of plain-language things to do. Every
// deduction says why, and most have a button that fixes or shows it.
// Nestward only deducts for things it can see; it never guesses.
package health

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/exposure"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/publish"
)

// Action is what the button next to an item does in the web page.
type Action struct {
	Kind   string `json:"kind"`             // alerts_setup, heartbeat_setup, show_service, show_machine, show_publications, show_exposure
	Target string `json:"target,omitempty"` // check id, machine id …
}

// Item is one deduction.
type Item struct {
	Key    string            `json:"key"`
	Points int               `json:"points"`
	Params map[string]string `json:"params,omitempty"`
	Text   string            `json:"text"`
	Fix    string            `json:"fix"` // what to do, in plain words
	Action *Action           `json:"action,omitempty"`
}

// Report is the score.
type Report struct {
	Score     int      `json:"score"`
	Grade     string   `json:"grade"` // great, good, attention, act
	GradeText string   `json:"grade_text"`
	Items     []Item   `json:"items"`
	Good      []string `json:"good"` // short list of what is fine, so the page is not only bad news
	// Restored counts guests whose backup passed a test restore lately.
	Restored int `json:"restored"`
}

// Machine is one machine's hardware for the disk and memory checks.
type Machine struct {
	ID       string
	Name     string
	Hardware hardware.Report
	// Discovery is the machine's last service report, for the check of
	// what starts by itself after a power cut. Nil when unknown.
	Discovery *discovery.Result
}

// Input is everything the score is based on.
type Input struct {
	Machines     []Machine
	Checks       []monitor.Check
	Alerts       alert.Status
	Heartbeat    alert.HeartbeatStatus
	Publications []publish.Publication
	// Exposure is what each public address shows a stranger.
	Exposure []exposure.Finding
	// Now is the time to measure backup ages from; zero = time.Now().
	Now time.Time
}

// Caps keep one kind of problem from eating the whole score.
const (
	ptsAlertsMissing    = 15
	ptsAlertsFailing    = 10
	ptsHeartbeatMissing = 10
	ptsHeartbeatFailing = 10
	ptsMachineSilent    = 10
	ptsServiceDown      = 5
	maxServiceDown      = 25
	ptsDiskFull         = 15 // 95 % or more
	ptsDiskNearlyFull   = 8  // 90 % or more
	ptsMemoryLow        = 5
	ptsPublishBroken    = 5
	maxPublishBroken    = 15
	ptsExposedOpen      = 20 // an open shell or AI server on the internet
	maxExposedOpen      = 40
	ptsExposedAdmin     = 3 // an admin login page anyone can reach
	maxExposedAdmin     = 12
	ptsNoAutostart      = 3 // a running service that will not come back after a power cut
	maxNoAutostart      = 15
	ptsArrayManual      = 10 // Unraid's array waits for someone to press Start
	ptsNoBackup         = 3  // a running Proxmox guest with no backup at all
	maxNoBackup         = 15
	ptsBackupStale      = 4 // newest backup older than backupMaxAge
	ptsBackupFailed     = 5 // the last backup run failed
	maxBackupStale      = 12
	ptsBackupNone       = 5 // no backups found anywhere
	backupMaxAge        = 8 * 24 * time.Hour
	ptsRestoreFailed    = 5 // a test restore of a backup failed
	maxRestoreFailed    = 10
	restoreMaxAge       = 45 * 24 * time.Hour // older test results do not count
	ptsGuestDiskFull    = 3                   // a Proxmox container's own disk is 90 % full or more
	maxGuestDiskFull    = 9
)

// Compute works out the score.
func Compute(in Input, t i18n.T) Report {
	var items []Item
	var good []string
	add := func(key string, pts int, action *Action, kv ...string) {
		params := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			params[kv[i]] = kv[i+1]
		}
		items = append(items, Item{Key: key, Points: pts, Params: params, Action: action,
			Text: t.Get("health."+key, kv...), Fix: t.Get("health."+key+".fix", kv...)})
	}

	// Alerts reach the phone.
	switch {
	case !in.Alerts.Configured:
		add("alerts_missing", ptsAlertsMissing, &Action{Kind: "alerts_setup"})
	case in.Alerts.LastError != "":
		add("alerts_failing", ptsAlertsFailing, &Action{Kind: "alerts_setup"}, "error", in.Alerts.LastError)
	default:
		good = append(good, t.Get("health.good.alerts"))
	}

	// Someone outside notices when the whole home goes quiet.
	switch {
	case !in.Heartbeat.Configured:
		add("heartbeat_missing", ptsHeartbeatMissing, &Action{Kind: "heartbeat_setup"})
	case in.Heartbeat.LastError != "":
		add("heartbeat_failing", ptsHeartbeatFailing, &Action{Kind: "heartbeat_setup"})
	default:
		good = append(good, t.Get("health.good.heartbeat"))
	}

	// What is not working right now.
	silent := map[string]bool{}
	serviceDown := 0
	var downChecks []monitor.Check
	for _, c := range in.Checks {
		if c.Status != monitor.Down {
			continue
		}
		if c.Kind == monitor.KindMachine {
			silent[c.MachineID] = true
			add("machine_silent", ptsMachineSilent, &Action{Kind: "show_machine", Target: c.MachineID}, "machine", c.Machine)
			continue
		}
		downChecks = append(downChecks, c)
	}
	// One line per service, even when both its container and page fail.
	seen := map[string]bool{}
	sort.Slice(downChecks, func(i, j int) bool { return downChecks[i].ID < downChecks[j].ID })
	for _, c := range downChecks {
		key := c.MachineID + "/" + c.Service
		if seen[key] || silent[c.MachineID] {
			continue
		}
		seen[key] = true
		pts := ptsServiceDown
		if serviceDown+pts > maxServiceDown {
			pts = maxServiceDown - serviceDown
		}
		serviceDown += pts
		name := c.App
		if name == "" {
			name = c.Service
		}
		add("service_down", pts, &Action{Kind: "show_service", Target: c.MachineID + "/" + c.Service},
			"app", name, "machine", c.Machine)
	}
	if len(in.Checks) > 0 && len(seen) == 0 && len(silent) == 0 {
		good = append(good, t.Get("health.good.services", "count", strconv.Itoa(countUp(in.Checks))))
	}

	// Disks and memory.
	diskOK := true
	for _, m := range in.Machines {
		for _, f := range m.Hardware.Filesystems {
			if f.TotalBytes == 0 {
				continue
			}
			used := 100 - int(f.FreeBytes*100/f.TotalBytes)
			switch {
			case used >= 95:
				diskOK = false
				add("disk_full", ptsDiskFull, &Action{Kind: "show_machine", Target: m.ID},
					"machine", m.Name, "mount", f.Mount, "percent", strconv.Itoa(used), "free", t.GB(f.FreeBytes))
			case used >= 90:
				diskOK = false
				add("disk_nearly_full", ptsDiskNearlyFull, &Action{Kind: "show_machine", Target: m.ID},
					"machine", m.Name, "mount", f.Mount, "percent", strconv.Itoa(used), "free", t.GB(f.FreeBytes))
			}
		}
		mem := m.Hardware.Memory
		if mem.TotalBytes > 0 && mem.AvailableBytes*10 < mem.TotalBytes {
			add("memory_low", ptsMemoryLow, &Action{Kind: "show_machine", Target: m.ID},
				"machine", m.Name, "free", t.GB(mem.AvailableBytes), "total", t.GB(mem.TotalBytes))
		}
	}
	guestFull := 0
	for _, m := range in.Machines {
		if m.Discovery == nil {
			continue
		}
		for _, sv := range m.Discovery.Services {
			g := sv.Guest
			if g == nil || g.DiskMax <= 0 || g.DiskUsed <= 0 || sv.State != "running" {
				continue
			}
			used := int(g.DiskUsed * 100 / g.DiskMax)
			if used < 90 {
				continue
			}
			diskOK = false
			name := g.Name
			if name == "" {
				name = sv.Name
			}
			pts := min(ptsGuestDiskFull, maxGuestDiskFull-guestFull)
			guestFull += pts
			add("guest_disk_full", pts, &Action{Kind: "show_machine", Target: m.ID},
				"name", name, "machine", m.Name, "percent", strconv.Itoa(used), "free", t.GB(uint64(g.DiskMax-g.DiskUsed)))
		}
	}
	if diskOK && len(in.Machines) > 0 {
		good = append(good, t.Get("health.good.disks"))
	}

	// What comes back by itself after a power cut.
	bootSeen, bootOK, noStart := false, true, 0
	for _, m := range in.Machines {
		d := m.Discovery
		if d == nil || !d.Available {
			continue
		}
		bootSeen = true
		if d.Boot != nil && d.Boot.Unraid != nil && !d.Boot.Unraid.ArrayAutostart {
			bootOK = false
			add("boot_array", ptsArrayManual, &Action{Kind: "show_machine", Target: m.ID}, "machine", m.Name)
		}
		names := d.NotStarting()
		if len(names) == 0 {
			continue
		}
		bootOK = false
		pts := min(ptsNoAutostart*len(names), maxNoAutostart-noStart)
		noStart += pts
		add("boot_manual", pts, &Action{Kind: "show_machine", Target: m.ID},
			"machine", m.Name, "count", strconv.Itoa(len(names)), "names", strings.Join(names, ", "))
	}
	if bootSeen && bootOK {
		good = append(good, t.Get("health.good.boot"))
	}

	// Backups: there are some, and they are fresh.
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	days := func(at time.Time) string { return strconv.Itoa(int(now.Sub(at).Hours() / 24)) }
	sources, backupBad, noBackup, stale := 0, false, 0, 0
	restoredOK, restoreBad := 0, 0
	for _, m := range in.Machines {
		d := m.Discovery
		if d == nil || !d.Available {
			continue
		}
		var missing []string
		for _, s := range d.Services {
			g := s.Guest
			if g != nil && g.RestoreTest != nil && now.Sub(g.RestoreTest.TestedAt) < restoreMaxAge {
				name := g.Name
				if name == "" {
					name = s.Name
				}
				switch g.RestoreTest.Result {
				case discovery.RestoreOK:
					restoredOK++
				case discovery.RestoreFailed:
					pts := min(ptsRestoreFailed, maxRestoreFailed-restoreBad)
					restoreBad += pts
					add("restore_failed", pts, &Action{Kind: "show_machine", Target: m.ID},
						"name", name, "machine", m.Name, "date", g.RestoreTest.TestedAt.Local().Format("2006-01-02"))
				}
			}
			if g == nil || !g.BackupChecked || s.State != "running" {
				continue
			}
			sources++
			name := g.Name
			if name == "" {
				name = s.Name
			}
			switch {
			case g.LastBackup == nil:
				missing = append(missing, name)
			case now.Sub(*g.LastBackup) > backupMaxAge:
				backupBad = true
				pts := min(ptsBackupStale, maxBackupStale-stale)
				stale += pts
				add("backup_stale", pts, &Action{Kind: "show_machine", Target: m.ID},
					"name", name, "machine", m.Name, "days", days(*g.LastBackup))
			}
		}
		if len(missing) > 0 {
			backupBad = true
			pts := min(ptsNoBackup*len(missing), maxNoBackup-noBackup)
			noBackup += pts
			add("backup_missing", pts, &Action{Kind: "show_machine", Target: m.ID},
				"machine", m.Name, "count", strconv.Itoa(len(missing)), "names", strings.Join(missing, ", "))
		}
		for _, b := range d.Backups {
			sources++
			if b.Tested != nil && now.Sub(*b.Tested) < restoreMaxAge {
				if b.TestOK {
					restoredOK++
				} else {
					pts := min(ptsRestoreFailed, maxRestoreFailed-restoreBad)
					restoreBad += pts
					name := b.Name
					if b.TestedApp != "" {
						name = b.TestedApp
					}
					key := "restore_failed"
					if b.Kind == "wizzos" {
						key = "restore_failed_wizzos"
					}
					add(key, pts, &Action{Kind: "show_machine", Target: m.ID},
						"name", name, "machine", m.Name, "date", b.Tested.Local().Format("2006-01-02"))
				}
			}
			switch {
			case b.Failed:
				backupBad = true
				pts := min(ptsBackupFailed, maxBackupStale-stale)
				stale += pts
				add("backup_failed", pts, &Action{Kind: "show_machine", Target: m.ID}, "name", b.Name, "machine", m.Name)
			case b.Last == nil:
				backupBad = true
				pts := min(ptsBackupStale, maxBackupStale-stale)
				stale += pts
				add("backup_empty", pts, &Action{Kind: "show_machine", Target: m.ID}, "name", b.Name, "machine", m.Name)
			case now.Sub(*b.Last) > backupMaxAge:
				backupBad = true
				pts := min(ptsBackupStale, maxBackupStale-stale)
				stale += pts
				add("backup_stale", pts, &Action{Kind: "show_machine", Target: m.ID},
					"name", b.Name, "machine", m.Name, "days", days(*b.Last))
			}
		}
	}
	switch {
	case sources == 0 && len(in.Machines) > 0 && anyDiscovery(in.Machines):
		add("backup_none", ptsBackupNone, nil)
	case sources > 0 && !backupBad:
		good = append(good, t.Get("health.good.backups", "count", strconv.Itoa(sources)))
	}
	if restoredOK > 0 && restoreBad == 0 {
		good = append(good, t.Get("health.good.restored", "count", strconv.Itoa(restoredOK)))
	}

	// Published addresses that do not answer.
	broken := 0
	for _, p := range in.Publications {
		if p.Status != publish.Active || p.LastCheck == nil || p.LastCheck.Result != "unreachable" {
			continue
		}
		pts := ptsPublishBroken
		if broken+pts > maxPublishBroken {
			pts = maxPublishBroken - broken
		}
		broken += pts
		add("publish_broken", pts, &Action{Kind: "show_publications"}, "host", p.Hostname)
	}

	// What strangers can reach on the internet.
	open, admin, safe := 0, 0, 0
	for _, f := range in.Exposure {
		if f.Accepted {
			continue
		}
		switch f.Verdict {
		case exposure.Open:
			pts := min(ptsExposedOpen, maxExposedOpen-open)
			open += pts
			if pts > 0 {
				add("exposed_open", pts, &Action{Kind: "show_exposure", Target: f.Host}, "host", f.Host, "app", appOr(f))
			}
		case exposure.AdminLogin:
			pts := min(ptsExposedAdmin, maxExposedAdmin-admin)
			admin += pts
			if pts > 0 {
				add("exposed_admin", pts, &Action{Kind: "show_exposure", Target: f.Host}, "host", f.Host, "app", appOr(f))
			}
		case exposure.Protected:
			safe++
		}
	}
	if len(in.Exposure) > 0 && open == 0 && admin == 0 {
		good = append(good, t.Get("health.good.exposure", "count", strconv.Itoa(safe)))
	}

	total := 0
	for _, it := range items {
		total += it.Points
	}
	score := 100 - total
	if score < 0 {
		score = 0
	}
	// Biggest problems first.
	sort.SliceStable(items, func(i, j int) bool { return items[i].Points > items[j].Points })
	grade := "act"
	switch {
	case score >= 90:
		grade = "great"
	case score >= 75:
		grade = "good"
	case score >= 50:
		grade = "attention"
	}
	if items == nil {
		items = []Item{}
	}
	if good == nil {
		good = []string{}
	}
	return Report{Score: score, Grade: grade, GradeText: t.Get("health.grade." + grade), Items: items, Good: good, Restored: restoredOK}
}

func anyDiscovery(ms []Machine) bool {
	for _, m := range ms {
		if m.Discovery != nil && m.Discovery.Available {
			return true
		}
	}
	return false
}

func countUp(cs []monitor.Check) int {
	n := 0
	for _, c := range cs {
		if c.Status == monitor.Up && c.Kind != monitor.KindMachine {
			n++
		}
	}
	return n
}

// String is for logs.
func (r Report) String() string {
	return fmt.Sprintf("%d (%s, %d items)", r.Score, r.Grade, len(r.Items))
}

func appOr(f exposure.Finding) string {
	if f.App != "" {
		return f.App
	}
	return f.Host
}
