package health

import (
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/i18n"
)

func TestBootItems(t *testing.T) {
	tr := i18n.MustLoad().For("en")
	ok := &discovery.Result{Available: true, Services: []discovery.Service{
		{Name: "plex", AppName: "Plex", State: "running", Autostart: discovery.AutostartYes},
	}}
	r := Compute(Input{Machines: []Machine{{ID: "a", Name: "Tower", Discovery: ok}}}, tr)
	if !containsGood(r, "starts by itself") {
		t.Errorf("good = %v", r.Good)
	}

	bad := &discovery.Result{Available: true, Services: []discovery.Service{
		{Name: "toy", AppName: "Toy", State: "running", Autostart: discovery.AutostartNo},
		{Name: "old", AppName: "Old", State: "exited", Autostart: discovery.AutostartNo},
	}, Boot: &discovery.Boot{Unraid: &discovery.UnraidBoot{ArrayAutostart: false}}}
	r = Compute(Input{Machines: []Machine{{ID: "a", Name: "Tower", Discovery: bad}}}, tr)
	keys := map[string]Item{}
	for _, it := range r.Items {
		keys[it.Key] = it
	}
	if it, ok := keys["boot_manual"]; !ok || it.Points != ptsNoAutostart || !strings.Contains(it.Text, "Toy") || strings.Contains(it.Text, "Old") {
		t.Errorf("boot_manual = %+v", keys["boot_manual"])
	}
	if it, ok := keys["boot_array"]; !ok || it.Points != ptsArrayManual || it.Action == nil || it.Action.Target != "a" {
		t.Errorf("boot_array = %+v", keys["boot_array"])
	}
	if containsGood(r, "starts by itself") {
		t.Errorf("good should not praise boot: %v", r.Good)
	}

	// Many services: capped.
	many := &discovery.Result{Available: true}
	for i := 0; i < 10; i++ {
		many.Services = append(many.Services, discovery.Service{Name: "s", AppName: "S", State: "running", Autostart: discovery.AutostartNo})
	}
	r = Compute(Input{Machines: []Machine{{ID: "a", Name: "Tower", Discovery: many}}}, tr)
	for _, it := range r.Items {
		if it.Key == "boot_manual" && it.Points != maxNoAutostart {
			t.Errorf("cap: %d", it.Points)
		}
	}
}

func containsGood(r Report, s string) bool {
	for _, g := range r.Good {
		if strings.Contains(g, s) {
			return true
		}
	}
	return false
}
