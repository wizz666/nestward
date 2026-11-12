package discovery

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Values of Service.Autostart.
const (
	AutostartYes = "yes"
	AutostartNo  = "no"
)

// Boot is what starts by itself when this machine starts again, for
// example after a power cut. Nestward cannot see the BIOS setting that
// switches the machine itself back on; this covers everything after that.
type Boot struct {
	// Unraid is set on Unraid, where the array, Docker and the virtual
	// machines each have their own "start at boot" setting.
	Unraid *UnraidBoot `json:"unraid,omitempty"`
	// VMs are libvirt virtual machines (Unraid, plain Linux with KVM).
	VMs []BootVM `json:"vms,omitempty"`
}

// UnraidBoot holds Unraid's own start settings.
type UnraidBoot struct {
	ArrayAutostart bool `json:"array_autostart"`
	DockerEnabled  bool `json:"docker_enabled"`
	VMsEnabled     bool `json:"vms_enabled"`
}

// BootVM is one libvirt virtual machine.
type BootVM struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	Autostart bool   `json:"autostart"`
}

// ReadBoot looks at the machine's start settings under root (the host's
// file system, "/" or the read-only /host mount) and fills r.Boot. On
// Unraid, containers in Unraid's own autostart list count as starting by
// themselves even when Docker's restart policy is "no".
func ReadBoot(root string, r *Result) {
	if root == "" {
		root = "/"
	}
	path := func(p string) string { return filepath.Join(root, p) }
	b := &Boot{}

	if exists(path("etc/unraid-version")) {
		b.Unraid = &UnraidBoot{
			ArrayAutostart: cfgValue(path("boot/config/disk.cfg"), "startArray") == "yes",
			DockerEnabled:  cfgValue(path("boot/config/docker.cfg"), "DOCKER_ENABLED") == "yes",
			VMsEnabled:     cfgValue(path("boot/config/domain.cfg"), "SERVICE") == "enable",
		}
		auto := unraidAutostart(path("var/lib/docker/unraid-autostart"))
		for i := range r.Services {
			s := &r.Services[i]
			if s.Guest == nil && s.Autostart == AutostartNo && auto[s.Name] {
				s.Autostart = AutostartYes
			}
		}
	}

	b.VMs = libvirtVMs(path("etc/libvirt/qemu"), path("var/run/libvirt/qemu"))
	if b.Unraid != nil || len(b.VMs) > 0 {
		r.Boot = b
	}
}

// NotStarting lists what runs now but will not start by itself: services
// first, then virtual machines. Stopped things are left out on purpose.
func (r *Result) NotStarting() []string {
	var out []string
	for _, s := range r.Services {
		if s.State == "running" && s.Autostart == AutostartNo {
			name := s.AppName
			if name == "" {
				name = s.Name
			}
			out = append(out, name)
		}
	}
	if r.Boot != nil {
		for _, v := range r.Boot.VMs {
			if v.Running && !v.Autostart {
				out = append(out, v.Name)
			}
		}
	}
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// cfgValue reads KEY="value" from an Unraid .cfg file.
func cfgValue(file, key string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// unraidAutostart reads Unraid's list of containers to start with Docker:
// one name per line, sometimes followed by a wait time.
func unraidAutostart(file string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			out[f[0]] = true
		}
	}
	return out
}

// libvirtVMs lists defined virtual machines: one XML file each in conf,
// a link in conf/autostart when it starts with the machine, and a .pid
// file in run while it is running.
func libvirtVMs(conf, run string) []BootVM {
	files, err := filepath.Glob(filepath.Join(conf, "*.xml"))
	if err != nil || len(files) == 0 {
		return nil
	}
	var out []BootVM
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".xml")
		out = append(out, BootVM{
			Name:      name,
			Running:   exists(filepath.Join(run, name+".pid")),
			Autostart: exists(filepath.Join(conf, "autostart", name+".xml")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
