// Package catalog loads and checks the app profiles.
package catalog

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Levels used in requirements.
const (
	LevelNone        = "none"
	LevelHelps       = "helps"       // GPU only: faster with one, verdict unchanged
	LevelRecommended = "recommended" // missing => "slow"
	LevelPreferred   = "preferred"   // virtualization only: missing => note, verdict unchanged
	LevelRequired    = "required"    // missing => "no"
)

// Profile describes what one app needs.
type Profile struct {
	ID       string          `yaml:"id" json:"id"`
	Name     string          `yaml:"name" json:"name"`
	Category string          `yaml:"category" json:"category"`
	Homepage string          `yaml:"homepage" json:"homepage,omitempty"`
	Requires Requirements    `yaml:"requires" json:"requires"`
	Text     map[string]Text `yaml:"text" json:"-"`
}

// Text is the plain-language text of a profile in one language.
type Text struct {
	Note       string `yaml:"note"`
	Suggestion string `yaml:"suggestion"`
}

// Requirements are the numbers the analysis compares with.
type Requirements struct {
	Arch             []string `yaml:"arch" json:"arch"`
	RAMMinGB         float64  `yaml:"ram_min_gb" json:"ram_min_gb"`
	RAMRecommendedGB float64  `yaml:"ram_recommended_gb" json:"ram_recommended_gb"`
	RAMUseGB         float64  `yaml:"ram_use_gb" json:"ram_use_gb"`
	CoresMin         int      `yaml:"cores_min" json:"cores_min"`
	CoresRecommended int      `yaml:"cores_recommended" json:"cores_recommended"`
	AVX2             string   `yaml:"avx2" json:"avx2"`
	GPU              string   `yaml:"gpu" json:"gpu"`
	GPUVRAMGB        float64  `yaml:"gpu_vram_gb" json:"gpu_vram_gb"`
	GPUVendors       []string `yaml:"gpu_vendors" json:"gpu_vendors"`
	GPUReplacesRAM   bool     `yaml:"gpu_replaces_ram" json:"gpu_replaces_ram"`
	DiskFreeGB       float64  `yaml:"disk_free_gb" json:"disk_free_gb"`
	SSD              string   `yaml:"ssd" json:"ssd"`
	Virtualization   string   `yaml:"virtualization" json:"virtualization"`
}

// TextFor returns the text in lang, falling back to English.
func (p Profile) TextFor(lang string) Text {
	if t, ok := p.Text[lang]; ok && t.Note != "" {
		return t
	}
	return p.Text["en"]
}

// Load reads every *.yaml file in fsys, checks it and returns the
// profiles sorted by name.
func Load(fsys fs.FS) ([]Profile, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	var out []Profile
	ids := map[string]string{}
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		var p Profile
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		p.normalise()
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if want := strings.TrimSuffix(path.Base(f), ".yaml"); p.ID != want {
			return nil, fmt.Errorf("%s: id %q must match the file name", f, p.ID)
		}
		if prev, dup := ids[p.ID]; dup {
			return nil, fmt.Errorf("%s: id %q already used in %s", f, p.ID, prev)
		}
		ids[p.ID] = f
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *Profile) normalise() {
	r := &p.Requires
	for _, l := range []*string{&r.AVX2, &r.GPU, &r.SSD, &r.Virtualization} {
		if *l == "" {
			*l = LevelNone
		}
	}
}

// Validate checks that a profile is complete and uses known values.
func (p Profile) Validate() error {
	if p.ID == "" || p.Name == "" {
		return fmt.Errorf("id and name are required")
	}
	r := p.Requires
	if r.RAMMinGB <= 0 {
		return fmt.Errorf("ram_min_gb must be above 0")
	}
	if r.RAMRecommendedGB != 0 && r.RAMRecommendedGB < r.RAMMinGB {
		return fmt.Errorf("ram_recommended_gb is below ram_min_gb")
	}
	if r.CoresRecommended != 0 && r.CoresRecommended < r.CoresMin {
		return fmt.Errorf("cores_recommended is below cores_min")
	}
	check := func(name, v string, allowed ...string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("%s: %q is not one of %v", name, v, allowed)
	}
	if err := check("avx2", r.AVX2, LevelNone, LevelRecommended, LevelRequired); err != nil {
		return err
	}
	if err := check("gpu", r.GPU, LevelNone, LevelHelps, LevelRecommended, LevelRequired); err != nil {
		return err
	}
	if err := check("ssd", r.SSD, LevelNone, LevelRecommended); err != nil {
		return err
	}
	if err := check("virtualization", r.Virtualization, LevelNone, LevelPreferred, LevelRequired); err != nil {
		return err
	}
	for _, a := range r.Arch {
		if err := check("arch", a, "amd64", "arm64", "arm", "386"); err != nil {
			return err
		}
	}
	for _, v := range r.GPUVendors {
		if err := check("gpu_vendors", v, "nvidia", "amd", "intel"); err != nil {
			return err
		}
	}
	for _, lang := range []string{"en", "sv"} {
		t, ok := p.Text[lang]
		if !ok || t.Note == "" || t.Suggestion == "" {
			return fmt.Errorf("text.%s needs note and suggestion", lang)
		}
	}
	return nil
}
