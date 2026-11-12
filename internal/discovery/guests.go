package discovery

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// KindGuest is a Proxmox virtual machine or container that Nestward does
// not recognise as a known app.
const KindGuest = "guest"

// Guest is a Proxmox virtual machine ("vm") or LXC container ("ct").
type Guest struct {
	Type     string   `json:"type"` // "vm" or "ct"
	VMID     int      `json:"vmid"`
	Name     string   `json:"name"`
	Status   string   `json:"status"` // running, stopped, paused
	CPUs     float64  `json:"cpus,omitempty"`
	MemMax   int64    `json:"mem_max,omitempty"`
	MemUsed  int64    `json:"mem_used,omitempty"`
	DiskMax  int64    `json:"disk_max,omitempty"`
	DiskUsed int64    `json:"disk_used,omitempty"` // containers only; Proxmox cannot see inside a VM's disk
	Uptime   int64    `json:"uptime,omitempty"`    // seconds
	OnBoot   bool     `json:"onboot"`              // starts with the machine
	Tags     []string `json:"tags,omitempty"`
	IP       string   `json:"ip,omitempty"` // its address on the home network, when known
}

// versionTail matches "16.3" in "haos16.3" or "-2" in "ubuntu-2".
var versionTail = regexp.MustCompile(`[-_.]?[0-9][0-9.]*$`)

// MatchName recognises an app from a guest's name, such as "ollama",
// "nextcloudpi" or "haos16.3". Only exact names and aliases count, so a
// guest called "appar" or "ubuntu" stays unrecognised.
func (c *Catalog) MatchName(name string) (Signature, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	tries := []string{n, versionTail.ReplaceAllString(n, "")}
	if i := strings.IndexAny(n, "-_ ."); i > 0 {
		tries = append(tries, n[:i])
	}
	for _, t := range tries {
		if i, ok := c.byAlias[strings.ReplaceAll(t, " ", "-")]; ok && t != "" {
			return c.sigs[i], true
		}
	}
	return Signature{}, false
}

// AddGuests puts a machine's Proxmox guests into its discovery result, so
// they are shown, monitored and scored like any other service. A Docker
// result on the same machine is kept; err is a Proxmox read problem.
func AddGuests(r *Result, gs []Guest, cat *Catalog, err error) {
	if err != nil {
		if !r.Available {
			r.Source = "proxmox"
			r.Error = "proxmox: " + err.Error()
		}
		return
	}
	if r.Available {
		r.Source += "+proxmox"
	} else {
		r.Source, r.Available, r.Error = "proxmox", true, ""
		r.Services = []Service{}
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].VMID < gs[j].VMID })
	for _, g := range gs {
		s := Service{
			ID:      g.Type + strconv.Itoa(g.VMID),
			Name:    g.Type + "-" + strconv.Itoa(g.VMID),
			Kind:    KindGuest,
			AppName: g.Name,
			State:   guestState(g.Status),
			Network: "proxmox",
			Ports:   []Port{},
			Guest:   &g,
		}
		if sig, ok := cat.MatchName(g.Name); ok {
			s.Kind = KindApp
			if sig.Role == RoleSupport {
				s.Kind = KindSupport
			}
			s.AppID, s.AppName, s.Category, s.Login, s.Profile = sig.ID, sig.Name, sig.Category, sig.Login, sig.Profile
			if sig.Port > 0 && g.IP != "" && s.Kind == KindApp {
				scheme := sig.Scheme
				if scheme == "" {
					scheme = "http"
				}
				s.Web = &WebLink{Scheme: scheme, Host: g.IP, Port: sig.Port, Path: sig.Path}
			}
		}
		r.Services = append(r.Services, s)
		r.Stats.Guests++
		if s.State == "running" {
			r.Stats.GuestsRunning++
		}
	}
}

// guestState speaks Docker's words, which the rest of Nestward knows.
func guestState(status string) string {
	switch status {
	case "stopped":
		return "exited"
	case "":
		return "created"
	}
	return status // running, paused
}
