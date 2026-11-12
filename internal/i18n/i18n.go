// Package i18n holds every text the program shows, one JSON file per
// language. Add a language by adding a file in locales/.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// Default is used when a requested language is missing.
const Default = "en"

// Bundle holds all loaded languages.
type Bundle struct {
	langs map[string]map[string]string
}

// Load reads the embedded locale files.
func Load() (*Bundle, error) { return LoadFS(localeFS, "locales") }

// LoadFS reads every <lang>.json in dir.
func LoadFS(fsys fs.FS, dir string) (*Bundle, error) {
	files, err := fs.Glob(fsys, dir+"/*.json")
	if err != nil {
		return nil, err
	}
	b := &Bundle{langs: map[string]map[string]string{}}
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		lang := strings.TrimSuffix(f[strings.LastIndex(f, "/")+1:], ".json")
		b.langs[lang] = m
	}
	if _, ok := b.langs[Default]; !ok {
		return nil, fmt.Errorf("i18n: %s.json missing", Default)
	}
	return b, nil
}

// MustLoad is Load that panics; the files are embedded, so failure is a
// build mistake caught by tests.
func MustLoad() *Bundle {
	b, err := Load()
	if err != nil {
		panic(err)
	}
	return b
}

// Languages returns the loaded language codes, sorted.
func (b *Bundle) Languages() []string {
	var out []string
	for l := range b.langs {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Has reports whether lang is loaded.
func (b *Bundle) Has(lang string) bool { _, ok := b.langs[lang]; return ok }

// Keys returns every key of lang.
func (b *Bundle) Keys(lang string) map[string]string { return b.langs[lang] }

// Match picks the best loaded language from a list like an
// Accept-Language header ("sv-SE,sv;q=0.9,en;q=0.8").
func (b *Bundle) Match(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		// Accepts "sv-SE" (browsers) and "sv_SE.UTF-8" (the LANG variable).
		f := strings.FieldsFunc(tag, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
		if len(f) == 0 {
			continue
		}
		if base := strings.ToLower(f[0]); b.Has(base) {
			return base
		}
	}
	return Default
}

// T is a translator for one language.
type T struct {
	Lang string
	b    *Bundle
}

// For returns a translator for lang, or the default language.
func (b *Bundle) For(lang string) T {
	if !b.Has(lang) {
		lang = Default
	}
	return T{Lang: lang, b: b}
}

// Get returns the text for key with {name} placeholders filled in.
// Missing keys fall back to English, then to the key itself.
func (t T) Get(key string, args ...string) string {
	s, ok := t.b.langs[t.Lang][key]
	if !ok {
		s, ok = t.b.langs[Default][key]
	}
	if !ok {
		return key
	}
	for i := 0; i+1 < len(args); i += 2 {
		s = strings.ReplaceAll(s, "{"+args[i]+"}", args[i+1])
	}
	return s
}

// Has reports whether key exists in the current or default language.
func (t T) Has(key string) bool {
	if _, ok := t.b.langs[t.Lang][key]; ok {
		return true
	}
	_, ok := t.b.langs[Default][key]
	return ok
}

// GB formats bytes as gigabytes with one decimal, using the decimal
// separator of the language ("7.8 GB" / "7,8 GB").
func (t T) GB(bytes uint64) string {
	v := float64(bytes) / (1 << 30)
	var s string
	if v >= 10 {
		s = fmt.Sprintf("%.0f", v)
	} else {
		s = strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
	}
	if t.Get("format.decimal") == "," {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s + " GB"
}

// Num formats a float for display with the language's decimal mark.
func (t T) Num(v float64) string {
	s := fmt.Sprintf("%g", v)
	if t.Get("format.decimal") == "," {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// List joins items as "a, b and c".
func (t T) List(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + t.Get("format.and") + " " + items[len(items)-1]
}
