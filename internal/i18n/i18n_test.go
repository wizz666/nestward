package i18n

import (
	"regexp"
	"testing"
)

// Every language must have exactly the same keys as English, and the
// same {placeholders} in each text.
func TestLanguagesMatchEnglish(t *testing.T) {
	b := MustLoad()
	ph := regexp.MustCompile(`\{[a-z_]+\}`)
	en := b.Keys("en")
	for _, lang := range b.Languages() {
		if lang == "en" {
			continue
		}
		other := b.Keys(lang)
		for k, v := range en {
			ov, ok := other[k]
			if !ok {
				t.Errorf("%s: missing %s", lang, k)
				continue
			}
			if ov == "" {
				t.Errorf("%s: %s is empty", lang, k)
			}
			a, c := ph.FindAllString(v, -1), ph.FindAllString(ov, -1)
			if len(a) != len(c) {
				t.Errorf("%s: %s placeholders %v vs %v", lang, k, a, c)
			}
		}
		for k := range other {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: extra key %s not in English", lang, k)
			}
		}
	}
	if !b.Has("sv") {
		t.Error("Swedish must be bundled")
	}
}

func TestGetAndFormat(t *testing.T) {
	b := MustLoad()
	sv := b.For("sv")
	if got := sv.Get("summary.slow", "apps", "Immich"); got != "Immich fungerar, men långsamt." {
		t.Errorf("got %q", got)
	}
	pi := 7.86
	if got := sv.GB(uint64(pi * (1 << 30))); got != "7,9 GB" {
		t.Errorf("sv GB = %q", got)
	}
	if got := b.For("en").GB(16 << 30); got != "16 GB" {
		t.Errorf("en GB = %q", got)
	}
	if got := b.For("en").GB(2 << 30); got != "2 GB" {
		t.Errorf("en GB = %q", got)
	}
	if got := sv.List([]string{"A", "B", "C"}); got != "A, B och C" {
		t.Errorf("list = %q", got)
	}
	if got := b.For("xx").Lang; got != "en" {
		t.Errorf("unknown language should fall back to en, got %s", got)
	}
	if got := sv.Get("does.not.exist"); got != "does.not.exist" {
		t.Errorf("missing key = %q", got)
	}
}

func TestMatch(t *testing.T) {
	b := MustLoad()
	cases := map[string]string{
		"sv-SE,sv;q=0.9,en;q=0.8": "sv",
		"de-DE,sv;q=0.5":          "sv",
		"sv_SE.UTF-8":             "sv",
		"fr":                      "en",
		"":                        "en",
	}
	for in, want := range cases {
		if got := b.Match(in); got != want {
			t.Errorf("Match(%q) = %s, want %s", in, got, want)
		}
	}
}
