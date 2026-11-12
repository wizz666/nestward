package hardware

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

const x86CPU = `processor	: 0
model name	: Intel(R) Core(TM) i5-10400 CPU @ 2.90GHz
flags		: fpu vme sse sse2 avx avx2 vmx
processor	: 1
model name	: Intel(R) Core(TM) i5-10400 CPU @ 2.90GHz
flags		: fpu vme sse sse2 avx avx2 vmx
`

const piCPU = `processor	: 0
BogoMIPS	: 108.00
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid asimdrdm lrcpc dcpop asimddp
processor	: 1
processor	: 2
processor	: 3
Model		: Raspberry Pi 5 Model B Rev 1.0
`

const meminfo16 = "MemTotal:       16311000 kB\nMemFree:  1000 kB\nMemAvailable:   12000000 kB\n"

func newTestDetector(sys, host fstest.MapFS, arch string) *Detector {
	if host == nil {
		host = sys
	}
	return &Detector{
		Sys: sys, Host: host, HostRoot: "/", GOOS: "linux", GOARCH: arch,
		Statfs: func(p string) (uint64, uint64, error) {
			if p == "/" {
				return 100 * GiB, 40 * GiB, nil
			}
			if p == "/mnt/user" {
				return 8000 * GiB, 3000 * GiB, nil
			}
			return 0, 0, errors.New("no")
		},
		Run:      func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not found") },
		Hostname: func() (string, error) { return "fallback", nil },
		Now:      func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	}
}

func TestRaspberryPi(t *testing.T) {
	sys := fstest.MapFS{
		"proc/cpuinfo":                       file(piCPU),
		"proc/meminfo":                       file("MemTotal: 8245000 kB\nMemAvailable: 6000000 kB\n"),
		"proc/device-tree/model":             file("Raspberry Pi 5 Model B Rev 1.0\x00"),
		"proc/self/mounts":                   file("/dev/mmcblk0p2 / ext4 rw 0 0\n/dev/mmcblk0p1 /boot/firmware vfat rw 0 0\nproc /proc proc rw 0 0\n"),
		"sys/block/mmcblk0/size":             file("62521344"),
		"sys/block/mmcblk0/removable":        file("0"),
		"sys/block/loop0/size":               file("100"),
		"sys/class/drm/card0/dev":            file("226:0"),
		"etc/os-release":                     file("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n"),
		"etc/hostname":                       file("pi5\n"),
		"etc/machine-id":                     file("abc123\n"),
		"sys/block/mmcblk0/queue/rotational": file("0"),
	}
	r := newTestDetector(sys, nil, "arm64").Detect(context.Background())
	if r.OS.Kind != OSRaspiOS {
		t.Errorf("os kind = %q, want raspios", r.OS.Kind)
	}
	if r.CPU.Cores != 4 || r.CPU.Model != "Raspberry Pi 5 Model B Rev 1.0" || r.CPU.AVX2 {
		t.Errorf("cpu = %+v", r.CPU)
	}
	if r.Virtualization.Known {
		t.Errorf("virtualization should be unknown on arm without /dev/kvm: %+v", r.Virtualization)
	}
	if len(r.Disks) != 1 || r.Disks[0].Kind != DiskSDCard {
		t.Errorf("disks = %+v", r.Disks)
	}
	if len(r.GPUs) != 0 {
		t.Errorf("Pi display block should not count as a GPU: %+v", r.GPUs)
	}
	if len(r.Filesystems) != 1 || r.Filesystems[0].Mount != "/" {
		t.Errorf("filesystems = %+v (boot must be skipped)", r.Filesystems)
	}
	if r.Hostname != "pi5" || r.MachineID != "abc123" {
		t.Errorf("host = %q id = %q", r.Hostname, r.MachineID)
	}
	if r.Memory.TotalBytes != 8245000*1024 {
		t.Errorf("memory = %d", r.Memory.TotalBytes)
	}
}

func TestUnraidWithNvidia(t *testing.T) {
	sys := fstest.MapFS{
		"proc/cpuinfo":                        file(x86CPU),
		"proc/meminfo":                        file(meminfo16),
		"proc/self/mounts":                    file("rootfs / rootfs rw 0 0\nshfs /mnt/user fuse.shfs rw 0 0\n/dev/md1p1 /mnt/disk1 xfs rw 0 0\n"),
		"sys/block/sda/size":                  file("1953525168"),
		"sys/block/sda/queue/rotational":      file("1"),
		"sys/block/sda/device/model":          file("WDC WD10EZEX\n"),
		"sys/block/nvme0n1/size":              file("976773168"),
		"sys/block/nvme0n1/queue/rotational":  file("0"),
		"sys/block/md1p1/size":                file("1953525168"),
		"sys/class/drm/card0/device/vendor":   file("0x10de\n"),
		"sys/class/drm/card0-HDMI-A-1/status": file("connected"),
		"sys/class/drm/renderD128/dev":        file("226:128"),
		"sys/class/drm/card1/device/vendor":   file("0x8086\n"),
		"dev/kvm":                             file(""),
		"etc/unraid-version":                  file("version=\"6.12.10\"\n"),
		"etc/HOSTNAME":                        file("Tower.local\n"),
		"etc/os-release":                      file("NAME=Slackware\nID=slackware\n"),
	}
	d := newTestDetector(sys, nil, "amd64")
	d.Run = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "nvidia-smi" {
			return nil, errors.New("unexpected")
		}
		return []byte("NVIDIA GeForce RTX 3060, 12288\n"), nil
	}
	r := d.Detect(context.Background())
	if r.Hostname != "Tower" {
		t.Errorf("hostname = %q, want Tower (from /etc/HOSTNAME)", r.Hostname)
	}
	if r.OS.Kind != OSUnraid || r.OS.Version != "6.12.10" {
		t.Errorf("os = %+v", r.OS)
	}
	if !r.CPU.AVX2 || r.CPU.Cores != 2 || !r.CPU.FlagsKnown {
		t.Errorf("cpu = %+v", r.CPU)
	}
	if !r.Virtualization.Supported || r.Virtualization.Method != "vmx" {
		t.Errorf("virt = %+v", r.Virtualization)
	}
	if len(r.GPUs) != 2 {
		t.Fatalf("gpus = %+v", r.GPUs)
	}
	var nv, intel *GPU
	for i := range r.GPUs {
		switch r.GPUs[i].Vendor {
		case VendorNvidia:
			nv = &r.GPUs[i]
		case VendorIntel:
			intel = &r.GPUs[i]
		}
	}
	if nv == nil || nv.Name != "NVIDIA GeForce RTX 3060" || nv.VRAMBytes != 12288<<20 {
		t.Errorf("nvidia = %+v", nv)
	}
	if intel == nil || !intel.Integrated || !intel.VideoDecode {
		t.Errorf("intel = %+v", intel)
	}
	kinds := map[string]string{}
	for _, dk := range r.Disks {
		kinds[dk.Name] = dk.Kind
	}
	if kinds["sda"] != DiskHDD || kinds["nvme0n1"] != DiskNVMe || len(kinds) != 2 {
		t.Errorf("disks = %+v (md devices must be skipped)", r.Disks)
	}
	free, ok := r.MaxFreeBytes()
	if !ok || free != 3000*GiB {
		t.Errorf("max free = %d, %v", free, ok)
	}
}

func TestOSKinds(t *testing.T) {
	cases := []struct {
		name string
		host fstest.MapFS
		want string
	}{
		{"proxmox", fstest.MapFS{"usr/bin/pveversion": file(""), "etc/os-release": file("ID=debian\n")}, OSProxmox},
		{"truenas", fstest.MapFS{"usr/bin/midclt": file(""), "etc/version": file("24.10.0\n")}, OSTrueNAS},
		{"synology", fstest.MapFS{"etc.defaults/VERSION": file("productversion=\"7.2.1\"\n")}, OSSynology},
		{"haos", fstest.MapFS{"etc/os-release": file("NAME=\"Home Assistant OS\"\nID=haos\nVERSION_ID=13.2\n")}, OSHAOS},
		{"raspbian", fstest.MapFS{"etc/os-release": file("ID=raspbian\nVERSION_ID=\"11\"\n")}, OSRaspiOS},
		{"ubuntu", fstest.MapFS{"etc/os-release": file("PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n")}, OSLinux},
		{"empty", fstest.MapFS{}, OSLinux},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newTestDetector(fstest.MapFS{}, c.host, "amd64").Detect(context.Background())
			if r.OS.Kind != c.want {
				t.Errorf("kind = %q, want %q (%+v)", r.OS.Kind, c.want, r.OS)
			}
		})
	}
}

func TestSynologyVersion(t *testing.T) {
	r := newTestDetector(fstest.MapFS{}, fstest.MapFS{"etc.defaults/VERSION": file("productversion=\"7.2.1\"\n")}, "amd64").Detect(context.Background())
	if r.OS.Version != "7.2.1" {
		t.Errorf("version = %q", r.OS.Version)
	}
}

func TestContainerMountsFiltered(t *testing.T) {
	sys := fstest.MapFS{
		"proc/self/mounts": file("overlay / overlay rw 0 0\n/dev/sda1 /host ext4 ro 0 0\n/dev/sdb1 /host/srv xfs ro 0 0\n/dev/sda1 /data ext4 rw 0 0\n"),
		".dockerenv":       file(""),
	}
	d := newTestDetector(sys, fstest.MapFS{}, "amd64")
	d.HostRoot = "/host"
	d.Statfs = func(p string) (uint64, uint64, error) { return 10 * GiB, 5 * GiB, nil }
	r := d.Detect(context.Background())
	if !r.InContainer {
		t.Error("should detect container")
	}
	if len(r.Filesystems) != 2 || r.Filesystems[0].Mount != "/" || r.Filesystems[1].Mount != "/srv" {
		t.Errorf("filesystems = %+v", r.Filesystems)
	}
}

func TestNoCPUFlagsInsideVM(t *testing.T) {
	sys := fstest.MapFS{"proc/cpuinfo": file("processor : 0\nmodel name : QEMU Virtual CPU\nflags : fpu sse2 hypervisor\n")}
	r := newTestDetector(sys, nil, "amd64").Detect(context.Background())
	if !r.Virtualization.Known || r.Virtualization.Supported || !r.Virtualization.InsideVM {
		t.Errorf("virt = %+v", r.Virtualization)
	}
	if r.CPU.AVX2 {
		t.Error("no avx2 expected")
	}
}

func TestAMDCard(t *testing.T) {
	sys := fstest.MapFS{
		"sys/class/drm/card0/device/vendor":              file("0x1002"),
		"sys/class/drm/card0/device/mem_info_vram_total": file("17163091968"),
		"sys/class/drm/card1/device/vendor":              file("0x1002"),
		"sys/class/drm/card1/device/mem_info_vram_total": file("536870912"),
	}
	r := newTestDetector(sys, nil, "amd64").Detect(context.Background())
	if len(r.GPUs) != 2 || r.GPUs[0].Integrated || !r.GPUs[1].Integrated || r.GPUs[0].VRAMBytes != 17163091968 {
		t.Errorf("gpus = %+v", r.GPUs)
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	g := parseNvidiaSMI([]byte("NVIDIA GeForce GTX 1080, 8192\nbad line\nTesla T4, 15360\n"))
	if len(g) != 2 || g[1].Name != "Tesla T4" || g[1].VRAMBytes != 15360<<20 {
		t.Errorf("got %+v", g)
	}
}

func TestDetectEmptyNeverPanics(t *testing.T) {
	r := newTestDetector(fstest.MapFS{}, nil, "riscv64").Detect(context.Background())
	if r.CPU.Cores == 0 || r.Hostname != "fallback" || r.MachineID != "fallback" {
		t.Errorf("fallbacks not used: %+v", r)
	}
	if len(r.Notes) == 0 {
		t.Error("expected detection notes")
	}
}

func link(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// Real layout seen on a small Unraid NAS:
// sysfs block entries are symlinks, the boot stick is USB, the eMMC has
// boot partitions, and docker.img/libvirt.img are loop mounts.
func TestUnraidNASLayout(t *testing.T) {
	sys := fstest.MapFS{
		"sys/block/sda":          link("../devices/pci0000:00/0000:00:14.0/usb1/1-7/host2/block/sda"),
		"sys/block/mmcblk1":      link("../devices/pci0000:00/0000:00:1a.0/mmc_host/mmc1/mmc1:0001/block/mmcblk1"),
		"sys/block/mmcblk1boot0": link("../devices/pci0000:00/0000:00:1a.0/mmc_host/mmc1/mmc1:0001/block/mmcblk1/mmcblk1boot0"),
		"sys/block/sdb":          link("../devices/pci0000:00/0000:00:17.0/ata1/host0/block/sdb"),

		"sys/devices/pci0000:00/0000:00:14.0/usb1/1-7/host2/block/sda/size":                           file("7843840"),
		"sys/devices/pci0000:00/0000:00:14.0/usb1/1-7/host2/block/sda/queue/rotational":               file("1"),
		"sys/devices/pci0000:00/0000:00:14.0/usb1/1-7/host2/block/sda/removable":                      file("1"),
		"sys/devices/pci0000:00/0000:00:1a.0/mmc_host/mmc1/mmc1:0001/block/mmcblk1/size":              file("241827840"),
		"sys/devices/pci0000:00/0000:00:1a.0/mmc_host/mmc1/mmc1:0001/block/mmcblk1/device/type":       file("MMC\n"),
		"sys/devices/pci0000:00/0000:00:1a.0/mmc_host/mmc1/mmc1:0001/block/mmcblk1/mmcblk1boot0/size": file("8192"),
		"sys/devices/pci0000:00/0000:00:17.0/ata1/host0/block/sdb/size":                               file("1953525168"),
		"sys/devices/pci0000:00/0000:00:17.0/ata1/host0/block/sdb/queue/rotational":                   file("0"),

		"proc/self/mounts": file("/dev/loop0 /usr squashfs ro 0 0\n" +
			"shfs /mnt/user0 fuse.shfs rw 0 0\nshfs /mnt/user fuse.shfs rw 0 0\n" +
			"/dev/loop2 /var/lib/docker btrfs rw 0 0\n/dev/loop3 /etc/libvirt btrfs rw 0 0\n"),
	}
	r := newTestDetector(sys, fstest.MapFS{}, "amd64").Detect(context.Background())
	kinds := map[string]string{}
	for _, d := range r.Disks {
		kinds[d.Name] = d.Kind
	}
	want := map[string]string{"sda": DiskUSB, "mmcblk1": DiskEMMC, "sdb": DiskSSD}
	if len(kinds) != len(want) {
		t.Errorf("disks = %v, want %v", kinds, want)
	}
	for n, k := range want {
		if kinds[n] != k {
			t.Errorf("%s: kind %q, want %q", n, kinds[n], k)
		}
	}
	if len(r.Filesystems) != 1 || r.Filesystems[0].Mount != "/mnt/user" {
		t.Errorf("filesystems = %+v, want only /mnt/user", r.Filesystems)
	}
}
