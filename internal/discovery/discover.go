package discovery

import (
	"context"
	"errors"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kinds of service.
const (
	KindApp     = "app"     // a recognised app
	KindSupport = "support" // database, cache or part of another app
	KindOwn     = "own"     // built locally by the user, nothing to recognise
	KindUnknown = "unknown" // a public image Nestward does not know yet
)

// Port is one port of a service.
type Port struct {
	Host      int    `json:"host,omitempty"` // published on the machine, 0 = not published
	Container int    `json:"container"`
	Proto     string `json:"proto"`
	// IP is set when the port is published on one address of the machine
	// only (for example 192.168.1.20:8080:80). Empty = every address.
	IP string `json:"ip,omitempty"`
}

// WebLink says where a service's web page is. An empty Host means "the
// machine's own address", which the interface fills in.
type WebLink struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port"`
	Path   string `json:"path,omitempty"`
}

// Service is one running (or stopped) container.
type Service struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Kind     string `json:"kind"`
	AppID    string `json:"app_id,omitempty"`
	AppName  string `json:"app_name"`
	Category string `json:"category,omitempty"`
	Login    string `json:"login,omitempty"` // own, none or empty when not known
	Profile  string `json:"profile,omitempty"`
	Group    string `json:"group,omitempty"`
	PartOf   string `json:"part_of,omitempty"` // name of the app a helper belongs to
	State    string `json:"state"`             // running, exited, paused, restarting, created, dead
	Health   string `json:"health,omitempty"`  // healthy, unhealthy, starting
	// Finished is true when the container exited with code 0: a job that
	// ran to the end (like a backup) or one stopped on purpose.
	Finished bool     `json:"finished,omitempty"`
	Network  string   `json:"network"`
	Ports    []Port   `json:"ports"`
	Web      *WebLink `json:"web,omitempty"`
	Guest    *Guest   `json:"guest,omitempty"` // set for Proxmox VMs and containers
	// Autostart says whether a running service comes back by itself when
	// the machine starts again after a power cut: "yes", "no", or empty
	// when Nestward cannot tell.
	Autostart string `json:"autostart,omitempty"`
}

// Stats counts what was recognised.
type Stats struct {
	Total      int `json:"total"`
	Recognized int `json:"recognized"` // apps + helpers
	Own        int `json:"own"`
	Unknown    int `json:"unknown"`
	Running    int `json:"running"`
	// Percent is recognised share of the public images (own builds are
	// left out, nothing can recognise them).
	Percent int `json:"percent"`
	// Proxmox guests are counted apart: they are not Docker images.
	Guests        int `json:"guests,omitempty"`
	GuestsRunning int `json:"guests_running,omitempty"`
}

// Result is the outcome of one discovery run.
type Result struct {
	Source    string    `json:"source"` // "docker", "proxmox" or "docker+proxmox"
	Available bool      `json:"available"`
	Error     string    `json:"error,omitempty"` // "docker_not_found" or a technical message
	At        time.Time `json:"at"`
	Services  []Service `json:"services"`
	Stats     Stats     `json:"stats"`
	// Boot is what starts by itself on this machine, when known.
	Boot *Boot `json:"boot,omitempty"`
	// Backups are the backup folders and backup containers found here.
	Backups []Backup `json:"backups,omitempty"`
}

// Lister returns the containers to look at.
type Lister interface {
	Containers(ctx context.Context) ([]Container, error)
}

// Discover lists Docker containers and recognises them. It never fails:
// problems end up in Result.Error.
func Discover(ctx context.Context, cat *Catalog, newLister func() (Lister, error)) Result {
	r := Result{Source: "docker", At: time.Now().UTC(), Services: []Service{}}
	l, err := newLister()
	if errors.Is(err, ErrNoDocker) {
		r.Error = "docker_not_found"
		return r
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	cs, err := l.Containers(ctx)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Available = true
	r.Services = Recognize(cs, cat)
	r.Stats = count(r.Services)
	if pl, ok := l.(policyLister); ok {
		for i := range r.Services {
			s := &r.Services[i]
			if s.State != "running" {
				continue
			}
			if p, err := pl.RestartPolicy(ctx, s.ID); err == nil {
				s.Autostart = AutostartNo
				if p == "always" || p == "unless-stopped" {
					s.Autostart = AutostartYes
				}
			}
		}
	}
	containerBackups(ctx, l, &r)
	return r
}

// policyLister can also say how Docker restarts a container.
type policyLister interface {
	RestartPolicy(ctx context.Context, id string) (string, error)
}

// DockerLister adapts NewDockerClient for Discover.
func DockerLister() (Lister, error) { return NewDockerClient() }

// Recognize turns containers into services, groups helpers with their
// app and sorts the list.
func Recognize(cs []Container, cat *Catalog) []Service {
	out := make([]Service, 0, len(cs))
	for _, c := range cs {
		out = append(out, toService(c, cat))
	}
	group(out)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.groupKey() != b.groupKey() {
			return a.groupKey() < b.groupKey()
		}
		if (a.Kind == KindSupport) != (b.Kind == KindSupport) {
			return b.Kind == KindSupport
		}
		return a.Name < b.Name
	})
	return out
}

func (s Service) groupKey() string {
	if s.PartOf != "" {
		return strings.ToLower(s.PartOf)
	}
	return strings.ToLower(s.AppName)
}

func containerName(c Container) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

var healthRe = regexp.MustCompile(`\((healthy|unhealthy|health: starting)\)`)

// exitedOK matches Docker's status for a clean exit: "Exited (0) 3 hours ago".
var exitedOK = regexp.MustCompile(`^Exited \(0\)`)

func toService(c Container, cat *Catalog) Service {
	s := Service{
		ID:      shortID(c.ID),
		Name:    containerName(c),
		Image:   c.Image,
		State:   c.State,
		Network: c.HostConfig.NetworkMode,
		Ports:   ports(c.Ports),
	}
	s.Finished = c.State == "exited" && exitedOK.MatchString(c.Status)
	if m := healthRe.FindStringSubmatch(c.Status); m != nil {
		s.Health = strings.TrimPrefix(m[1], "health: ")
	}
	sig, ok := cat.Match(c.Image, c.Labels["org.opencontainers.image.title"])
	switch {
	case ok:
		s.Kind = KindApp
		if sig.Role == RoleSupport {
			s.Kind = KindSupport
		}
		s.AppID, s.AppName, s.Category, s.Login, s.Profile = sig.ID, sig.Name, sig.Category, sig.Login, sig.Profile
	case isOwnImage(c.Image):
		s.Kind = KindOwn
		s.AppName = prettyName(s.Name)
	default:
		s.Kind = KindUnknown
		s.AppName = prettyName(s.Name)
	}
	if s.Kind != KindSupport {
		s.Web = webLink(c, s.Ports, sig, ok)
	}
	if p := c.Labels["com.docker.compose.project"]; p != "" {
		s.Group = p
	} else if n := c.HostConfig.NetworkMode; n != "" && !sharedNetwork(n) {
		s.Group = n
	}
	return s
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// isOwnImage is true for images without a namespace that Nestward does
// not know, such as "home-dashboard:0.4.0": built on the machine itself.
func isOwnImage(image string) bool {
	repo := NormalizeImage(image)
	return !strings.Contains(repo, "/")
}

func prettyName(name string) string {
	n := strings.NewReplacer("_", " ", "-", " ").Replace(name)
	if n == "" {
		return name
	}
	return strings.ToUpper(n[:1]) + n[1:]
}

// sharedNetwork is true for networks many unrelated containers use, so
// they say nothing about which containers belong together.
func sharedNetwork(n string) bool {
	switch n {
	case "bridge", "host", "none", "default":
		return true
	}
	return macvlanLike(n) || strings.HasPrefix(n, "container:")
}

// boundIP keeps the address a port is published on when it is one
// specific address. Every address (0.0.0.0, ::) gives "", and so does
// loopback, which other machines and containers cannot reach anyway.
func boundIP(ip string) string {
	a := net.ParseIP(ip)
	if a == nil || a.IsUnspecified() || a.IsLoopback() {
		return ""
	}
	return a.String()
}

var macvlanRe = regexp.MustCompile(`^(br|eth|bond|wlan|vhost)\d+(\.\d+)?$`)

// macvlanLike matches Unraid's custom networks (br0, eth0, bond0) where
// every container gets its own address on the home network.
func macvlanLike(n string) bool { return macvlanRe.MatchString(n) }

func ports(ps []ContainerPort) []Port {
	seen := map[Port]bool{}
	out := []Port{}
	for _, p := range ps {
		// Docker lists IPv4 and IPv6 bindings separately; keep one.
		x := Port{Host: p.PublicPort, Container: p.PrivatePort, Proto: p.Type, IP: boundIP(p.IP)}
		if seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Container != out[j].Container {
			return out[i].Container < out[j].Container
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

// Unraid writes the web page address as a template label, e.g.
// "http://[IP]:[PORT:32400]/web/index.html".
var unraidWebUI = regexp.MustCompile(`(?i)^(https?)://([^/:\]]+|\[ip\]):(?:\[port:(\d+)\]|(\d+))(/.*)?$`)

func webLink(c Container, ps []Port, sig Signature, known bool) *WebLink {
	hostNet := c.HostConfig.NetworkMode == "host"
	ownIP := ""
	if macvlanLike(c.HostConfig.NetworkMode) {
		ownIP = c.NetworkSettings.Networks[c.HostConfig.NetworkMode].IPAddress
	}
	// publishedFor finds the machine port for a port inside the container,
	// and the one address it is published on, if any.
	publishedFor := func(cp int) (int, string, bool) {
		if hostNet || ownIP != "" {
			return cp, "", true
		}
		for _, p := range ps {
			if p.Container == cp && p.Host != 0 && p.Proto == "tcp" {
				return p.Host, p.IP, true
			}
		}
		// Some Unraid templates name the machine port instead.
		for _, p := range ps {
			if p.Host == cp && p.Proto == "tcp" {
				return cp, p.IP, true
			}
		}
		return 0, "", false
	}

	if m := unraidWebUI.FindStringSubmatch(c.Labels["net.unraid.docker.webui"]); m != nil {
		l := &WebLink{Scheme: strings.ToLower(m[1]), Path: strings.TrimSuffix(m[5], "/")}
		if !strings.EqualFold(m[2], "[ip]") {
			l.Host = m[2]
		} else if ownIP != "" {
			l.Host = ownIP
		}
		if m[3] != "" {
			cp, _ := strconv.Atoi(m[3])
			if hp, ip, ok := publishedFor(cp); ok {
				l.Port = hp
				if l.Host == "" {
					l.Host = ip
				}
			}
		} else {
			l.Port, _ = strconv.Atoi(m[4])
		}
		if l.Port != 0 {
			return l
		}
	}
	if known && sig.Port != 0 {
		if hp, ip, ok := publishedFor(sig.Port); ok {
			h := ownIP
			if h == "" {
				h = ip
			}
			return &WebLink{Scheme: sig.Scheme, Host: h, Port: hp, Path: sig.Path}
		}
	}
	// Last resort: a web app with exactly one published TCP port.
	if known && sig.Port != 0 || c.Labels["net.unraid.docker.webui"] != "" {
		var only []Port
		for _, p := range ps {
			if p.Host != 0 && p.Proto == "tcp" {
				only = append(only, p)
			}
		}
		if len(only) == 1 {
			scheme := "http"
			if known {
				scheme = sig.Scheme
			}
			return &WebLink{Scheme: scheme, Host: only[0].IP, Port: only[0].Host}
		}
	}
	return nil
}

// group links helpers to the app they serve. Containers in the same
// compose project or private network form a group; the first real app
// in it is the main one.
func group(ss []Service) {
	type pick struct {
		name string
		web  bool
	}
	main := map[string]pick{}
	for _, s := range ss {
		if s.Group == "" || s.Kind == KindSupport {
			continue
		}
		// Prefer an app with a web page as the face of the group.
		if cur, ok := main[s.Group]; !ok || (!cur.web && s.Web != nil) {
			main[s.Group] = pick{s.AppName, s.Web != nil}
		}
	}
	for i := range ss {
		s := &ss[i]
		if s.Kind != KindSupport || s.Group == "" {
			continue
		}
		if p, ok := main[s.Group]; ok {
			s.PartOf = p.name
		}
	}
}

func count(ss []Service) Stats {
	st := Stats{Total: len(ss)}
	for _, s := range ss {
		switch s.Kind {
		case KindApp, KindSupport:
			st.Recognized++
		case KindOwn:
			st.Own++
		default:
			st.Unknown++
		}
		if s.State == "running" {
			st.Running++
		}
	}
	if public := st.Total - st.Own; public > 0 {
		st.Percent = st.Recognized * 100 / public
	} else {
		st.Percent = 100
	}
	return st
}
