package hardware

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Detector collects a Report. All inputs are injectable so the logic
// can be tested with fake filesystems.
type Detector struct {
	// Sys is rooted at "/" of the running kernel and is used for
	// proc, sys and dev. Inside a container these still describe the
	// real hardware.
	Sys fs.FS
	// Host is rooted at the host's "/" and is used for etc and other
	// system files. Same as Sys when not running in a container; set to
	// a bind mount (e.g. /host) when running in one.
	Host fs.FS
	// HostRoot is the real path of Host, used to filter mounts. "/" when
	// not in a container.
	HostRoot string
	// GOOS and GOARCH default to the running program's values.
	GOOS, GOARCH string
	// Statfs returns total and free bytes of a real path.
	Statfs func(path string) (total, free uint64, err error)
	// Run runs a helper program (nvidia-smi). May be nil.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Hostname returns the host name if the host files do not say.
	Hostname func() (string, error)
	// Now returns the current time.
	Now func() time.Time
}

// NewDetector returns a Detector for the running machine. hostRoot is
// where the host's root filesystem is mounted ("" or "/" if not in a
// container).
func NewDetector(hostRoot string) *Detector {
	if hostRoot == "" {
		hostRoot = "/"
	}
	return &Detector{
		Sys:      os.DirFS("/"),
		Host:     os.DirFS(hostRoot),
		HostRoot: hostRoot,
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Statfs:   statfs,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		Hostname: os.Hostname,
		Now:      time.Now,
	}
}

// Detect collects the full report. It never fails: anything it cannot
// read is left empty and noted.
func (d *Detector) Detect(ctx context.Context) Report {
	r := Report{
		CollectedAt: d.now().UTC(),
		GPUs:        []GPU{},
		Disks:       []Disk{},
		Filesystems: []Filesystem{},
	}
	r.Hostname = d.hostname()
	r.MachineID = strings.TrimSpace(d.readHost("etc/machine-id"))
	if r.MachineID == "" {
		r.MachineID = r.Hostname
	}
	r.Model = d.model()
	r.InContainer = d.existsSys(".dockerenv") || d.existsSys("run/.containerenv")
	r.CPU, r.Virtualization = d.cpu(&r)
	r.Memory = d.memory(&r)
	r.GPUs = d.gpus(ctx, &r)
	r.Disks = d.disks()
	r.Filesystems = d.filesystems()
	r.OS = d.os(r.Model)
	return r
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Detector) readSys(p string) string { return readFS(d.Sys, p) }
func (d *Detector) readHost(p string) string {
	return readFS(d.Host, p)
}
func (d *Detector) existsSys(p string) bool  { return existsFS(d.Sys, p) }
func (d *Detector) existsHost(p string) bool { return existsFS(d.Host, p) }

func readFS(f fs.FS, p string) string {
	if f == nil {
		return ""
	}
	b, err := fs.ReadFile(f, p)
	if err != nil {
		return ""
	}
	return string(b)
}

func existsFS(f fs.FS, p string) bool {
	if f == nil {
		return false
	}
	_, err := fs.Stat(f, p)
	return err == nil
}

func (d *Detector) hostname() string {
	// Most systems use /etc/hostname; Slackware-based Unraid uses /etc/HOSTNAME.
	for _, p := range []string{"etc/hostname", "etc/HOSTNAME"} {
		if h := strings.TrimSpace(d.readHost(p)); h != "" {
			return strings.SplitN(h, ".", 2)[0]
		}
	}
	if d.Hostname != nil {
		if h, err := d.Hostname(); err == nil {
			return h
		}
	}
	return "unknown"
}

func (d *Detector) model() string {
	m := strings.TrimRight(d.readSys("proc/device-tree/model"), "\x00\n ")
	if m != "" {
		return m
	}
	vendor := strings.TrimSpace(d.readSys("sys/class/dmi/id/sys_vendor"))
	product := strings.TrimSpace(d.readSys("sys/class/dmi/id/product_name"))
	if product == "" || isPlaceholder(product) {
		return ""
	}
	if vendor != "" && !isPlaceholder(vendor) && !strings.HasPrefix(product, vendor) {
		return vendor + " " + product
	}
	return product
}

func isPlaceholder(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "to be filled") || strings.Contains(l, "default string") ||
		strings.Contains(l, "system product name") || l == "none"
}

// cpu reads /proc/cpuinfo for model, core count, AVX2 and virtualization flags.
func (d *Detector) cpu(r *Report) (CPUInfo, Virt) {
	c := CPUInfo{Arch: d.GOARCH}
	v := Virt{}
	info := d.readSys("proc/cpuinfo")
	var flags map[string]bool
	cores := 0
	sc := bufio.NewScanner(strings.NewReader(info))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "processor":
			cores++
		case "model name":
			if c.Model == "" {
				c.Model = val
			}
		case "Model", "Hardware":
			if c.Model == "" {
				c.Model = val
			}
		case "flags", "Features":
			if flags == nil {
				flags = map[string]bool{}
				for _, f := range strings.Fields(val) {
					flags[f] = true
				}
			}
		}
	}
	c.Cores = cores
	if c.Cores == 0 {
		c.Cores = runtime.NumCPU()
	}
	isX86 := d.GOARCH == "amd64" || d.GOARCH == "386"
	if flags != nil {
		c.FlagsKnown = true
		c.AVX2 = flags["avx2"]
		v.InsideVM = flags["hypervisor"]
		if isX86 {
			v.Known = true
			switch {
			case flags["vmx"]:
				v.Supported, v.Method = true, "vmx"
			case flags["svm"]:
				v.Supported, v.Method = true, "svm"
			}
		}
	} else if info == "" {
		r.Notes = append(r.Notes, "cpuinfo not readable")
	}
	if d.existsSys("dev/kvm") {
		v.Known, v.Supported = true, true
		if v.Method == "" {
			v.Method = "kvm"
		}
	}
	return c, v
}

func (d *Detector) memory(r *Report) MemoryInfo {
	m := MemoryInfo{}
	for _, line := range strings.Split(d.readSys("proc/meminfo"), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(val)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.TotalBytes = n * 1024
		case "MemAvailable":
			m.AvailableBytes = n * 1024
		}
	}
	if m.TotalBytes == 0 {
		r.Notes = append(r.Notes, "meminfo not readable")
	}
	return m
}

var pciVendors = map[string]string{
	"0x10de": VendorNvidia,
	"0x1002": VendorAMD,
	"0x8086": VendorIntel,
}

// gpus reads /sys/class/drm for graphics cards, then asks nvidia-smi
// for names and memory of Nvidia cards when it is available.
func (d *Detector) gpus(ctx context.Context, r *Report) []GPU {
	out := []GPU{}
	entries, _ := fs.ReadDir(d.Sys, "sys/class/drm")
	hasRender := false
	var cards []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "renderD") {
			hasRender = true
		}
		if strings.HasPrefix(n, "card") && !strings.Contains(n, "-") {
			cards = append(cards, n)
		}
	}
	sort.Strings(cards)
	for _, n := range cards {
		base := path.Join("sys/class/drm", n, "device")
		vendor, ok := pciVendors[strings.TrimSpace(d.readSys(base+"/vendor"))]
		if !ok {
			continue // not a PCI graphics card (e.g. Raspberry Pi display block)
		}
		g := GPU{Vendor: vendor}
		switch vendor {
		case VendorAMD:
			g.Name = "AMD Radeon"
			if vr, err := strconv.ParseUint(strings.TrimSpace(d.readSys(base+"/mem_info_vram_total")), 10, 64); err == nil {
				g.VRAMBytes = vr
			}
			// APUs report a small carve-out (usually 512 MB or less).
			g.Integrated = g.VRAMBytes > 0 && g.VRAMBytes <= 1*GiB
		case VendorIntel:
			g.Name = "Intel graphics"
			g.Integrated = true
		case VendorNvidia:
			g.Name = "Nvidia"
		}
		out = append(out, g)
	}
	// Nvidia's driver lists its cards in /proc even without /sys/class/drm.
	nv, _ := fs.ReadDir(d.Sys, "proc/driver/nvidia/gpus")
	var nvNames []string
	for _, e := range nv {
		for _, line := range strings.Split(d.readSys(path.Join("proc/driver/nvidia/gpus", e.Name(), "information")), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Model" {
				nvNames = append(nvNames, strings.TrimSpace(v))
			}
		}
	}
	if smi := d.nvidiaSMI(ctx); len(smi) > 0 {
		out = replaceVendor(out, VendorNvidia, smi)
	} else if len(nvNames) > 0 {
		var gs []GPU
		for _, name := range nvNames {
			gs = append(gs, GPU{Vendor: VendorNvidia, Name: name})
		}
		out = replaceVendor(out, VendorNvidia, gs)
		r.Notes = append(r.Notes, "nvidia-smi not available, video memory unknown")
	}
	for i := range out {
		out[i].VideoDecode = hasRender || out[i].Vendor == VendorNvidia
	}
	return out
}

func replaceVendor(list []GPU, vendor string, with []GPU) []GPU {
	out := []GPU{}
	for _, g := range list {
		if g.Vendor != vendor {
			out = append(out, g)
		}
	}
	return append(out, with...)
}

func (d *Detector) nvidiaSMI(ctx context.Context) []GPU {
	if d.Run == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := d.Run(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(b)
}

func parseNvidiaSMI(b []byte) []GPU {
	var out []GPU
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, mem, ok := strings.Cut(sc.Text(), ",")
		if !ok {
			continue
		}
		mib, err := strconv.ParseUint(strings.TrimSpace(mem), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, GPU{Vendor: VendorNvidia, Name: strings.TrimSpace(name), VRAMBytes: mib << 20})
	}
	return out
}

var skipBlock = []string{"loop", "ram", "zram", "sr", "fd", "dm-", "md", "nbd", "mtdblock"}

// disks lists physical block devices from /sys/block.
func (d *Detector) disks() []Disk {
	out := []Disk{}
	entries, _ := fs.ReadDir(d.Sys, "sys/block")
	for _, e := range entries {
		n := e.Name()
		skip := false
		for _, p := range skipBlock {
			if strings.HasPrefix(n, p) {
				skip = true
				break
			}
		}
		// eMMC chips expose small boot and RPMB areas as extra devices.
		if skip || (strings.HasPrefix(n, "mmcblk") && (strings.Contains(n, "boot") || strings.Contains(n, "rpmb"))) {
			continue
		}
		base := path.Join("sys/block", n)
		sectors, _ := strconv.ParseUint(strings.TrimSpace(d.readSys(base+"/size")), 10, 64)
		if sectors == 0 {
			continue
		}
		disk := Disk{
			Name:      n,
			Model:     strings.TrimSpace(d.readSys(base + "/device/model")),
			SizeBytes: sectors * 512,
			Removable: strings.TrimSpace(d.readSys(base+"/removable")) == "1",
		}
		rot := strings.TrimSpace(d.readSys(base + "/queue/rotational"))
		link, _ := fs.ReadLink(d.Sys, base) // e.g. ../devices/pci.../usb1/.../block/sda
		switch {
		case strings.HasPrefix(n, "nvme"):
			disk.Kind = DiskNVMe
		case strings.HasPrefix(n, "mmcblk") && strings.TrimSpace(d.readSys(base+"/device/type")) == "MMC":
			disk.Kind = DiskEMMC
		case strings.HasPrefix(n, "mmcblk"):
			disk.Kind = DiskSDCard
		case strings.Contains(link, "/usb"):
			disk.Kind = DiskUSB
		case rot == "1":
			disk.Kind = DiskHDD
		case rot == "0":
			disk.Kind = DiskSSD
		default:
			disk.Kind = DiskUnknown
		}
		out = append(out, disk)
	}
	return out
}

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"fuse.shfs": true, // Unraid user shares
	"9p":        true, "virtiofs": true, "fuseblk": true,
}

// filesystems lists real mounted filesystems with free space. When the
// host is bind-mounted into a container only mounts below HostRoot are
// used, and the prefix is removed for display.
func (d *Detector) filesystems() []Filesystem {
	out := []Filesystem{}
	if d.Statfs == nil {
		return out
	}
	mounts := d.readSys("proc/self/mounts")
	seen := map[string]bool{}
	root := strings.TrimRight(d.HostRoot, "/")
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		src, mnt, typ := f[0], unescapeMount(f[1]), f[2]
		// Loop devices are image files (Unraid's docker.img, libvirt.img,
		// snaps), not storage the user can put apps on.
		if !realFS[typ] || strings.HasPrefix(src, "/dev/loop") {
			continue
		}
		show := mnt
		if root != "" {
			if mnt != root && !strings.HasPrefix(mnt, root+"/") {
				continue
			}
			show = strings.TrimPrefix(mnt, root)
			if show == "" {
				show = "/"
			}
		}
		// Unraid's /mnt/user and the disks behind it are the same space;
		// keep one entry per source device.
		key := src
		if typ == "fuse.shfs" {
			key = "shfs"
		}
		// /mnt/user0 is the same share without the cache; show /mnt/user.
		if seen[key] || strings.HasPrefix(show, "/boot") || show == "/mnt/user0" {
			continue
		}
		total, free, err := d.Statfs(mnt)
		if err != nil || total == 0 {
			continue
		}
		seen[key] = true
		out = append(out, Filesystem{Mount: show, Type: typ, TotalBytes: total, FreeBytes: free})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// osRelease parses /etc/os-release style files.
func osRelease(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		m[k] = strings.Trim(v, `"'`)
	}
	return m
}

// os works out which system the host runs, checking the special
// systems before falling back to plain Linux.
func (d *Detector) os(model string) OSInfo {
	switch d.GOOS {
	case "windows":
		return OSInfo{Kind: OSWindows, Name: "Windows"}
	case "darwin":
		return OSInfo{Kind: OSMacOS, Name: "macOS"}
	case "linux":
	default:
		return OSInfo{Kind: OSUnknown, Name: d.GOOS}
	}
	kernel := strings.TrimSpace(d.readSys("proc/sys/kernel/osrelease"))
	rel := osRelease(d.readHost("etc/os-release"))
	if len(rel) == 0 {
		rel = osRelease(d.readHost("usr/lib/os-release"))
	}
	info := OSInfo{Kernel: kernel}
	switch {
	case d.existsHost("etc/unraid-version"):
		info.Kind, info.Name = OSUnraid, "Unraid"
		info.Version = osRelease(d.readHost("etc/unraid-version"))["version"]
	case d.existsHost("usr/bin/pveversion") || d.existsHost("etc/pve"):
		info.Kind, info.Name = OSProxmox, "Proxmox VE"
		// The Proxmox version needs pveversion or the API; the agent's
		// Proxmox adapter (later milestone) will fill it in.
	case d.existsHost("usr/bin/midclt") || d.existsHost("data/freenas-v1.db") || strings.Contains(strings.ToLower(rel["NAME"]), "truenas"):
		info.Kind, info.Name = OSTrueNAS, "TrueNAS"
		info.Version = strings.TrimSpace(d.readHost("etc/version"))
	case d.existsHost("etc/synoinfo.conf") || d.existsHost("etc.defaults/VERSION"):
		info.Kind, info.Name = OSSynology, "Synology DSM"
		v := osRelease(d.readHost("etc.defaults/VERSION"))
		info.Version = v["productversion"]
	case rel["ID"] == "haos":
		info.Kind, info.Name = OSHAOS, "Home Assistant OS"
		info.Version = rel["VERSION_ID"]
	case rel["ID"] == "raspbian" || d.existsHost("etc/rpi-issue") ||
		(strings.Contains(model, "Raspberry Pi") && rel["ID"] == "debian"):
		info.Kind, info.Name = OSRaspiOS, "Raspberry Pi OS"
		info.Version = rel["VERSION_ID"]
	default:
		info.Kind = OSLinux
		info.Name = rel["PRETTY_NAME"]
		if info.Name == "" {
			info.Name = "Linux"
		}
		info.Version = rel["VERSION_ID"]
	}
	return info
}
