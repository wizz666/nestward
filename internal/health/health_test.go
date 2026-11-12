package health

import (
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/exposure"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/publish"
)

const gib = hardware.GiB

func healthyHome() Input {
	return Input{
		Machines: []Machine{{ID: "tower", Name: "Tower", Hardware: hardware.Report{
			Memory:      hardware.MemoryInfo{TotalBytes: 16 * gib, AvailableBytes: 6 * gib},
			Filesystems: []hardware.Filesystem{{Mount: "/mnt/user", TotalBytes: 1000 * gib, FreeBytes: 400 * gib}},
		}}},
		Checks: []monitor.Check{
			{ID: "container/tower/plex", Kind: monitor.KindContainer, MachineID: "tower", Machine: "Tower", Service: "plex", App: "Plex", Status: monitor.Up},
			{ID: "web/tower/plex", Kind: monitor.KindWeb, MachineID: "tower", Machine: "Tower", Service: "plex", App: "Plex", Status: monitor.Up},
		},
		Alerts:    alert.Status{Configured: true},
		Heartbeat: alert.HeartbeatStatus{Configured: true},
	}
}

func TestHealthyHomeScores100(t *testing.T) {
	r := Compute(healthyHome(), i18n.MustLoad().For("sv"))
	if r.Score != 100 || r.Grade != "great" || len(r.Items) != 0 || len(r.Good) != 4 {
		t.Errorf("report = %+v", r)
	}
}

func TestDeductionsAndOrder(t *testing.T) {
	in := healthyHome()
	in.Alerts = alert.Status{}
	in.Heartbeat = alert.HeartbeatStatus{}
	in.Checks[0].Status = monitor.Down                          // plex container
	in.Checks[1].Status = monitor.Down                          // and its page: still one problem
	in.Machines[0].Hardware.Filesystems[0].FreeBytes = 30 * gib // 97 % used
	in.Machines[0].Hardware.Memory.AvailableBytes = gib         // 6 % free
	in.Publications = []publish.Publication{{Hostname: "x.example.com", Status: publish.Active,
		LastCheck: &publish.Check{Result: "unreachable"}}}
	r := Compute(in, i18n.MustLoad().For("sv"))
	want := 100 - ptsAlertsMissing - ptsHeartbeatMissing - ptsServiceDown - ptsDiskFull - ptsMemoryLow - ptsPublishBroken
	if r.Score != want {
		t.Errorf("score = %d, want %d (%+v)", r.Score, want, r.Items)
	}
	if r.Items[0].Points < r.Items[len(r.Items)-1].Points {
		t.Error("biggest problems must come first")
	}
	keys := map[string]Item{}
	for _, it := range r.Items {
		keys[it.Key] = it
		if it.Text == "" || strings.HasPrefix(it.Text, "health.") || it.Fix == "" || strings.HasPrefix(it.Fix, "health.") {
			t.Errorf("%s: untranslated text %q / %q", it.Key, it.Text, it.Fix)
		}
		if strings.Contains(it.Text, "{") || strings.Contains(it.Fix, "{") {
			t.Errorf("%s: placeholder left: %q", it.Key, it.Text)
		}
	}
	if keys["service_down"].Action.Target != "tower/plex" || keys["alerts_missing"].Action.Kind != "alerts_setup" {
		t.Errorf("actions = %+v", keys)
	}
	if !strings.Contains(keys["disk_full"].Text, "97") {
		t.Errorf("disk text = %q", keys["disk_full"].Text)
	}
}

func TestCapsAndSilentMachine(t *testing.T) {
	in := healthyHome()
	in.Checks = nil
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		in.Checks = append(in.Checks, monitor.Check{ID: "container/tower/" + n, Kind: monitor.KindContainer,
			MachineID: "tower", Machine: "Tower", Service: n, Status: monitor.Down})
	}
	// pve is silent: its own services are not counted again.
	in.Checks = append(in.Checks,
		monitor.Check{ID: "machine/pve", Kind: monitor.KindMachine, MachineID: "pve", Machine: "pve", Status: monitor.Down},
		monitor.Check{ID: "container/pve/x", Kind: monitor.KindContainer, MachineID: "pve", Machine: "pve", Service: "x", Status: monitor.Down})
	r := Compute(in, i18n.MustLoad().For("en"))
	if r.Score != 100-maxServiceDown-ptsMachineSilent {
		t.Errorf("score = %d (%+v)", r.Score, r.Items)
	}
}

func TestGrades(t *testing.T) {
	in := healthyHome()
	in.Alerts = alert.Status{Configured: true, LastError: "ntfy answered 403"}
	r := Compute(in, i18n.MustLoad().For("en"))
	if r.Score != 90 || r.Grade != "great" {
		t.Errorf("90 = %+v", r)
	}
	in.Heartbeat = alert.HeartbeatStatus{}
	in.Checks[0].Status = monitor.Down
	r = Compute(in, i18n.MustLoad().For("en"))
	if r.Score != 75 || r.Grade != "good" {
		t.Errorf("75 = %d %s", r.Score, r.Grade)
	}
}

func TestExposure(t *testing.T) {
	tr := i18n.MustLoad().For("sv")
	in := healthyHome()
	f := func(host string, v exposure.Verdict, app string, accepted bool) exposure.Finding {
		return exposure.Finding{Host: host, Probe: exposure.Probe{Verdict: v, App: app}, Severity: exposure.SeverityOf(v), Accepted: accepted}
	}
	in.Exposure = []exposure.Finding{
		f("code.x", exposure.Open, "VS Code", false),
		f("pve.x", exposure.AdminLogin, "Proxmox", false),
		f("qnap.x", exposure.AdminLogin, "QNAP", true), // accepted: no deduction
		f("chat.x", exposure.Protected, "", false),
	}
	r := Compute(in, tr)
	if r.Score != 100-20-3 || len(r.Items) != 2 || r.Items[0].Key != "exposed_open" {
		t.Fatalf("report = %+v", r)
	}
	if r.Items[0].Action == nil || r.Items[0].Action.Kind != "show_exposure" || r.Items[0].Action.Target != "code.x" {
		t.Errorf("action = %+v", r.Items[0].Action)
	}
	if !strings.Contains(r.Items[0].Text, "VS Code") || !strings.Contains(r.Items[0].Text, "code.x") {
		t.Errorf("text = %q", r.Items[0].Text)
	}
	// Three open tools are capped at 40 points.
	in.Exposure = []exposure.Finding{f("a", exposure.Open, "", false), f("b", exposure.Open, "", false), f("c", exposure.Open, "", false)}
	if r := Compute(in, tr); r.Score != 60 {
		t.Errorf("cap: score %d", r.Score)
	}
	// All safe: a good line, no deduction.
	in.Exposure = []exposure.Finding{f("chat.x", exposure.Protected, "", false), f("blog.x", exposure.Website, "", false)}
	r = Compute(in, tr)
	if r.Score != 100 || len(r.Good) != 5 {
		t.Errorf("safe home = %+v", r)
	}
}
