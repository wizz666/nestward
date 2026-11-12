// Package hardware reads what a machine has: CPU, memory, graphics,
// disks, virtualization support and which system it runs.
package hardware

import "time"

// OS kinds Nestward knows how to recognise.
const (
	OSUnraid   = "unraid"
	OSProxmox  = "proxmox"
	OSTrueNAS  = "truenas"
	OSSynology = "synology"
	OSHAOS     = "haos"
	OSRaspiOS  = "raspios"
	OSLinux    = "linux"
	OSWindows  = "windows"
	OSMacOS    = "macos"
	OSUnknown  = "unknown"
)

// GPU vendors.
const (
	VendorNvidia = "nvidia"
	VendorAMD    = "amd"
	VendorIntel  = "intel"
	VendorOther  = "other"
)

// Disk kinds.
const (
	DiskSSD     = "ssd"
	DiskNVMe    = "nvme"
	DiskHDD     = "hdd"
	DiskSDCard  = "sdcard"
	DiskEMMC    = "emmc" // built-in flash (mini PCs, HA Green)
	DiskUSB     = "usb"  // USB stick or USB disk, e.g. Unraid's boot stick
	DiskUnknown = "unknown"
)

// Report is everything the agent found on one machine.
type Report struct {
	Hostname       string       `json:"hostname"`
	MachineID      string       `json:"machine_id"`
	Model          string       `json:"model,omitempty"` // board/product, e.g. "Raspberry Pi 5 Model B"
	CollectedAt    time.Time    `json:"collected_at"`
	OS             OSInfo       `json:"os"`
	CPU            CPUInfo      `json:"cpu"`
	Memory         MemoryInfo   `json:"memory"`
	GPUs           []GPU        `json:"gpus"`
	Disks          []Disk       `json:"disks"`
	Filesystems    []Filesystem `json:"filesystems"`
	Virtualization Virt         `json:"virtualization"`
	InContainer    bool         `json:"in_container"`
	Notes          []string     `json:"notes,omitempty"` // technical detection notes
}

// OSInfo describes the operating system.
type OSInfo struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
}

// CPUInfo describes the processor.
type CPUInfo struct {
	Arch       string `json:"arch"` // Go names: amd64, arm64, arm, 386
	Model      string `json:"model,omitempty"`
	Cores      int    `json:"cores"` // logical cores (threads)
	AVX2       bool   `json:"avx2"`
	FlagsKnown bool   `json:"flags_known"` // false if cpu flags could not be read
}

// MemoryInfo is RAM in bytes.
type MemoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// GPU is one graphics device.
type GPU struct {
	Vendor      string `json:"vendor"`
	Name        string `json:"name"`
	VRAMBytes   uint64 `json:"vram_bytes"` // 0 = unknown or shared
	Integrated  bool   `json:"integrated"`
	VideoDecode bool   `json:"video_decode"` // a render node exists for hardware video
}

// Disk is one physical block device.
type Disk struct {
	Name      string `json:"name"`
	Model     string `json:"model,omitempty"`
	Kind      string `json:"kind"`
	SizeBytes uint64 `json:"size_bytes"`
	Removable bool   `json:"removable"`
}

// Filesystem is one mounted filesystem with space figures.
type Filesystem struct {
	Mount      string `json:"mount"`
	Type       string `json:"type"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// Virt says whether the machine can run virtual machines.
type Virt struct {
	Known     bool   `json:"known"`
	Supported bool   `json:"supported"`
	Method    string `json:"method,omitempty"` // vmx, svm, kvm
	InsideVM  bool   `json:"inside_vm"`
}

// GiB is 1024^3 bytes.
const GiB = 1 << 30

// MaxFreeBytes returns the largest free space on any filesystem and
// whether any filesystem was found at all.
func (r Report) MaxFreeBytes() (uint64, bool) {
	var best uint64
	for _, f := range r.Filesystems {
		if f.FreeBytes > best {
			best = f.FreeBytes
		}
	}
	return best, len(r.Filesystems) > 0
}
