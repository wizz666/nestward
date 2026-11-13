package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backup is one place where backups land, and when the newest one was
// made. Proxmox guests carry their own backup time (Guest.LastBackup).
type Backup struct {
	Kind string `json:"kind"` // "folder", "container" or "wizzos"
	Name string `json:"name"` // what to call it in the page, such as "Easy Backup: appdata"
	Path string `json:"path,omitempty"`
	// Last is when the newest backup was made; nil when none was found.
	Last *time.Time `json:"last,omitempty"`
	// Failed is set when the last run did not succeed (a backup container
	// that exited with an error).
	Failed bool   `json:"failed,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Tested is when a backup was last restored as a test (WizzOS does
	// this every night); TestOK says whether it worked.
	Tested    *time.Time `json:"tested,omitempty"`
	TestOK    bool       `json:"test_ok,omitempty"`
	TestedApp string     `json:"tested_app,omitempty"`
}

// backupImage matches containers that make backups and then exit, such
// as Nextcloud AIO's borgbackup, borgmatic or restic.
var backupImage = regexp.MustCompile(`(?i)(borg|restic|kopia|duplicati|backup)`)

// stateLister can say how a container last ended.
type stateLister interface {
	LastRun(ctx context.Context, id string) (finished time.Time, exitCode int, err error)
}

// containerBackups adds a Backup for each backup container that runs and
// exits. A container that runs all the time (Duplicati's web page, say)
// keeps its own schedule, which Nestward cannot see.
func containerBackups(ctx context.Context, l Lister, r *Result) {
	sl, ok := l.(stateLister)
	if !ok {
		return
	}
	for _, s := range r.Services {
		if s.State != "exited" || !(backupImage.MatchString(s.Image) || backupImage.MatchString(s.Name)) {
			continue
		}
		at, code, err := sl.LastRun(ctx, s.ID)
		if err != nil || at.IsZero() {
			continue
		}
		b := Backup{Kind: "container", Name: s.Name, Last: &at}
		if code != 0 {
			b.Failed = true
			b.Detail = "exit " + strconv.Itoa(code)
		}
		r.Backups = append(r.Backups, b)
	}
}

// ReadBackups finds backup folders under root (the host's file system):
// the ones Unraid backup plugins write to, plus any in extra (host paths,
// from NESTWARD_BACKUP_PATHS). For each it looks for the newest file.
func ReadBackups(root string, extra []string, r *Result) {
	if root == "" {
		root = "/"
	}
	type place struct{ name, path string }
	var places []place

	// Easy Backup (Unraid plugin): appdata, virtual machines and the flash drive.
	var eb map[string]any
	if b, err := os.ReadFile(filepath.Join(root, "boot/config/plugins/easybackup/config.json")); err == nil && json.Unmarshal(b, &eb) == nil {
		for _, k := range []struct{ on, path, name string }{
			{"ENABLE_APPDATA_BACKUP", "APPDATA_BACKUP_PATH", "Easy Backup: appdata"},
			{"ENABLE_VM_BACKUP", "VM_BACKUP_PATH", "Easy Backup: VMs"},
			{"ENABLE_FLASH_BACKUP", "FLASH_BACKUP_PATH", "Easy Backup: flash"},
		} {
			if on, _ := eb[k.on].(bool); on {
				if p, _ := eb[k.path].(string); p != "" {
					places = append(places, place{k.name, p})
				}
			}
		}
	}
	// Appdata Backup (Unraid plugin).
	var ab map[string]any
	if b, err := os.ReadFile(filepath.Join(root, "boot/config/plugins/appdata.backup/config.json")); err == nil && json.Unmarshal(b, &ab) == nil {
		if p, _ := ab["destination"].(string); p != "" {
			places = append(places, place{"Appdata Backup", p})
		}
	}
	for _, p := range extra {
		if p = strings.TrimSpace(p); p != "" {
			places = append(places, place{p, p})
		}
	}

	seen := map[string]bool{}
	for _, p := range places {
		clean := path.Clean("/" + filepath.ToSlash(p.path))
		if seen[clean] {
			continue
		}
		seen[clean] = true
		b := Backup{Kind: "folder", Name: p.name, Path: clean}
		if at, ok := newestFile(filepath.Join(root, filepath.FromSlash(clean))); ok {
			b.Last = &at
		}
		r.Backups = append(r.Backups, b)
	}
}

// Walking a backup folder costs disk reads, so the answer is reused.
var (
	newestMu    sync.Mutex
	newestCache = map[string]newestEntry{}
	newestAge   = 10 * time.Minute
)

type newestEntry struct {
	at, checked time.Time
	ok          bool
}

// newestFile finds the newest file up to three folders deep (backup tools
// keep one folder per app or date), looking at no more than 20 000 entries.
func newestFile(dir string) (time.Time, bool) {
	newestMu.Lock()
	if e, ok := newestCache[dir]; ok && time.Since(e.checked) < newestAge {
		newestMu.Unlock()
		return e.at, e.ok
	}
	newestMu.Unlock()

	var newest time.Time
	found, n := false, 0
	errStop := errors.New("stop")
	base := strings.Count(filepath.Clean(dir), string(filepath.Separator))
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > 20000 {
			return errStop
		}
		if d.IsDir() {
			if strings.EqualFold(d.Name(), ".Recycle.Bin") || strings.Count(p, string(filepath.Separator))-base >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest, found = info.ModTime().UTC(), true
		}
		return nil
	})

	newestMu.Lock()
	newestCache[dir] = newestEntry{at: newest, ok: found, checked: time.Now()}
	newestMu.Unlock()
	return newest, found
}
