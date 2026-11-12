// Package discovery finds the services already running on a machine and
// recognises which apps they are.
package discovery

import (
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

// Roles of a recognised container.
const (
	RoleApp     = "app"
	RoleSupport = "support"
)

// Signature describes how to recognise one app.
type Signature struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Category string   `yaml:"category"`
	Role     string   `yaml:"role"`
	Images   []string `yaml:"images"`
	Prefixes []string `yaml:"prefixes"`
	Aliases  []string `yaml:"aliases"`
	Port     int      `yaml:"port"`
	Path     string   `yaml:"path"`
	Scheme   string   `yaml:"scheme"`
	Login    string   `yaml:"login"`
	Profile  string   `yaml:"profile"`
}

// Catalog matches images to signatures.
type Catalog struct {
	sigs    []Signature
	byImage map[string]int
	byAlias map[string]int
}

var knownCategories = map[string]bool{
	"media": true, "downloads": true, "photos": true, "smart-home": true, "documents": true,
	"files": true, "security": true, "network": true, "monitoring": true, "dashboard": true,
	"management": true, "ai": true, "development": true, "web": true, "finance": true,
	"communication": true, "backup": true, "database": true,
}

// Categories returns every category a signature may use.
func Categories() []string {
	var out []string
	for c := range knownCategories {
		out = append(out, c)
	}
	return out
}

// LoadCatalog reads every *.yaml in fsys (a list under "apps:").
func LoadCatalog(fsys fs.FS) (*Catalog, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	c := &Catalog{byImage: map[string]int{}, byAlias: map[string]int{}}
	ids := map[string]bool{}
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Apps []Signature `yaml:"apps"`
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, s := range doc.Apps {
			if err := s.validate(); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", f, s.ID, err)
			}
			if ids[s.ID] {
				return nil, fmt.Errorf("%s: id %q used twice", f, s.ID)
			}
			ids[s.ID] = true
			if s.Role == "" {
				s.Role = RoleApp
			}
			if s.Scheme == "" {
				s.Scheme = "http"
			}
			i := len(c.sigs)
			c.sigs = append(c.sigs, s)
			for _, img := range s.Images {
				n := NormalizeImage(img)
				if prev, dup := c.byImage[n]; dup {
					return nil, fmt.Errorf("%s: image %q in both %s and %s", f, img, c.sigs[prev].ID, s.ID)
				}
				c.byImage[n] = i
			}
			for _, a := range append([]string{s.ID}, s.Aliases...) {
				if _, dup := c.byAlias[a]; !dup {
					c.byAlias[a] = i
				}
			}
		}
	}
	return c, nil
}

func (s Signature) validate() error {
	if s.ID == "" || s.Name == "" {
		return fmt.Errorf("id and name are required")
	}
	if !knownCategories[s.Category] {
		return fmt.Errorf("unknown category %q", s.Category)
	}
	if s.Role != "" && s.Role != RoleApp && s.Role != RoleSupport {
		return fmt.Errorf("unknown role %q", s.Role)
	}
	if len(s.Images) == 0 && len(s.Prefixes) == 0 {
		return fmt.Errorf("needs images or prefixes")
	}
	if s.Login != "" && s.Login != "own" && s.Login != "none" {
		return fmt.Errorf("login must be own or none")
	}
	if s.Scheme != "" && s.Scheme != "http" && s.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	return nil
}

// Len returns the number of signatures.
func (c *Catalog) Len() int { return len(c.sigs) }

// NormalizeImage turns an image reference into a plain repository name:
// "lscr.io/linuxserver/plex:latest" -> "linuxserver/plex",
// "redis:6.2-alpine" -> "redis", "docker.io/library/nginx" -> "nginx".
func NormalizeImage(ref string) string {
	s := strings.ToLower(strings.TrimSpace(ref))
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s = s[:i]
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		parts = parts[1:]
	}
	if len(parts) > 1 && parts[0] == "library" {
		parts = parts[1:]
	}
	return strings.Join(parts, "/")
}

// baseName is the last part of a repository without common decorations.
func baseName(repo string) string {
	b := repo[strings.LastIndex(repo, "/")+1:]
	for _, p := range []string{"arch-", "docker-"} {
		b = strings.TrimPrefix(b, p)
	}
	return b
}

// Match finds the signature for an image. title is the image's
// org.opencontainers.image.title label, used as a last hint.
func (c *Catalog) Match(image, title string) (Signature, bool) {
	repo := NormalizeImage(image)
	if i, ok := c.byImage[repo]; ok {
		return c.sigs[i], true
	}
	for _, s := range c.sigs {
		for _, p := range s.Prefixes {
			if strings.HasPrefix(repo, p) {
				return s, true
			}
		}
	}
	if i, ok := c.byAlias[baseName(repo)]; ok {
		return c.sigs[i], true
	}
	if t := strings.ToLower(strings.TrimSpace(title)); t != "" {
		if i, ok := c.byAlias[strings.ReplaceAll(t, " ", "-")]; ok {
			return c.sigs[i], true
		}
	}
	return Signature{}, false
}
