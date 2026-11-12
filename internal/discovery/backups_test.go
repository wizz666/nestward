package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadBackupsEasyBackup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "boot/config/plugins/easybackup/config.json",
		`{"ENABLE_APPDATA_BACKUP":true,"APPDATA_BACKUP_PATH":"\/mnt\/user\/backup\/appdata\/","ENABLE_VM_BACKUP":true,"VM_BACKUP_PATH":"\/mnt\/user\/backup\/domains\/","ENABLE_FLASH_BACKUP":false,"FLASH_BACKUP_PATH":"\/mnt\/user\/backup\/flash\/"}`)
	writeFile(t, root, "mnt/user/backup/appdata/plex/2026-11-01_09.12.zip", "x")
	newer := filepath.Join(root, "mnt/user/backup/appdata/plex/2026-11-08_10.17.zip")
	writeFile(t, root, "mnt/user/backup/appdata/plex/2026-11-08_10.17.zip", "x")
	want := time.Date(2026, 11, 8, 10, 17, 0, 0, time.UTC)
	if err := os.Chtimes(newer, want, want); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "mnt/user/backup/appdata/plex/2026-11-01_09.12.zip")
	os.Chtimes(old, want.Add(-7*24*time.Hour), want.Add(-7*24*time.Hour))
	// The VM folder exists but is empty; files in the recycle bin do not count.
	os.MkdirAll(filepath.Join(root, "mnt/user/backup/domains"), 0o755)
	writeFile(t, root, "mnt/user/backup/domains/.Recycle.Bin/old.img", "x")

	r := Result{}
	ReadBackups(root, []string{"/srv/restic/"}, &r)
	if len(r.Backups) != 3 {
		t.Fatalf("backups = %+v", r.Backups)
	}
	by := map[string]Backup{}
	for _, b := range r.Backups {
		by[b.Name] = b
	}
	if b := by["Easy Backup: appdata"]; b.Last == nil || !b.Last.Equal(want) || b.Path != "/mnt/user/backup/appdata" {
		t.Errorf("appdata = %+v", b)
	}
	if b := by["Easy Backup: VMs"]; b.Last != nil {
		t.Errorf("empty VM folder = %+v", b)
	}
	if _, ok := by["Easy Backup: flash"]; ok {
		t.Error("flash backup is turned off and must not be listed")
	}
	if b := by["/srv/restic/"]; b.Kind != "folder" || b.Last != nil {
		t.Errorf("extra path = %+v", b)
	}
}

type stateFake struct {
	fakeLister
	runs map[string]time.Time
	code map[string]int
}

func (s stateFake) LastRun(_ context.Context, id string) (time.Time, int, error) {
	return s.runs[id], s.code[id], nil
}

func TestContainerBackups(t *testing.T) {
	at := time.Date(2026, 11, 12, 0, 1, 17, 0, time.UTC)
	l := stateFake{
		fakeLister: fakeLister{cs: []Container{
			{ID: "a", Names: []string{"/nextcloud-aio-borgbackup"}, Image: "ghcr.io/nextcloud-releases/aio-borgbackup:latest", State: "exited", Status: "Exited (0) 13 hours ago"},
			{ID: "b", Names: []string{"/restic-job"}, Image: "restic/restic", State: "exited", Status: "Exited (1) 2 days ago"},
			{ID: "c", Names: []string{"/plex"}, Image: "lscr.io/linuxserver/plex", State: "exited", Status: "Exited (0) 1 hour ago"},
			{ID: "d", Names: []string{"/duplicati"}, Image: "duplicati/duplicati", State: "running", Status: "Up 3 days"},
		}},
		runs: map[string]time.Time{"a": at, "b": at.Add(-48 * time.Hour), "c": at},
		code: map[string]int{"b": 1},
	}
	r := Discover(context.Background(), catalog(t), func() (Lister, error) { return l, nil })
	if len(r.Backups) != 2 {
		t.Fatalf("backups = %+v", r.Backups)
	}
	for _, b := range r.Backups {
		switch b.Name {
		case "nextcloud-aio-borgbackup":
			if b.Failed || b.Last == nil || !b.Last.Equal(at) {
				t.Errorf("borg = %+v", b)
			}
		case "restic-job":
			if !b.Failed || b.Detail != "exit 1" {
				t.Errorf("restic = %+v", b)
			}
		default:
			t.Errorf("unexpected %+v", b)
		}
	}
}
