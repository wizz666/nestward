package catalog

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wizz666/nestward/profiles"
)

func TestBundledProfilesLoad(t *testing.T) {
	ps, err := Load(profiles.FS)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"home-assistant", "immich", "ollama-3b", "ollama-8b", "jellyfin", "paperless-ngx", "vaultwarden"}
	have := map[string]bool{}
	for _, p := range ps {
		have[p.ID] = true
	}
	for _, id := range want {
		if !have[id] {
			t.Errorf("profile %s missing", id)
		}
	}
}

func TestTextFallsBackToEnglish(t *testing.T) {
	p := Profile{Text: map[string]Text{"en": {Note: "hello", Suggestion: "s"}}}
	if p.TextFor("de").Note != "hello" {
		t.Error("fallback to English expected")
	}
}

const good = `id: demo
name: Demo
category: test
requires:
  ram_min_gb: 1
text:
  en: {note: n, suggestion: s}
  sv: {note: n, suggestion: s}
`

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown field":   strings.Replace(good, "category: test", "category: test\nbogus: 1", 1),
		"bad level":       strings.Replace(good, "ram_min_gb: 1", "ram_min_gb: 1\n  gpu: maybe", 1),
		"zero ram":        strings.Replace(good, "ram_min_gb: 1", "ram_min_gb: 0", 1),
		"missing swedish": strings.Replace(good, "  sv: {note: n, suggestion: s}\n", "", 1),
		"bad arch":        strings.Replace(good, "ram_min_gb: 1", "ram_min_gb: 1\n  arch: [sparc]", 1),
		"rec below min":   strings.Replace(good, "ram_min_gb: 1", "ram_min_gb: 4\n  ram_recommended_gb: 2", 1),
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fstest.MapFS{"demo.yaml": {Data: []byte(y)}}); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := Load(fstest.MapFS{"demo.yaml": {Data: []byte(good)}}); err != nil {
		t.Errorf("good profile rejected: %v", err)
	}
	if _, err := Load(fstest.MapFS{"other.yaml": {Data: []byte(good)}}); err == nil {
		t.Error("id must match file name")
	}
}
