package discovery

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An Unraid machine as seen through the /host mount.
func TestReadBootUnraid(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/unraid-version", `version="7.2.7"`+"\n")
	writeFile(t, root, "boot/config/disk.cfg", "startArray=\"yes\"\nspindownDelay=\"0\"\n")
	writeFile(t, root, "boot/config/docker.cfg", "DOCKER_ENABLED=\"yes\"\n")
	writeFile(t, root, "boot/config/domain.cfg", "SERVICE=\"enable\"\n")
	writeFile(t, root, "var/lib/docker/unraid-autostart", "plex\nvaultwarden 10\n")
	writeFile(t, root, "etc/libvirt/qemu/Home Assistant.xml", "<domain/>")
	// Like on Unraid: an absolute link that cannot be followed through /host.
	if err := os.MkdirAll(filepath.Join(root, "etc/libvirt/qemu/autostart"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/libvirt/qemu/does-not-resolve.xml", filepath.Join(root, "etc/libvirt/qemu/autostart/Home Assistant.xml")); err != nil {
		writeFile(t, root, "etc/libvirt/qemu/autostart/Home Assistant.xml", "<domain/>") // no symlinks here (Windows)
	}
	writeFile(t, root, "etc/libvirt/qemu/test-vm.xml", "<domain/>")
	writeFile(t, root, "run/libvirt/qemu/Home Assistant.pid", "123") // /var/run is a link to /run
	writeFile(t, root, "var/run/libvirt/qemu/test-vm.pid", "456")

	r := Result{Available: true, Services: []Service{
		{Name: "plex", AppName: "Plex", State: "running", Autostart: AutostartNo},               // Unraid list
		{Name: "vaultwarden", AppName: "Vaultwarden", State: "running", Autostart: AutostartNo}, // list with wait time
		{Name: "immich", AppName: "Immich", State: "running", Autostart: AutostartYes},          // unless-stopped
		{Name: "toy", AppName: "Toy", State: "running", Autostart: AutostartNo},                 // will not come back
		{Name: "backup", AppName: "Backup", State: "exited", Autostart: ""},                     // stopped: not judged
	}}
	ReadBoot(root, &r)

	if r.Boot == nil || r.Boot.Unraid == nil {
		t.Fatalf("boot = %+v", r.Boot)
	}
	if u := *r.Boot.Unraid; !u.ArrayAutostart || !u.DockerEnabled || !u.VMsEnabled {
		t.Errorf("unraid = %+v", u)
	}
	want := []BootVM{{Name: "Home Assistant", Running: true, Autostart: true}, {Name: "test-vm", Running: true, Autostart: false}}
	if !reflect.DeepEqual(r.Boot.VMs, want) {
		t.Errorf("vms = %+v", r.Boot.VMs)
	}
	if got := r.NotStarting(); !reflect.DeepEqual(got, []string{"Toy", "test-vm"}) {
		t.Errorf("not starting = %v", got)
	}
}

func TestReadBootArrayManual(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "etc/unraid-version", `version="7.2.7"`)
	writeFile(t, root, "boot/config/disk.cfg", "startArray=\"no\"\n")
	r := Result{Available: true}
	ReadBoot(root, &r)
	if r.Boot == nil || r.Boot.Unraid == nil || r.Boot.Unraid.ArrayAutostart {
		t.Errorf("boot = %+v", r.Boot)
	}
}

// Plain Linux without Unraid or libvirt: nothing to report, and the
// Unraid list is never read.
func TestReadBootPlainLinux(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "var/lib/docker/unraid-autostart", "toy\n")
	r := Result{Available: true, Services: []Service{{Name: "toy", AppName: "Toy", State: "running", Autostart: AutostartNo}}}
	ReadBoot(root, &r)
	if r.Boot != nil {
		t.Errorf("boot = %+v", r.Boot)
	}
	if got := r.NotStarting(); !reflect.DeepEqual(got, []string{"Toy"}) {
		t.Errorf("not starting = %v", got)
	}
}

func TestGuestAutostart(t *testing.T) {
	r := Result{}
	AddGuests(&r, []Guest{
		{Type: "ct", VMID: 100, Name: "tunnel", Status: "running", OnBoot: true},
		{Type: "vm", VMID: 101, Name: "toy", Status: "running", OnBoot: false},
		{Type: "vm", VMID: 102, Name: "old", Status: "stopped", OnBoot: false},
	}, catalog(t), nil)
	got := map[string]string{}
	for _, s := range r.Services {
		got[s.Name] = s.Autostart
	}
	if got["ct-100"] != AutostartYes || got["vm-101"] != AutostartNo || got["vm-102"] != "" {
		t.Errorf("autostart = %v", got)
	}
}

type policyFake struct {
	fakeLister
	policies map[string]string
}

func (p policyFake) RestartPolicy(_ context.Context, id string) (string, error) {
	return p.policies[id], nil
}

func TestDiscoverReadsRestartPolicy(t *testing.T) {
	l := policyFake{
		fakeLister: fakeLister{cs: []Container{
			{ID: "aaa", Names: []string{"/kuma"}, Image: "louislam/uptime-kuma:1", State: "running"},
			{ID: "bbb", Names: []string{"/toy"}, Image: "toy:1", State: "running"},
			{ID: "ccc", Names: []string{"/job"}, Image: "job:1", State: "exited", Status: "Exited (0) 1 hour ago"},
		}},
		policies: map[string]string{"aaa": "unless-stopped", "bbb": "no", "ccc": "no"},
	}
	r := Discover(context.Background(), catalog(t), func() (Lister, error) { return l, nil })
	got := map[string]string{}
	for _, s := range r.Services {
		got[s.Name] = s.Autostart
	}
	if got["kuma"] != AutostartYes || got["toy"] != AutostartNo || got["job"] != "" {
		t.Errorf("autostart = %v", got)
	}
}
