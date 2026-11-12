package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/signatures"
)

func catalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(signatures.FS)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loadFixture(t *testing.T, name string) []Container {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var cs []Container
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func byName(ss []Service) map[string]Service {
	m := map[string]Service{}
	for _, s := range ss {
		m[s.Name] = s
	}
	return m
}

func TestNormalizeImage(t *testing.T) {
	cases := map[string]string{
		"lscr.io/linuxserver/plex":                         "linuxserver/plex",
		"lscr.io/linuxserver/plex:latest":                  "linuxserver/plex",
		"ghcr.io/immich-app/immich-server:release":         "immich-app/immich-server",
		"redis:6.2-alpine":                                 "redis",
		"docker.io/library/nginx:1.27":                     "nginx",
		"localhost:5000/me/app:1":                          "me/app",
		"Vaultwarden/Server@sha256:abcdef":                 "vaultwarden/server",
		"registry.example.com:5000/team/tool:v2":           "team/tool",
		"ghcr.io/nextcloud-releases/aio-postgresql:latest": "nextcloud-releases/aio-postgresql",
	}
	for in, want := range cases {
		if got := NormalizeImage(in); got != want {
			t.Errorf("NormalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogLoads(t *testing.T) {
	c := catalog(t)
	if c.Len() < 80 {
		t.Errorf("only %d signatures", c.Len())
	}
}

func TestCatalogValidation(t *testing.T) {
	bad := map[string]string{
		"unknown field": "apps:\n  - {id: a, name: A, category: media, images: [a/a], bogus: 1}\n",
		"bad category":  "apps:\n  - {id: a, name: A, category: toys, images: [a/a]}\n",
		"no images":     "apps:\n  - {id: a, name: A, category: media}\n",
		"dup id":        "apps:\n  - {id: a, name: A, category: media, images: [a/a]}\n  - {id: a, name: B, category: media, images: [b/b]}\n",
		"dup image":     "apps:\n  - {id: a, name: A, category: media, images: [x/x]}\n  - {id: b, name: B, category: media, images: [x/x]}\n",
		"bad login":     "apps:\n  - {id: a, name: A, category: media, images: [a/a], login: maybe}\n",
	}
	for name, y := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCatalog(fstest.MapFS{"x.yaml": {Data: []byte(y)}}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestMatch(t *testing.T) {
	c := catalog(t)
	cases := map[string]string{
		"lscr.io/linuxserver/plex":                        "plex",
		"binhex/arch-emby":                                "emby",
		"someone/arch-jellyfin":                           "jellyfin", // alias via base name
		"ghcr.io/nextcloud-releases/aio-talk:latest":      "nextcloud-aio-part",
		"ghcr.io/immich-app/postgres:14-vectorchord0.4.3": "postgres",
		"vaultwarden/server":                              "vaultwarden",
		"ghcr.io/home-assistant/home-assistant:stable":    "home-assistant",
		"nginx:alpine":                                    "nginx",
		"ahmadnassri/vscode-server":                       "code-server",
		"nestward:m2-test":                                "nestward", // Nestward sees itself
	}
	for img, want := range cases {
		s, ok := c.Match(img, "")
		if !ok || s.ID != want {
			t.Errorf("Match(%q) = %q, %v; want %q", img, s.ID, ok, want)
		}
	}
	if _, ok := c.Match("someone/totally-new-thing", ""); ok {
		t.Error("unknown image must not match")
	}
	if s, ok := c.Match("someone/mystery", "Uptime Kuma"); !ok || s.ID != "uptime-kuma" {
		t.Errorf("title fallback: %q %v", s.ID, ok)
	}
}

// A real Unraid container list, with names and addresses anonymised.
func TestUnraidSampleRecognition(t *testing.T) {
	ss := Recognize(loadFixture(t, "unraid-sample.json"), catalog(t))
	st := count(ss)
	for _, s := range ss {
		t.Logf("%-8s %-28s %-24s part of %-14q web %+v", s.Kind, s.Name, s.AppName, s.PartOf, s.Web)
	}
	t.Logf("stats %+v", st)
	if st.Total != 33 {
		t.Fatalf("total = %d", st.Total)
	}
	// Spec goal: at least 95 % of ordinary apps recognised automatically.
	if st.Percent < 95 {
		for _, s := range ss {
			if s.Kind == KindUnknown {
				t.Logf("not recognised: %s (%s)", s.Name, s.Image)
			}
		}
		t.Errorf("recognised %d%%, want >= 95%%", st.Percent)
	}
	if st.Own != 3 {
		t.Errorf("own apps = %d, want 3 (home-dashboard, family-media, mm-legacy)", st.Own)
	}

	m := byName(ss)
	check := func(name, kind, app string) {
		t.Helper()
		s, ok := m[name]
		if !ok {
			t.Errorf("%s missing", name)
			return
		}
		if s.Kind != kind || (app != "" && s.AppID != app) {
			t.Errorf("%s: kind %s app %s, want %s %s", name, s.Kind, s.AppID, kind, app)
		}
	}
	check("plex", KindApp, "plex")
	check("binhex-emby", KindApp, "emby")
	check("immich-server", KindApp, "immich")
	check("immich-postgres", KindSupport, "postgres")
	check("immich-redis", KindSupport, "redis")
	check("librechat-mongo", KindSupport, "mongo")
	check("nextcloud-aio-talk", KindSupport, "nextcloud-aio-part")
	check("home-dashboard", KindOwn, "")
	check("Newt", KindApp, "newt")

	// Helpers belong to their app.
	if p := m["immich-postgres"].PartOf; p != "Immich" {
		t.Errorf("immich-postgres part of %q", p)
	}
	if p := m["librechat-mongo"].PartOf; p != "LibreChat" {
		t.Errorf("librechat-mongo part of %q", p)
	}
	if p := m["nextcloud-aio-database"].PartOf; p != "Nextcloud AIO" {
		t.Errorf("nextcloud-aio-database part of %q", p)
	}

	// Web links: Unraid label, host network, published port.
	web := func(name string) WebLink {
		t.Helper()
		w := m[name].Web
		if w == nil {
			t.Errorf("%s: no web link", name)
			return WebLink{}
		}
		return *w
	}
	if w := web("plex"); w.Port != 32400 || w.Path != "/web/index.html" || w.Host != "" {
		t.Errorf("plex web = %+v", w)
	}
	if w := web("immich-server"); w.Port != 2283 {
		t.Errorf("immich web = %+v", w)
	}
	if w := web("vaultwarden"); w.Port != 4743 || w.Path != "/admin" {
		t.Errorf("vaultwarden web = %+v (container port 80 is published as 4743)", w)
	}
	if w := web("MagicMirror"); w.Port != 8099 {
		t.Errorf("MagicMirror web = %+v (template names the machine port)", w)
	}
	if w := web("nextcloud-aio-mastercontainer"); w.Scheme != "https" || w.Port != 7282 {
		t.Errorf("nextcloud aio web = %+v", w)
	}
	if w := web("glances"); w.Port != 61208 {
		t.Errorf("glances (host network) web = %+v", w)
	}
	if w := web("WordPress"); w.Port != 8090 {
		t.Errorf("WordPress web = %+v (only published port)", w)
	}
	if m["immich-postgres"].Web != nil {
		t.Error("databases get no web link")
	}
	if m["nextcloud-aio-borgbackup"].State != "exited" {
		t.Error("stopped containers keep their state")
	}
	if m["immich-server"].Health != "healthy" {
		t.Errorf("health = %q", m["immich-server"].Health)
	}
}

func TestMacvlanUsesContainerIP(t *testing.T) {
	c := Container{ID: "abc", Names: []string{"/jf"}, Image: "jellyfin/jellyfin", State: "running"}
	c.HostConfig.NetworkMode = "br0"
	c.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"br0": {IPAddress: "192.168.1.50"}}
	s := Recognize([]Container{c}, catalog(t))[0]
	if s.Web == nil || s.Web.Host != "192.168.1.50" || s.Web.Port != 8096 {
		t.Errorf("web = %+v", s.Web)
	}
	if s.Group != "" {
		t.Errorf("br0 is shared, no group expected: %q", s.Group)
	}
}

// A port published on one address only is checked on that address: the
// Docker gateway or host.docker.internal would not reach it.
func TestPortBoundToOneAddress(t *testing.T) {
	jf := Container{ID: "a", Names: []string{"/jf"}, Image: "jellyfin/jellyfin", State: "running",
		Ports: []ContainerPort{{IP: "192.168.1.20", PrivatePort: 8096, PublicPort: 8096, Type: "tcp"}}}
	all := Container{ID: "b", Names: []string{"/jf2"}, Image: "jellyfin/jellyfin", State: "running",
		Ports: []ContainerPort{{IP: "0.0.0.0", PrivatePort: 8096, PublicPort: 18096, Type: "tcp"},
			{IP: "::", PrivatePort: 8096, PublicPort: 18096, Type: "tcp"}}}
	lo := Container{ID: "c", Names: []string{"/jf3"}, Image: "jellyfin/jellyfin", State: "running",
		Ports: []ContainerPort{{IP: "127.0.0.1", PrivatePort: 8096, PublicPort: 28096, Type: "tcp"}}}
	ss := Recognize([]Container{jf, all, lo}, catalog(t))
	m := byName(ss)
	if w := m["jf"].Web; w == nil || w.Host != "192.168.1.20" || w.Port != 8096 {
		t.Errorf("one address: web = %+v", w)
	}
	if w := m["jf2"].Web; w == nil || w.Host != "" || w.Port != 18096 {
		t.Errorf("every address: web = %+v", w)
	}
	if len(m["jf2"].Ports) != 1 {
		t.Errorf("IPv4 and IPv6 bindings should be one port: %+v", m["jf2"].Ports)
	}
	if w := m["jf3"].Web; w == nil || w.Host != "" {
		t.Errorf("loopback: web = %+v", w)
	}
}

type fakeLister struct {
	cs  []Container
	err error
}

func (f fakeLister) Containers(context.Context) ([]Container, error) { return f.cs, f.err }

func TestDiscoverErrors(t *testing.T) {
	cat := catalog(t)
	r := Discover(context.Background(), cat, func() (Lister, error) { return nil, ErrNoDocker })
	if r.Available || r.Error != "docker_not_found" {
		t.Errorf("no docker: %+v", r)
	}
	r = Discover(context.Background(), cat, func() (Lister, error) { return fakeLister{err: errors.New("permission denied")}, nil })
	if r.Available || !strings.Contains(r.Error, "permission") {
		t.Errorf("list error: %+v", r)
	}
	r = Discover(context.Background(), cat, func() (Lister, error) {
		return fakeLister{cs: []Container{{ID: "1", Names: []string{"/x"}, Image: "louislam/uptime-kuma:1", State: "running"}}}, nil
	})
	if !r.Available || r.Stats.Recognized != 1 || r.Stats.Running != 1 || r.Services[0].AppName != "Uptime Kuma" {
		t.Errorf("ok: %+v", r)
	}
}

// The client must only read: every request is a GET.
func TestDockerClientOnlyReads(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Write([]byte(`[{"Id":"0123456789abcdef","Names":["/kuma"],"Image":"louislam/uptime-kuma","State":"running","Status":"Up (healthy)","Ports":[{"PrivatePort":3001,"PublicPort":3001,"Type":"tcp"}]}]`))
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	r := Discover(context.Background(), catalog(t), DockerLister)
	if !r.Available || len(r.Services) != 1 || r.Services[0].Web.Port != 3001 {
		t.Fatalf("result = %+v", r)
	}
	for _, m := range methods {
		if !strings.HasPrefix(m, "GET ") {
			t.Errorf("non-GET request: %s", m)
		}
	}
	t.Setenv("DOCKER_HOST", "unix:///does/not/exist.sock")
	if r := Discover(context.Background(), catalog(t), DockerLister); r.Error != "docker_not_found" {
		t.Errorf("missing socket: %+v", r)
	}
}

// Every category and container state the interface can show needs a text
// in every language.
func TestCategoriesTranslated(t *testing.T) {
	b := i18n.MustLoad()
	keys := []string{}
	for _, c := range Categories() {
		keys = append(keys, "category."+c)
	}
	for _, s := range []string{"running", "exited", "paused", "restarting", "created", "dead", "removing"} {
		keys = append(keys, "state."+s)
	}
	for _, lang := range b.Languages() {
		for _, k := range keys {
			if _, ok := b.Keys(lang)[k]; !ok {
				t.Errorf("%s: %s missing", lang, k)
			}
		}
	}
}
