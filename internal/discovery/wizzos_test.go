package discovery

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeWizzOS(t *testing.T, root, body string) {
	t.Helper()
	p := filepath.Join(root, "DATA", "WizzOS")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "status.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadWizzOS(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	ts := strconv.FormatInt(now.Unix(), 10)
	run := strconv.FormatInt(now.Add(-2*time.Hour).Unix(), 10)
	writeWizzOS(t, root, `{"product":"WizzOS","version":"0.9.0","updated":`+ts+`,
	  "apps":[{"name":"notes","title":"My Notes","status":"running"},
	          {"name":"memos","title":"Memos","status":"running","trial":true},
	          {"name":"files","title":"Files","status":"stopped","user_stopped":true}],
	  "backup":{"configured":true,"target":"s3 (r2)","last_run":{"time":`+run+`,"ok":true},
	            "last_test":{"time":`+run+`,"ok":true,"app":"notes"}}}`)
	r := Result{At: now, Services: []Service{
		{Name: "notes-web", Kind: KindUnknown, AppName: "Notes Web", WizzOS: "notes"},
		{Name: "memos", Kind: KindApp, AppName: "Memos", WizzOS: "memos"},
		{Name: "files", Kind: KindUnknown, WizzOS: "files", State: "exited"},
		{Name: "other", Kind: KindUnknown, AppName: "Other"},
	}}
	ReadWizzOS(root, &r)
	if r.WizzOS == nil || r.WizzOS.Version != "0.9.0" || len(r.WizzOS.Apps) != 3 {
		t.Fatalf("wizzos = %+v", r.WizzOS)
	}
	if r.Services[0].AppName != "My Notes" {
		t.Errorf("unknown service should take the WizzOS title, got %q", r.Services[0].AppName)
	}
	if r.Services[1].AppName != "Memos" || !r.Services[1].Quiet {
		t.Errorf("trial app: %+v", r.Services[1])
	}
	if !r.Services[2].Quiet || r.Services[0].Quiet || r.Services[3].Quiet {
		t.Errorf("quiet flags wrong: %+v", r.Services)
	}
	if len(r.Backups) != 1 {
		t.Fatalf("backups = %+v", r.Backups)
	}
	b := r.Backups[0]
	if b.Kind != "wizzos" || b.Last == nil || b.Failed || b.Tested == nil || !b.TestOK || b.TestedApp != "notes" {
		t.Errorf("backup = %+v", b)
	}
}

func TestReadWizzOSIgnoresOldOrForeign(t *testing.T) {
	root := t.TempDir()
	old := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
	writeWizzOS(t, root, `{"product":"WizzOS","updated":`+old+`,"apps":[]}`)
	r := Result{At: time.Now()}
	ReadWizzOS(root, &r)
	if r.WizzOS != nil {
		t.Error("an old status file must be ignored")
	}
	writeWizzOS(t, root, `{"product":"Other","updated":`+strconv.FormatInt(time.Now().Unix(), 10)+`}`)
	ReadWizzOS(root, &r)
	if r.WizzOS != nil {
		t.Error("a file from another product must be ignored")
	}
	ReadWizzOS(t.TempDir(), &r) // missing file
	if r.WizzOS != nil || len(r.Backups) != 0 {
		t.Error("missing file must change nothing")
	}
}
