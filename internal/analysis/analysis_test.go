package analysis

import (
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/catalog"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/profiles"
)

const gib = hardware.GiB

func gb(f float64) uint64 { return uint64(f * gib) }

func setup(t *testing.T) ([]catalog.Profile, *i18n.Bundle) {
	t.Helper()
	p, err := catalog.Load(profiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	return p, i18n.MustLoad()
}

// Machines that match the test plan in the spec.
func pi5() hardware.Report {
	return hardware.Report{
		Hostname: "pi5", MachineID: "pi5", Model: "Raspberry Pi 5 Model B Rev 1.0",
		OS:             hardware.OSInfo{Kind: hardware.OSRaspiOS, Name: "Raspberry Pi OS", Version: "12"},
		CPU:            hardware.CPUInfo{Arch: "arm64", Model: "Raspberry Pi 5", Cores: 4, FlagsKnown: true},
		Memory:         hardware.MemoryInfo{TotalBytes: gb(7.86), AvailableBytes: 6 * gib},
		Disks:          []hardware.Disk{{Name: "mmcblk0", Kind: hardware.DiskSDCard, SizeBytes: 64 * gib}},
		Filesystems:    []hardware.Filesystem{{Mount: "/", Type: "ext4", TotalBytes: 58 * gib, FreeBytes: 40 * gib}},
		Virtualization: hardware.Virt{Known: false},
	}
}

func desktopWithGPU() hardware.Report {
	return hardware.Report{
		Hostname: "stationar", MachineID: "desk",
		OS:             hardware.OSInfo{Kind: hardware.OSLinux, Name: "Ubuntu 24.04"},
		CPU:            hardware.CPUInfo{Arch: "amd64", Model: "Ryzen 7 5800X", Cores: 16, AVX2: true, FlagsKnown: true},
		Memory:         hardware.MemoryInfo{TotalBytes: gb(31.3), AvailableBytes: 24 * gib},
		GPUs:           []hardware.GPU{{Vendor: hardware.VendorNvidia, Name: "RTX 3070", VRAMBytes: 8 * gib}},
		Disks:          []hardware.Disk{{Name: "nvme0n1", Kind: hardware.DiskNVMe}},
		Filesystems:    []hardware.Filesystem{{Mount: "/", FreeBytes: 500 * gib, TotalBytes: 1000 * gib}},
		Virtualization: hardware.Virt{Known: true, Supported: true, Method: "svm"},
	}
}

func tinyVPS() hardware.Report {
	return hardware.Report{
		Hostname: "vps", MachineID: "vps",
		OS:             hardware.OSInfo{Kind: hardware.OSLinux, Name: "Ubuntu 22.04"},
		CPU:            hardware.CPUInfo{Arch: "amd64", Model: "QEMU Virtual CPU", Cores: 1, AVX2: false, FlagsKnown: true},
		Memory:         hardware.MemoryInfo{TotalBytes: gb(0.95), AvailableBytes: gb(0.5)},
		Disks:          []hardware.Disk{{Name: "vda", Kind: hardware.DiskHDD}},
		Filesystems:    []hardware.Filesystem{{Mount: "/", FreeBytes: 12 * gib, TotalBytes: 20 * gib}},
		Virtualization: hardware.Virt{Known: true, Supported: false, InsideVM: true},
	}
}

func verdicts(r Report) map[string]Verdict {
	m := map[string]Verdict{}
	for _, a := range r.Apps {
		m[a.ID] = a.Verdict
	}
	return m
}

func expect(t *testing.T, got map[string]Verdict, want map[string]Verdict) {
	t.Helper()
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s: got %q, want %q", id, got[id], v)
		}
	}
}

func TestPi5(t *testing.T) {
	profs, b := setup(t)
	r := Analyze(pi5(), profs, b.For("en"))
	expect(t, verdicts(r), map[string]Verdict{
		"vaultwarden":    OK,
		"home-assistant": Slow, // runs from an SD card
		"immich":         Slow,
		"ollama-3b":      Slow,
		"ollama-8b":      Slow,
		"jellyfin":       OK,
		"paperless-ngx":  OK,
	})
	if !strings.Contains(r.Summary, "Vaultwarden") || !strings.Contains(r.Machine.Title, "Raspberry Pi 5") {
		t.Errorf("summary = %q title = %q", r.Summary, r.Machine.Title)
	}
}

func TestDesktopWithGPU(t *testing.T) {
	profs, b := setup(t)
	r := Analyze(desktopWithGPU(), profs, b.For("en"))
	for _, a := range r.Apps {
		if a.Verdict != OK {
			t.Errorf("%s: %s %+v", a.ID, a.Verdict, a.Reasons)
		}
		if a.Suggestion != "" {
			t.Errorf("%s: no suggestion expected when it runs well", a.ID)
		}
	}
	var o8 AppResult
	for _, a := range r.Apps {
		if a.ID == "ollama-8b" {
			o8 = a
		}
	}
	found := false
	for _, rs := range o8.Reasons {
		if rs.Key == "reason.gpu_ok" && strings.Contains(rs.Text, "RTX 3070") {
			found = true
		}
	}
	if !found {
		t.Errorf("ollama-8b should mention the GPU: %+v", o8.Reasons)
	}
}

func TestTinyVPS(t *testing.T) {
	profs, b := setup(t)
	r := Analyze(tinyVPS(), profs, b.For("sv"))
	expect(t, verdicts(r), map[string]Verdict{
		"vaultwarden":    OK,
		"home-assistant": Slow,
		"immich":         No,
		"ollama-3b":      No,
		"ollama-8b":      No,
		"paperless-ngx":  No,
	})
	for _, a := range r.Apps {
		if a.Verdict != OK && a.Suggestion == "" {
			t.Errorf("%s: suggestion missing", a.ID)
		}
		if a.VerdictText == "" || strings.HasPrefix(a.VerdictText, "verdict.") {
			t.Errorf("%s: verdict text not translated: %q", a.ID, a.VerdictText)
		}
		for _, rs := range a.Reasons {
			if strings.Contains(rs.Text, "{") || strings.HasPrefix(rs.Text, "reason.") {
				t.Errorf("%s: untranslated reason %q", a.ID, rs.Text)
			}
		}
	}
	if !strings.Contains(r.Summary, "klarar den inte") {
		t.Errorf("summary = %q", r.Summary)
	}
	if !strings.Contains(r.Machine.Memory, "0,9") && !strings.Contains(r.Machine.Memory, "1 GB") {
		t.Errorf("swedish decimal comma expected: %q", r.Machine.Memory)
	}
}

func TestToleranceCountsEightGBAsEight(t *testing.T) {
	if !hasGB(gb(7.6), 8) {
		t.Error("7.6 GiB should count as an 8 GB machine")
	}
	if hasGB(gb(6), 8) {
		t.Error("6 GiB must not count as 8 GB")
	}
}

func TestArchMismatch(t *testing.T) {
	_, b := setup(t)
	hw := pi5()
	hw.CPU.Arch = "arm"
	p := catalog.Profile{ID: "x", Name: "X", Requires: catalog.Requirements{Arch: []string{"amd64"}, RAMMinGB: 0.1,
		AVX2: "none", GPU: "none", SSD: "none", Virtualization: "none"}}
	if got := Evaluate(hw, p, b.For("en")); got.Verdict != No {
		t.Errorf("verdict = %s", got.Verdict)
	}
}

func TestRequiredVirtualization(t *testing.T) {
	_, b := setup(t)
	p := catalog.Profile{ID: "x", Name: "X", Requires: catalog.Requirements{RAMMinGB: 0.1,
		AVX2: "none", GPU: "none", SSD: "none", Virtualization: "required"}}
	if got := Evaluate(tinyVPS(), p, b.For("en")); got.Verdict != No {
		t.Errorf("no virtualization: verdict = %s", got.Verdict)
	}
	if got := Evaluate(pi5(), p, b.For("en")); got.Verdict != OK {
		t.Errorf("unknown virtualization should not block: verdict = %s", got.Verdict)
	}
}

func TestDiskFull(t *testing.T) {
	profs, b := setup(t)
	hw := desktopWithGPU()
	hw.Filesystems = []hardware.Filesystem{{Mount: "/", FreeBytes: 2 * gib, TotalBytes: 100 * gib}}
	expect(t, verdicts(Analyze(hw, profs, b.For("en"))), map[string]Verdict{"immich": No, "vaultwarden": OK})
}

func TestRAMBusyNow(t *testing.T) {
	profs, b := setup(t)
	hw := desktopWithGPU()
	hw.Memory.AvailableBytes = 1 * gib
	expect(t, verdicts(Analyze(hw, profs, b.For("en"))), map[string]Verdict{"immich": Slow, "vaultwarden": OK})
}

func TestPlaceElsewhere(t *testing.T) {
	profs, b := setup(t)
	tr := b.For("sv")
	reports := []Report{Analyze(pi5(), profs, tr), Analyze(desktopWithGPU(), profs, tr)}
	PlaceElsewhere(reports, tr)
	for _, a := range reports[0].Apps {
		if a.ID == "ollama-8b" {
			if a.ElsewhereOn != "stationar" || !strings.Contains(a.Suggestion, "stationar") {
				t.Errorf("ollama-8b on pi: %+v", a)
			}
		}
		if a.ID == "vaultwarden" && a.ElsewhereOn != "" {
			t.Errorf("vaultwarden runs fine on the Pi, no move suggested")
		}
	}
}

func TestEveryReasonKeyIsTranslated(t *testing.T) {
	profs, b := setup(t)
	machines := []hardware.Report{pi5(), desktopWithGPU(), tinyVPS(), {}}
	for _, lang := range []string{"en", "sv"} {
		tr := b.For(lang)
		for _, hw := range machines {
			for _, a := range Analyze(hw, profs, tr).Apps {
				for _, rs := range a.Reasons {
					if _, ok := b.Keys(lang)[rs.Key]; !ok {
						t.Errorf("%s: key %s missing", lang, rs.Key)
					}
				}
			}
		}
	}
}

// When memory cannot be read, the answer must not claim the app runs well.
func TestUnknownMemoryIsNotOK(t *testing.T) {
	profs, b := setup(t)
	hw := desktopWithGPU()
	hw.Memory = hardware.MemoryInfo{}
	r := Analyze(hw, profs, b.For("sv"))
	for _, a := range r.Apps {
		if a.Verdict != Unknown {
			t.Errorf("%s: verdict %s, want unknown", a.ID, a.Verdict)
		}
		if a.VerdictText != "Okänt" {
			t.Errorf("%s: verdict text %q", a.ID, a.VerdictText)
		}
	}
	if !strings.Contains(r.Summary, "kan inte säga") {
		t.Errorf("summary = %q", r.Summary)
	}
	// A hard limit still wins over missing memory.
	hw.Filesystems = []hardware.Filesystem{{Mount: "/", FreeBytes: 2 * gib, TotalBytes: 10 * gib}}
	expect(t, verdicts(Analyze(hw, profs, b.For("en"))), map[string]Verdict{"immich": No})
}
