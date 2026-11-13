package travel

import (
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/health"
	"github.com/wizz666/nestward/internal/i18n"
)

func lang(t *testing.T) i18n.T {
	b, err := i18n.Load()
	if err != nil {
		t.Fatal(err)
	}
	return b.For("en")
}

func find(r Report, key string) Check {
	for _, c := range r.Checks {
		if c.Key == key {
			return c
		}
	}
	return Check{}
}

func TestBuildGroupsHealthItems(t *testing.T) {
	tr := lang(t)
	h := health.Report{Score: 70, Restored: 0, Items: []health.Item{
		{Key: "boot_manual", Text: "2 things will not start", Fix: "Turn on autostart"},
		{Key: "guest_disk_full", Text: "nextcloudpi is 95 % full", Fix: "Clean up"},
		{Key: "service_down", Text: "Immich is not working"},
	}}
	machines := []health.Machine{
		{ID: "a", Name: "Tower"},
		{ID: "b", Name: "VPS", Hardware: hardware.Report{Virtualization: hardware.Virt{InsideVM: true}}},
		{ID: "c", Name: "pve"},
	}
	r := Build(h, machines, map[string]string{"a": "2026-11-20"}, tr)
	if r.Ready {
		t.Error("ready with a power problem and an unchecked BIOS")
	}
	if c := find(r, "power"); c.Status != Bad || len(c.Issues) != 1 || c.Fixes[0] != "Turn on autostart" {
		t.Errorf("power: %+v", c)
	}
	if c := find(r, "disks"); c.Status != Warn {
		t.Errorf("disks: %+v", c)
	}
	if c := find(r, "services"); c.Status != Warn {
		t.Errorf("services: %+v", c)
	}
	if c := find(r, "alerts"); c.Status != OK {
		t.Errorf("alerts: %+v", c)
	}
	if c := find(r, "restore"); c.Status != Warn || len(c.Issues) != 1 {
		t.Errorf("no restore test yet should warn: %+v", c)
	}
	b := find(r, "bios")
	if b.Status != Manual || len(b.Machines) != 2 {
		t.Fatalf("bios: %+v", b)
	}
	for _, m := range b.Machines {
		if m.Name == "VPS" {
			t.Error("a VPS has no BIOS to set")
		}
		if m.ID == "a" && m.Confirmed != "2026-11-20" {
			t.Error("confirmation lost")
		}
	}
}

func TestReadyWhenAllGood(t *testing.T) {
	r := Build(health.Report{Score: 100, Restored: 3}, []health.Machine{{ID: "a", Name: "Tower"}}, map[string]string{"a": "2026-11-20"}, lang(t))
	if !r.Ready {
		t.Fatalf("not ready: %+v", r.Checks)
	}
	for _, c := range r.Checks {
		if c.Status != OK || c.Title == "" || strings.HasPrefix(c.Title, "travel.") {
			t.Errorf("%s: %+v", c.Key, c)
		}
	}
}

func TestDueAndExpired(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse("2006-01-02 15:04", s); return v }
	if Due(false, "", 8, "", at("2026-11-20 09:00")) {
		t.Error("due while off")
	}
	if Due(true, "", 8, "", at("2026-11-20 07:59")) {
		t.Error("due before the hour")
	}
	if !Due(true, "", 8, "2026-11-19", at("2026-11-20 08:00")) {
		t.Error("not due at the hour")
	}
	if Due(true, "", 8, "2026-11-20", at("2026-11-20 12:00")) {
		t.Error("sent twice a day")
	}
	if !Due(true, "2026-11-20", 8, "", at("2026-11-20 08:30")) || Due(true, "2026-11-20", 8, "", at("2026-11-21 08:30")) {
		t.Error("until day wrong")
	}
	if Expired("2026-11-20", at("2026-11-20 23:59")) || !Expired("2026-11-20", at("2026-11-21 00:00")) || Expired("", at("2030-01-01 00:00")) {
		t.Error("expired wrong")
	}
}

func TestDaily(t *testing.T) {
	tr := lang(t)
	title, body, prio := Daily(health.Report{Score: 100}, 7, 110, 0, tr)
	if !strings.Contains(title, "all fine") || !strings.Contains(body, "110") || prio != 2 {
		t.Errorf("%q %q %d", title, body, prio)
	}
	var items []health.Item
	for i := 0; i < 7; i++ {
		items = append(items, health.Item{Text: "problem"})
	}
	title, body, prio = Daily(health.Report{Score: 60, Items: items}, 7, 100, 2, tr)
	if !strings.Contains(title, "7 things") || strings.Count(body, "• problem") != 5 || !strings.Contains(body, "2 more") || prio != 4 {
		t.Errorf("%q %q %d", title, body, prio)
	}
}

func TestGuestDiskFullInHealth(t *testing.T) {
	g := discovery.Guest{Type: "ct", VMID: 101, Name: "nextcloudpi", DiskMax: 18 << 30, DiskUsed: 17 << 30}
	in := health.Input{Machines: []health.Machine{{ID: "p", Name: "pve", Discovery: &discovery.Result{Available: true,
		Services: []discovery.Service{{Name: "ct-101", State: "running", Guest: &g}}}}}}
	h := health.Compute(in, lang(t))
	found := false
	for _, it := range h.Items {
		if it.Key == "guest_disk_full" && strings.Contains(it.Text, "nextcloudpi") && strings.Contains(it.Text, "94 %") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no guest disk item: %+v", h.Items)
	}
}
