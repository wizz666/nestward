// Package analysis compares a machine's hardware with each app profile
// and writes the answer in plain language: can run, runs slowly, or
// cannot run, with a suggestion.
package analysis

import (
	"slices"
	"strconv"
	"time"

	"github.com/wizz666/nestward/internal/catalog"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
)

// Verdict is the answer for one app.
type Verdict string

const (
	OK   Verdict = "ok"   // klarar
	Slow Verdict = "slow" // klarar långsamt
	No   Verdict = "no"   // klarar inte
	// Unknown is used when the memory could not be read, so no honest
	// answer is possible.
	Unknown Verdict = "unknown"
)

func (v Verdict) rank() int {
	switch v {
	// Unknown outranks Slow: without the memory size a "runs slowly"
	// would be a guess. A hard "no" is still certain.
	case No:
		return 3
	case Unknown:
		return 2
	case Slow:
		return 1
	}
	return 0
}

// tolerance lets "8 GB" machines count as 8 GB even though the kernel
// reports a little less (firmware and graphics reserve some).
const tolerance = 0.9

// Reason is one fact behind a verdict. Key and Params are language
// neutral; Text is the translated sentence.
type Reason struct {
	Key    string            `json:"key"`
	Params map[string]string `json:"params,omitempty"`
	Effect Verdict           `json:"effect"` // what this reason does to the verdict
	Text   string            `json:"text"`
}

// AppResult is the answer for one app on one machine.
type AppResult struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Verdict     Verdict  `json:"verdict"`
	VerdictText string   `json:"verdict_text"`
	Reasons     []Reason `json:"reasons"`
	Note        string   `json:"note"`
	Suggestion  string   `json:"suggestion,omitempty"`
	ElsewhereOn string   `json:"elsewhere_on,omitempty"` // another machine that runs it well
}

// Facts are the machine facts in plain language, for display.
type Facts struct {
	Title          string `json:"title"`
	System         string `json:"system"`
	CPU            string `json:"cpu"`
	Memory         string `json:"memory"`
	Graphics       string `json:"graphics"`
	Storage        string `json:"storage"`
	Virtualization string `json:"virtualization"`
}

// Report is the full analysis of one machine.
type Report struct {
	Lang        string          `json:"lang"`
	GeneratedAt time.Time       `json:"generated_at"`
	Machine     Facts           `json:"machine"`
	Summary     string          `json:"summary"`
	Apps        []AppResult     `json:"apps"`
	Hardware    hardware.Report `json:"hardware"`
}

// Analyze runs every profile against hw.
func Analyze(hw hardware.Report, profiles []catalog.Profile, t i18n.T) Report {
	r := Report{
		Lang:        t.Lang,
		GeneratedAt: time.Now().UTC(),
		Machine:     describe(hw, t),
		Hardware:    hw,
		Apps:        []AppResult{},
	}
	for _, p := range profiles {
		r.Apps = append(r.Apps, Evaluate(hw, p, t))
	}
	name := hw.Model
	if name == "" {
		name = hw.Hostname
	}
	r.Summary = summary(r.Machine.Title, name, r.Apps, t)
	return r
}

type evaluator struct {
	t       i18n.T
	verdict Verdict
	reasons []Reason
}

func (e *evaluator) add(effect Verdict, key string, kv ...string) {
	params := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		params[kv[i]] = kv[i+1]
	}
	e.reasons = append(e.reasons, Reason{Key: key, Params: params, Effect: effect, Text: e.t.Get(key, kv...)})
	if effect.rank() > e.verdict.rank() {
		e.verdict = effect
	}
}

func hasGB(bytes uint64, gb float64) bool {
	return float64(bytes) >= gb*hardware.GiB*tolerance
}

// suitableGPU returns the best GPU that meets the profile's needs.
func suitableGPU(hw hardware.Report, req catalog.Requirements) (hardware.GPU, bool) {
	for _, g := range hw.GPUs {
		if len(req.GPUVendors) > 0 && !slices.Contains(req.GPUVendors, g.Vendor) {
			continue
		}
		if req.GPUVRAMGB > 0 {
			if g.Integrated || !hasGB(g.VRAMBytes, req.GPUVRAMGB) {
				continue
			}
		}
		return g, true
	}
	return hardware.GPU{}, false
}

// Evaluate answers one profile on one machine.
func Evaluate(hw hardware.Report, p catalog.Profile, t i18n.T) AppResult {
	req := p.Requires
	e := &evaluator{t: t, verdict: OK}
	app := p.Name

	// Processor type.
	if len(req.Arch) > 0 && !slices.Contains(req.Arch, hw.CPU.Arch) {
		e.add(No, "reason.arch", "app", app, "arch", archName(hw.CPU.Arch, t))
	}

	gpu, gpuOK := suitableGPU(hw, req)

	// Memory.
	total := hw.Memory.TotalBytes
	switch {
	case total == 0:
		e.add(Unknown, "reason.ram_unknown")
	case !hasGB(total, req.RAMMinGB):
		e.add(No, "reason.ram_too_little", "app", app, "have", t.GB(total), "need", t.Num(req.RAMMinGB)+" GB")
	case req.RAMRecommendedGB > 0 && !hasGB(total, req.RAMRecommendedGB) && !(req.GPUReplacesRAM && gpuOK):
		e.add(Slow, "reason.ram_below_recommended", "have", t.GB(total), "want", t.Num(req.RAMRecommendedGB)+" GB")
	case req.RAMUseGB > 0 && hw.Memory.AvailableBytes > 0 && float64(hw.Memory.AvailableBytes) < req.RAMUseGB*hardware.GiB:
		e.add(Slow, "reason.ram_busy_now", "free", t.GB(hw.Memory.AvailableBytes), "need", t.Num(req.RAMUseGB)+" GB")
	}

	// Processor cores.
	cores := hw.CPU.Cores
	switch {
	case req.CoresMin > 0 && cores < req.CoresMin:
		e.add(No, "reason.cores_too_few", "have", strconv.Itoa(cores), "need", strconv.Itoa(req.CoresMin))
	case req.CoresRecommended > 0 && cores < req.CoresRecommended:
		e.add(Slow, "reason.cores_below_recommended", "have", strconv.Itoa(cores), "want", strconv.Itoa(req.CoresRecommended))
	}

	// AVX2 only matters on x86 processors.
	if hw.CPU.Arch == "amd64" && hw.CPU.FlagsKnown && !hw.CPU.AVX2 && !(gpuOK && req.GPU != catalog.LevelNone) {
		switch req.AVX2 {
		case catalog.LevelRequired:
			e.add(No, "reason.avx2_required", "app", app)
		case catalog.LevelRecommended:
			e.add(Slow, "reason.avx2_recommended")
		}
	}

	// Graphics.
	switch {
	case req.GPU == catalog.LevelNone:
	case gpuOK:
		e.add(OK, "reason.gpu_ok", "gpu", gpuLabel(gpu, t))
	case req.GPU == catalog.LevelRequired:
		e.add(No, "reason.gpu_required", "app", app, "need", gpuNeed(req, t))
	case req.GPU == catalog.LevelRecommended:
		e.add(Slow, "reason.gpu_recommended", "need", gpuNeed(req, t))
	case req.GPU == catalog.LevelHelps:
		e.add(OK, "reason.gpu_helps")
	}

	// Disk space.
	if free, known := hw.MaxFreeBytes(); !known {
		e.add(OK, "reason.disk_unknown")
	} else if req.DiskFreeGB > 0 && !hasGB(free, req.DiskFreeGB) {
		e.add(No, "reason.disk_full", "free", t.GB(free), "need", t.Num(req.DiskFreeGB)+" GB")
	}

	// Disk type.
	if req.SSD == catalog.LevelRecommended && len(hw.Disks) > 0 {
		fast, sd := false, false
		for _, d := range hw.Disks {
			switch d.Kind {
			case hardware.DiskSSD, hardware.DiskNVMe, hardware.DiskEMMC:
				fast = true
			case hardware.DiskSDCard:
				sd = true
			}
		}
		switch {
		case fast:
		case sd:
			e.add(Slow, "reason.sdcard")
		default:
			e.add(Slow, "reason.no_ssd")
		}
	}

	// Virtualization.
	if v := hw.Virtualization; v.Known && !v.Supported {
		switch req.Virtualization {
		case catalog.LevelRequired:
			e.add(No, "reason.virt_required", "app", app)
		case catalog.LevelPreferred:
			e.add(OK, "reason.virt_preferred_missing", "app", app)
		}
	}

	if e.verdict == OK {
		e.reasons = append([]Reason{{Key: "reason.all_good", Effect: OK, Text: t.Get("reason.all_good")}}, e.reasons...)
	}

	text := p.TextFor(t.Lang)
	res := AppResult{
		ID:          p.ID,
		Name:        p.Name,
		Category:    p.Category,
		Verdict:     e.verdict,
		VerdictText: t.Get("verdict." + string(e.verdict)),
		Reasons:     e.reasons,
		Note:        text.Note,
	}
	if e.verdict == Slow || e.verdict == No {
		res.Suggestion = text.Suggestion
	}
	return res
}

func archName(a string, t i18n.T) string {
	if t.Has("arch." + a) {
		return t.Get("arch." + a)
	}
	return a
}

func gpuLabel(g hardware.GPU, t i18n.T) string {
	if g.VRAMBytes > 0 && !g.Integrated {
		return t.Get("gpu.with_vram", "name", g.Name, "vram", t.GB(g.VRAMBytes))
	}
	return g.Name
}

func gpuNeed(req catalog.Requirements, t i18n.T) string {
	if req.GPUVRAMGB > 0 {
		return t.Get("gpu.need_vram", "vram", t.Num(req.GPUVRAMGB)+" GB")
	}
	return t.Get("gpu.need_any")
}

// describe turns the hardware into short plain-language facts.
func describe(hw hardware.Report, t i18n.T) Facts {
	f := Facts{}
	ram := t.Get("facts.unknown")
	if hw.Memory.TotalBytes > 0 {
		ram = t.GB(hw.Memory.TotalBytes)
	}
	name := hw.Model
	if name == "" {
		name = hw.Hostname
	}
	f.Title = t.Get("facts.title", "name", name, "ram", ram)

	f.System = hw.OS.Name
	if hw.OS.Version != "" {
		f.System += " " + hw.OS.Version
	}
	if hw.Virtualization.InsideVM {
		f.System += " · " + t.Get("facts.inside_vm")
	}

	model := hw.CPU.Model
	if model == "" {
		model = archName(hw.CPU.Arch, t)
	}
	f.CPU = t.Get("facts.cpu", "model", model, "cores", strconv.Itoa(hw.CPU.Cores))
	if hw.CPU.Arch == "amd64" && hw.CPU.FlagsKnown {
		if hw.CPU.AVX2 {
			f.CPU += " · " + t.Get("facts.avx2_yes")
		} else {
			f.CPU += " · " + t.Get("facts.avx2_no")
		}
	}

	if hw.Memory.TotalBytes > 0 {
		f.Memory = t.Get("facts.memory", "total", t.GB(hw.Memory.TotalBytes), "free", t.GB(hw.Memory.AvailableBytes))
	} else {
		f.Memory = t.Get("facts.unknown")
	}

	if len(hw.GPUs) == 0 {
		f.Graphics = t.Get("facts.no_gpu")
	} else {
		var gs []string
		for _, g := range hw.GPUs {
			s := gpuLabel(g, t)
			if g.Integrated {
				s += " (" + t.Get("facts.integrated") + ")"
			}
			gs = append(gs, s)
		}
		f.Graphics = t.List(gs)
	}

	free, known := hw.MaxFreeBytes()
	var kinds []string
	seen := map[string]bool{}
	for _, d := range hw.Disks {
		k := t.Get("disk." + d.Kind)
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	switch {
	case len(kinds) > 0 && known:
		f.Storage = t.Get("facts.storage", "kinds", t.List(kinds), "free", t.GB(free))
	case known:
		f.Storage = t.Get("facts.storage_free", "free", t.GB(free))
	case len(kinds) > 0:
		f.Storage = t.List(kinds)
	default:
		f.Storage = t.Get("facts.unknown")
	}

	switch v := hw.Virtualization; {
	case !v.Known:
		f.Virtualization = t.Get("facts.virt_unknown")
	case v.Supported:
		f.Virtualization = t.Get("facts.virt_yes")
	default:
		f.Virtualization = t.Get("facts.virt_no")
	}
	return f
}

// summary writes one paragraph like "This machine can run A and B. C
// runs slowly. D cannot run here."
func summary(title, name string, apps []AppResult, t i18n.T) string {
	var ok, slow, no, unknown []string
	for _, a := range apps {
		switch a.Verdict {
		case OK:
			ok = append(ok, a.Name)
		case Slow:
			slow = append(slow, a.Name)
		case No:
			no = append(no, a.Name)
		case Unknown:
			unknown = append(unknown, a.Name)
		}
	}
	if len(unknown) == len(apps) && len(apps) > 0 {
		return t.Get("summary.unknown", "machine", name)
	}
	s := ""
	if len(ok) > 0 {
		s = t.Get("summary.ok", "machine", title, "apps", t.List(ok))
	} else {
		s = t.Get("summary.none_ok", "machine", title)
	}
	if len(slow) > 0 {
		s += " " + t.Get("summary.slow", "apps", t.List(slow))
	}
	if len(no) > 0 {
		s += " " + t.Get("summary.no", "apps", t.List(no))
	}
	if len(unknown) > 0 {
		s += " " + t.Get("summary.some_unknown", "apps", t.List(unknown))
	}
	return s
}

// PlaceElsewhere fills ElsewhereOn for apps that do not run well on a
// machine but run fine on another known machine. Reports must all be in
// the same language.
func PlaceElsewhere(reports []Report, t i18n.T) {
	for i := range reports {
		for j := range reports[i].Apps {
			a := &reports[i].Apps[j]
			if a.Verdict == OK {
				continue
			}
			for k := range reports {
				if k == i {
					continue
				}
				if other := findApp(reports[k].Apps, a.ID); other != nil && other.Verdict == OK {
					name := reports[k].Hardware.Hostname
					a.ElsewhereOn = name
					a.Suggestion = t.Get("suggestion.elsewhere", "app", a.Name, "machine", name)
					break
				}
			}
		}
	}
}

func findApp(apps []AppResult, id string) *AppResult {
	for i := range apps {
		if apps[i].ID == id {
			return &apps[i]
		}
	}
	return nil
}
