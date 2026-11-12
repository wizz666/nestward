package health

import (
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/i18n"
)

func TestBackupItems(t *testing.T) {
	tr := i18n.MustLoad().For("en")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	fresh, old := now.Add(-24*time.Hour), now.Add(-20*24*time.Hour)
	keys := func(r Report) map[string]Item {
		m := map[string]Item{}
		for _, it := range r.Items {
			m[it.Key] = it
		}
		return m
	}

	pve := &discovery.Result{Available: true, Services: []discovery.Service{
		{Name: "ct-100", State: "running", Guest: &discovery.Guest{Name: "tunnel", BackupChecked: true}},
		{Name: "ct-101", State: "running", Guest: &discovery.Guest{Name: "apps", BackupChecked: true, LastBackup: &old}},
		{Name: "vm-102", State: "exited", Guest: &discovery.Guest{Name: "off", BackupChecked: true}},
	}}
	tower := &discovery.Result{Available: true, Backups: []discovery.Backup{
		{Kind: "folder", Name: "Easy Backup: appdata", Last: &fresh},
		{Kind: "container", Name: "borg", Last: &fresh, Failed: true, Detail: "exit 1"},
	}}
	r := Compute(Input{Now: now, Machines: []Machine{{ID: "p", Name: "pve", Discovery: pve}, {ID: "t", Name: "Tower", Discovery: tower}}}, tr)
	k := keys(r)
	if it := k["backup_missing"]; it.Points != ptsNoBackup || it.Params["names"] != "tunnel" {
		t.Errorf("missing = %+v", it)
	}
	if it := k["backup_stale"]; it.Points != ptsBackupStale || it.Params["days"] != "20" {
		t.Errorf("stale = %+v", it)
	}
	if it := k["backup_failed"]; it.Points != ptsBackupFailed {
		t.Errorf("failed = %+v", it)
	}
	if _, ok := k["backup_none"]; ok {
		t.Error("backups exist")
	}

	// Everything fresh: a good line, no items.
	ok := &discovery.Result{Available: true, Backups: []discovery.Backup{{Kind: "folder", Name: "x", Last: &fresh}}}
	r = Compute(Input{Now: now, Machines: []Machine{{ID: "t", Name: "Tower", Discovery: ok}}}, tr)
	for k := range keys(r) {
		if len(k) > 7 && k[:7] == "backup_" {
			t.Errorf("fresh backups gave item %s", k)
		}
	}
	if !containsGood(r, "Backups are fresh") {
		t.Errorf("items %v good %v", r.Items, r.Good)
	}

	// Nothing anywhere.
	none := &discovery.Result{Available: true}
	r = Compute(Input{Now: now, Machines: []Machine{{ID: "t", Name: "Tower", Discovery: none}}}, tr)
	if it, ok := keys(r)["backup_none"]; !ok || it.Points != ptsBackupNone {
		t.Errorf("none = %+v", r.Items)
	}
}
