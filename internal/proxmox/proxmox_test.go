package proxmox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/signatures"
)

// Trimmed from real `pvesh get` output on the test machines.
var answers = map[string]string{
	"/cluster/resources": `[{"id":"qemu/103","type":"qemu","node":"pve","vmid":103,"name":"haos16.3","status":"running","maxcpu":2,"maxmem":4294967296,"mem":2410239091,"maxdisk":34359738368,"uptime":5100841,"tags":"community-script","template":0},
		{"id":"qemu/900","type":"qemu","node":"pve","vmid":900,"name":"debian-template","status":"stopped","template":1},
		{"id":"qemu/102","type":"qemu","node":"pve","vmid":102,"name":"3CX","status":"stopped","maxcpu":1,"maxmem":2147483648,"maxdisk":34359738368,"template":0},
		{"id":"lxc/105","type":"lxc","node":"pve","vmid":105,"name":"appar","status":"running","maxcpu":4,"maxmem":4294967296,"mem":1975095296,"maxdisk":31526391808,"disk":13137076224,"uptime":220835,"template":0},
		{"id":"lxc/104","type":"lxc","node":"pve","vmid":104,"name":"ollama","status":"running","maxcpu":4,"maxmem":8589934592,"mem":84926464,"maxdisk":67452583936,"disk":48158195712,"tags":"proxmox-helper-scripts;ai","template":0},
		{"id":"lxc/200","type":"lxc","node":"other","vmid":200,"name":"elsewhere","status":"running","template":0}]`,
	"/nodes/pve/storage": `[{"active":1,"content":"vztmpl,iso,backup","storage":"local","type":"dir"}]`,
	"/nodes/pve/storage/local/content": `[{"content":"backup","ctime":1789110410,"format":"tar.zst","subtype":"lxc","vmid":105,"volid":"local:backup/vzdump-lxc-105-2026_09_11-09_06_50.tar.zst"},
		{"content":"backup","ctime":1789000000,"subtype":"lxc","vmid":105,"volid":"local:backup/old.tar.zst"}]`,
	"/nodes/pve/lxc/105/interfaces": `[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"inet"}]},
		{"name":"docker0","ip-addresses":[{"ip-address":"172.17.0.1","ip-address-type":"inet"}]},
		{"name":"eth0","ip-addresses":[{"ip-address":"192.168.1.188","ip-address-type":"inet"},{"ip-address":"fe80::1","ip-address-type":"inet6"}]}]`,
	"/nodes/pve/qemu/103/agent/network-get-interfaces": `{"result":[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4"}]},
		{"name":"hassio","ip-addresses":[{"ip-address":"172.30.32.1","ip-address-type":"ipv4"}]},
		{"name":"enp0s18","ip-addresses":[{"ip-address":"192.168.1.123","ip-address-type":"ipv4"}]}]}`,
}

func fakeReader(t *testing.T, calls *int) *Reader {
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, "nodes", "pve", p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(s), 0o644)
	}
	write("qemu-server/103.conf", "agent: enabled=1\nonboot: 1\nname: haos16.3\n\n[snap1]\nonboot: 0\n")
	write("qemu-server/102.conf", "onboot: 0\n")
	write("lxc/105.conf", "hostname: appar\nnet0: name=eth0,bridge=vmbr0,ip=dhcp\nonboot: 1\n")
	write("lxc/104.conf", "hostname: ollama\nnet0: name=eth0,bridge=vmbr0,gw=192.168.1.1,ip=192.168.1.150/24\n")
	return &Reader{Node: "pve", ConfDir: dir, Run: func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) < 2 || args[0] != "get" {
			t.Fatalf("only read commands are allowed, got %v", args)
		}
		*calls++
		if a, ok := answers[args[1]]; ok {
			return []byte(a), nil
		}
		return nil, errors.New("no answer for " + args[1])
	}}
}

func TestGuests(t *testing.T) {
	calls := 0
	r := fakeReader(t, &calls)
	first, err := r.Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range first {
		if g.VMID == 105 && g.IP != "" {
			t.Error("the first list must not wait for address lookups")
		}
	}
	r.WaitLookups()
	if calls != 5 {
		t.Errorf("first run made %d calls, want 5 (the list, two backup reads and two addresses)", calls)
	}
	gs, _ := r.Guests(context.Background())
	by := map[int]discovery.Guest{}
	for _, g := range gs {
		by[g.VMID] = g
	}
	if len(gs) != 4 {
		t.Fatalf("templates and other nodes' guests must be left out: %+v", gs)
	}
	if g := by[103]; g.Type != "vm" || !g.OnBoot || g.IP != "192.168.1.123" || g.DiskUsed != 0 || g.MemMax != 4294967296 {
		t.Errorf("vm 103 = %+v", g)
	}
	if g := by[105]; g.Type != "ct" || !g.OnBoot || g.IP != "192.168.1.188" || g.DiskUsed != 13137076224 {
		t.Errorf("ct 105 = %+v", g)
	}
	if g := by[105]; !g.BackupChecked || g.LastBackup == nil || g.LastBackup.Unix() != 1789110410 {
		t.Errorf("ct 105 backup = %v %v, want the newest", g.BackupChecked, g.LastBackup)
	}
	if g := by[103]; !g.BackupChecked || g.LastBackup != nil {
		t.Errorf("vm 103 has no backup: %v %v", g.BackupChecked, g.LastBackup)
	}
	if g := by[104]; g.IP != "192.168.1.150" || g.OnBoot || strings.Join(g.Tags, ",") != "proxmox-helper-scripts,ai" {
		t.Errorf("ct 104 (static address) = %+v", g)
	}
	if g := by[102]; g.Status != "stopped" || g.IP != "" {
		t.Errorf("stopped vm = %+v", g)
	}

	// The list is reused for a minute, addresses for ten.
	before := calls
	r.Guests(context.Background())
	if calls != before {
		t.Errorf("within a minute pvesh must not run again: %d calls", calls-before)
	}
	r.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	before = calls
	r.Guests(context.Background())
	if calls-before != 1 {
		t.Errorf("after a minute only the list is read again: %d calls", calls-before)
	}
	r.Now = func() time.Time { return time.Now().Add(15 * time.Minute) }
	before = calls
	r.Guests(context.Background())
	r.WaitLookups()
	if calls-before != 5 {
		t.Errorf("after 10 minutes the addresses and backups are read again: %d calls", calls-before)
	}
}

func TestGuestsBecomeServices(t *testing.T) {
	cat, err := discovery.LoadCatalog(signatures.FS)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fr := fakeReader(t, &calls)
	fr.Guests(context.Background())
	fr.WaitLookups()
	gs, _ := fr.Guests(context.Background())

	r := discovery.Result{Source: "docker", Error: "docker_not_found", Services: []discovery.Service{}}
	discovery.AddGuests(&r, gs, cat, nil)
	if !r.Available || r.Error != "" || r.Source != "proxmox" || r.Stats.Guests != 4 || r.Stats.GuestsRunning != 3 {
		t.Fatalf("result = %+v", r)
	}
	by := map[string]discovery.Service{}
	for _, s := range r.Services {
		by[s.Name] = s
	}
	if s := by["vm-103"]; s.AppID != "home-assistant" || s.Web == nil || s.Web.Host != "192.168.1.123" || s.Web.Port != 8123 || s.State != "running" {
		t.Errorf("haos = %+v %+v", s, s.Web)
	}
	if s := by["ct-104"]; s.AppID != "ollama" || s.Web == nil || s.Web.Port != 11434 {
		t.Errorf("ollama = %+v", s)
	}
	if s := by["ct-105"]; s.Kind != discovery.KindGuest || s.AppName != "appar" || s.Web != nil {
		t.Errorf("appar = %+v", s)
	}
	if s := by["vm-102"]; s.State != "exited" || s.Kind != discovery.KindGuest {
		t.Errorf("3CX = %+v", s)
	}

	// Docker and Proxmox on one machine: both kept.
	d := discovery.Result{Source: "docker", Available: true, Services: []discovery.Service{{Name: "kuma", State: "running"}}}
	discovery.AddGuests(&d, gs, cat, nil)
	if d.Source != "docker+proxmox" || len(d.Services) != 5 {
		t.Errorf("mixed = %s %d", d.Source, len(d.Services))
	}
	// A Proxmox problem does not hide the Docker list.
	e := discovery.Result{Source: "docker", Available: true, Services: []discovery.Service{{Name: "kuma"}}}
	discovery.AddGuests(&e, nil, cat, errors.New("boom"))
	if !e.Available || e.Error != "" || len(e.Services) != 1 {
		t.Errorf("error case = %+v", e)
	}
}

func TestMatchNameIsStrict(t *testing.T) {
	cat, _ := discovery.LoadCatalog(signatures.FS)
	for name, want := range map[string]string{
		"ollama": "ollama", "haos16.3": "home-assistant", "nextcloudpi": "nextcloud", "cloudflared": "cloudflared",
		"appar": "", "ubuntu": "", "Windows11": "", "rag": "", "kasm": "",
	} {
		sig, _ := cat.MatchName(name)
		if sig.ID != want {
			t.Errorf("%s -> %q, want %q", name, sig.ID, want)
		}
	}
}
