package restoretest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
)

const ctConf = `#Cloudflared LXC
arch: amd64
cores: 1
features: keyctl=1,nesting=1
hostname: cloudflared
memory: 4096
net0: name=eth0,bridge=vmbr0,hwaddr=BC:24:11:8D:16:23,ip=dhcp,type=veth
mp0: /mnt/data,mp=/data
mp1: local-lvm:vm-150-disk-1,mp=/srv,size=1G
dev0: /dev/dri/renderD128,gid=104
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file
onboot: 1
protection: 1
rootfs: local-lvm:vm-150-disk-0,size=2G
unprivileged: 1
`

// fake is a pretend Proxmox host: it keeps guest configs in a folder and
// answers the commands the tester runs.
type fake struct {
	t        *testing.T
	dir      string
	running  map[int]bool
	calls    []string
	failOn   string // a command prefix that fails
	initDead bool   // pct exec never answers
}

func newFake(t *testing.T) *fake {
	dir := t.TempDir()
	for _, d := range []string{"lxc", "qemu-server"} {
		os.MkdirAll(filepath.Join(dir, "nodes", "pve", d), 0o755)
	}
	f := &fake{t: t, dir: dir, running: map[int]bool{}}
	f.write("lxc", 150, ctConf)
	return f
}

func (f *fake) write(d string, id int, text string) {
	os.WriteFile(filepath.Join(f.dir, "nodes", "pve", d, strconv.Itoa(id)+".conf"), []byte(text), 0o644)
}

func (f *fake) exists(d string, id int) bool {
	_, err := os.Stat(filepath.Join(f.dir, "nodes", "pve", d, strconv.Itoa(id)+".conf"))
	return err == nil
}

func (f *fake) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		return nil, errors.New("boom")
	}
	j := func(v any) ([]byte, error) { b, _ := json.Marshal(v); return b, nil }
	switch {
	case line == "pvesh get /nodes/pve/storage --content backup --output-format json":
		return j([]map[string]any{{"storage": "tower-backup", "active": 1}})
	case strings.HasPrefix(line, "pvesh get /nodes/pve/storage/tower-backup/content"):
		return j([]map[string]any{
			{"volid": "tower-backup:backup/vzdump-lxc-150-2026_11_12-02_30_00.tar.zst", "vmid": 150, "ctime": 1762911000, "size": 300 << 20, "subtype": "lxc"},
			{"volid": "tower-backup:backup/vzdump-lxc-150-2026_11_13-02_30_00.tar.zst", "vmid": 150, "ctime": 1762997400, "size": 310 << 20, "subtype": "lxc"},
			{"volid": "tower-backup:backup/vzdump-lxc-777-2026_11_13-02_30_00.tar.zst", "vmid": 777, "ctime": 1762997400, "size": 1 << 20, "subtype": "lxc"}, // guest gone
		})
	case strings.HasPrefix(line, "pvesh get /nodes/pve/storage/local-lvm/status"):
		return j(map[string]any{"type": "lvmthin", "avail": int64(28 << 30), "total": int64(140 << 30)})
	case strings.HasPrefix(line, "pvesh get /cluster/nextid --vmid "):
		id, _ := strconv.Atoi(args[3])
		if f.exists("lxc", id) || f.exists("qemu-server", id) {
			return nil, errors.New("already exists")
		}
		return []byte(`"` + args[3] + `"`), nil
	case strings.HasPrefix(line, "pct restore "):
		id, _ := strconv.Atoi(args[1])
		f.write("lxc", id, strings.Replace(ctConf, "rootfs: local-lvm:vm-150-disk-0", "rootfs: local-lvm:vm-"+args[1]+"-disk-0", 1))
		return nil, nil
	case strings.HasPrefix(line, "pct start "):
		id, _ := strconv.Atoi(args[1])
		f.running[id] = true
		return nil, nil
	case strings.HasPrefix(line, "pct exec "):
		if f.initDead {
			return nil, errors.New("not running")
		}
		return []byte("systemd\n"), nil
	case strings.HasPrefix(line, "pvesh get /nodes/pve/lxc/"):
		id, _ := strconv.Atoi(strings.Split(args[1], "/")[4])
		st := "stopped"
		if f.running[id] {
			st = "running"
		}
		return j(map[string]string{"status": st})
	case strings.HasPrefix(line, "pct stop "):
		id, _ := strconv.Atoi(args[1])
		f.running[id] = false
		return nil, nil
	case strings.HasPrefix(line, "pct destroy "):
		id, _ := strconv.Atoi(args[1])
		os.Remove(filepath.Join(f.dir, "nodes", "pve", "lxc", strconv.Itoa(id)+".conf"))
		return nil, nil
	}
	f.t.Fatalf("unexpected command: %s", line)
	return nil, nil
}

func (f *fake) tester() *Tester {
	return &Tester{
		Run: f.run, Node: "pve", ConfDir: f.dir, DataDir: f.t.TempDir(),
		Poll: time.Millisecond, Wait: 20 * time.Millisecond,
		MemAvailable: func() (int64, error) { return 8 << 30, nil },
	}
}

func TestCandidatesNewestBackupOfExistingGuests(t *testing.T) {
	f := newFake(t)
	cands, err := f.tester().Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].VMID != 150 || !strings.Contains(cands[0].Archive, "2026_11_13") || cands[0].Name != "cloudflared" {
		t.Fatalf("got %+v", cands)
	}
}

func TestFullTestIsolatesAndDeletesTheCopy(t *testing.T) {
	f := newFake(t)
	tr := f.tester()
	var seenConf string
	run := f.run
	tr.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "pct" && args[0] == "start" {
			raw, _ := os.ReadFile(filepath.Join(f.dir, "nodes", "pve", "lxc", args[1]+".conf"))
			seenConf = string(raw)
		}
		return run(ctx, name, args...)
	}
	r, ok, err := tr.RunOnce(context.Background())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if r.Result != discovery.RestoreOK || r.Proof != "init" {
		t.Fatalf("result %+v", r)
	}
	// What the copy looked like when it was started.
	for _, bad := range []string{"net0", "/mnt/data", "dev0", "lxc.mount.entry", "onboot: 1", "protection", "hostname: cloudflared", "memory: 4096"} {
		if strings.Contains(seenConf, bad) {
			t.Errorf("copy still has %q:\n%s", bad, seenConf)
		}
	}
	for _, want := range []string{"tags: " + Tag, "hostname: nestward-test-9900", "onboot: 0", "memory: 2048", "mp1: local-lvm", "unprivileged: 1"} {
		if !strings.Contains(seenConf, want) {
			t.Errorf("copy lacks %q:\n%s", want, seenConf)
		}
	}
	if f.exists("lxc", 9900) {
		t.Error("the copy was not deleted")
	}
	if !f.exists("lxc", 150) {
		t.Fatal("the real guest is gone!")
	}
	for _, c := range f.calls {
		if strings.Contains(c, " 150") && (strings.HasPrefix(c, "pct stop") || strings.HasPrefix(c, "pct destroy") || strings.HasPrefix(c, "pct start")) {
			t.Fatalf("touched the real guest: %s", c)
		}
	}
	// Tested now, so not due again.
	if _, due := tr.Due([]Candidate{{VMID: 150}}); due {
		t.Error("due right after a test")
	}
	if got := tr.Results()[150]; got.Result != discovery.RestoreOK {
		t.Errorf("result not kept: %+v", got)
	}
}

func TestCheckFailureStillDeletesCopy(t *testing.T) {
	f := newFake(t)
	f.initDead = true
	r, _, _ := f.tester().RunOnce(context.Background())
	if r.Result != discovery.RestoreFailed || r.Stage != "check" {
		t.Fatalf("got %+v", r)
	}
	if f.exists("lxc", 9900) {
		t.Error("copy left behind")
	}
}

func TestRestoreFailureCleansHalfDoneCopy(t *testing.T) {
	f := newFake(t)
	tr := f.tester()
	run := f.run
	tr.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "pct" && args[0] == "restore" {
			// Proxmox made the guest but the restore broke half-way: it has
			// the original name and no tag of ours.
			f.write("lxc", 9900, ctConf)
			return nil, errors.New("unexpected end of archive")
		}
		return run(ctx, name, args...)
	}
	r, _, _ := tr.RunOnce(context.Background())
	if r.Result != discovery.RestoreFailed || r.Stage != "restore" {
		t.Fatalf("got %+v", r)
	}
	if f.exists("lxc", 9900) {
		t.Error("half-done copy left behind")
	}
}

func TestNeverTouchesUnmarkedGuests(t *testing.T) {
	f := newFake(t)
	f.write("lxc", 9901, ctConf) // someone's own guest in our range
	tr := f.tester()
	tr.CleanLeftovers(context.Background())
	if err := tr.cleanup(context.Background(), "pve", "ct", 9901); err == nil {
		t.Error("cleanup accepted a guest without our marks")
	}
	if err := tr.cleanup(context.Background(), "pve", "ct", 150); err == nil {
		t.Error("cleanup accepted a real guest")
	}
	if !f.exists("lxc", 9901) || !f.exists("lxc", 150) {
		t.Fatal("deleted a guest that is not ours")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pct destroy") || strings.HasPrefix(c, "pct stop") {
			t.Fatalf("ran %s", c)
		}
	}
}

func TestLeftoverFromCutShortRunIsDeleted(t *testing.T) {
	f := newFake(t)
	f.write("lxc", 9900, "hostname: nestward-test-9900\ntags: "+Tag+"\nrootfs: local-lvm:vm-9900-disk-0,size=2G\n")
	f.running[9900] = true
	f.tester().CleanLeftovers(context.Background())
	if f.exists("lxc", 9900) {
		t.Error("leftover copy not deleted")
	}
}

func TestSkipsBigBackupsAndFullDisks(t *testing.T) {
	f := newFake(t)
	tr := f.tester()
	tr.MaxArchive = 100 << 20
	r, _, _ := tr.RunOnce(context.Background())
	if r.Result != discovery.RestoreSkipped || r.Stage != "size" {
		t.Fatalf("size: %+v", r)
	}

	f2 := newFake(t)
	tr2 := f2.tester()
	run := f2.run
	tr2.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "pvesh" && strings.HasSuffix(args[1], "/status") {
			b, _ := json.Marshal(map[string]any{"type": "lvmthin", "avail": int64(1 << 30), "total": int64(140 << 30)})
			return b, nil
		}
		return run(ctx, name, args...)
	}
	r, _, _ = tr2.RunOnce(context.Background())
	if r.Result != discovery.RestoreSkipped || r.Stage != "space" {
		t.Fatalf("space: %+v", r)
	}
	for _, c := range f2.calls {
		if strings.HasPrefix(c, "pct restore") {
			t.Fatal("restored without room")
		}
	}
}

func TestLowMemoryRestoresWithoutStarting(t *testing.T) {
	f := newFake(t)
	tr := f.tester()
	tr.MemAvailable = func() (int64, error) { return 1 << 30, nil }
	r, _, _ := tr.RunOnce(context.Background())
	if r.Result != discovery.RestoreOK || r.Proof != "restored" {
		t.Fatalf("got %+v", r)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "pct start") {
			t.Fatal("started with too little memory")
		}
	}
	if f.exists("lxc", 9900) {
		t.Error("copy left behind")
	}
}

func TestDuePicksLongestWaiting(t *testing.T) {
	now := time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC)
	tr := &Tester{Now: func() time.Time { return now }, state: &state{Results: map[int]discovery.RestoreTest{
		100: {TestedAt: now.AddDate(0, 0, -40), Result: "ok"},
		101: {TestedAt: now.AddDate(0, 0, -30), Result: "ok"},
		102: {TestedAt: now.AddDate(0, 0, -3), Result: "ok"},
		104: {TestedAt: now.AddDate(0, 0, -8), Result: "failed"},
	}}}
	c, ok := tr.Due([]Candidate{{VMID: 101}, {VMID: 100}, {VMID: 102}})
	if !ok || c.VMID != 100 {
		t.Fatalf("got %v %v", c.VMID, ok)
	}
	if c, _ := tr.Due([]Candidate{{VMID: 100}, {VMID: 103}}); c.VMID != 103 {
		t.Errorf("never tested should go first, got %d", c.VMID)
	}
	if c, ok := tr.Due([]Candidate{{VMID: 102}, {VMID: 104}}); !ok || c.VMID != 104 {
		t.Errorf("a failed test is retried after a week, got %d %v", c.VMID, ok)
	}
	if _, ok := tr.Due([]Candidate{{VMID: 102}}); ok {
		t.Error("recently tested is not due")
	}
}

func TestVMConfigIsolated(t *testing.T) {
	f := newFake(t)
	f.write("qemu-server", 9902, `name: ollama
memory: 16384
balloon: 2048
net0: virtio=BC:24:11:00:00:01,bridge=vmbr0
hostpci0: 0000:01:00,pcie=1
usb0: host=1-2
args: -cpu host
agent: 1
onboot: 1
scsi0: local-lvm:vm-104-disk-0,size=64G
`)
	if err := f.tester().isolate("pve", "vm", 9902, 4096); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.dir, "nodes", "pve", "qemu-server", "9902.conf"))
	s := string(raw)
	for _, bad := range []string{"net0", "hostpci0", "usb0", "args", "16384", "onboot: 1", "name: ollama", "balloon: 2048"} {
		if strings.Contains(s, bad) {
			t.Errorf("VM copy still has %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"name: nestward-test-9902", "memory: 4096", "balloon: 0", "agent: 1", "scsi0: local-lvm"} {
		if !strings.Contains(s, want) {
			t.Errorf("VM copy lacks %q:\n%s", want, s)
		}
	}
}

func TestConfHelpers(t *testing.T) {
	c := conf{"boot": "order=scsi0;ide2;net0", "scsi0": "local-lvm:vm-1-disk-0,size=32G", "ide2": "local:iso/x.iso,media=cdrom",
		"efidisk0": "local-lvm:vm-1-disk-1,size=4M"}
	if c.rootStorage("vm") != "local-lvm" {
		t.Error(c.rootStorage("vm"))
	}
	if got := c.diskBytes(); got != 32<<30+4<<20 {
		t.Error(got)
	}
}

func TestSecondsAreKept(t *testing.T) {
	f := newFake(t)
	tr := f.tester()
	now := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	tr.Now = func() time.Time { now = now.Add(2 * time.Second); return now }
	r, _, _ := tr.RunOnce(context.Background())
	if r.Seconds <= 0 || tr.Results()[150].Seconds != r.Seconds {
		t.Fatalf("seconds lost: %+v", r)
	}
}
