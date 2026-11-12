// Package proxmox lists the virtual machines and containers on a Proxmox
// VE host. The agent runs as root on the host, so it uses Proxmox's own
// read-only command (`pvesh get`) and config files: no API token, and it
// never starts, stops or changes anything.
package proxmox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
)

// Runner runs `pvesh` with the given arguments and returns its output.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Available reports whether this machine is a Proxmox VE host.
func Available() bool {
	if _, err := exec.LookPath("pvesh"); err != nil {
		return false
	}
	_, err := os.Stat("/etc/pve/nodes")
	return err == nil
}

// Reader reads the guests of one Proxmox node.
type Reader struct {
	Run     Runner        // nil runs the real pvesh
	Node    string        // "" = this machine's host name
	ConfDir string        // "" = /etc/pve
	IPAge   time.Duration // how long an address is remembered; default 10 minutes
	ListAge time.Duration // how long the guest list is reused; default 1 minute
	// BackupAge is how long the list of backups is reused; default 10 minutes.
	BackupAge time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	ips      map[string]ipEntry // "lxc/105" -> address, looked up now and then
	list     []discovery.Guest
	listedAt time.Time
	looking  bool           // addresses are being looked up in the background
	lookups  sync.WaitGroup // for WaitLookups
	backups  map[int]time.Time
	backupOK bool
	backupAt time.Time
}

// lookup is one address to find in the background.
type lookup struct {
	key, node, path string
	id              int
	conf            map[string]string
}

type ipEntry struct {
	ip string
	at time.Time
}

func (r *Reader) run(ctx context.Context, args ...string) ([]byte, error) {
	if r.Run != nil {
		return r.Run(ctx, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "pvesh", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pvesh %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (r *Reader) get(ctx context.Context, path string, v any, extra ...string) error {
	args := append([]string{"get", path}, extra...)
	out, err := r.run(ctx, append(args, "--output-format", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

func (r *Reader) node() (string, error) {
	if r.Node != "" {
		return r.Node, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return strings.SplitN(h, ".", 2)[0], nil
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// listed is one row of /cluster/resources --type vm.
type listed struct {
	Type     string      `json:"type"` // qemu or lxc
	Node     string      `json:"node"`
	VMID     json.Number `json:"vmid"`
	Name     string      `json:"name"`
	Status   string      `json:"status"`
	CPUs     float64     `json:"maxcpu"`
	MaxMem   int64       `json:"maxmem"`
	Mem      int64       `json:"mem"`
	MaxDisk  int64       `json:"maxdisk"`
	Disk     int64       `json:"disk"`
	Uptime   int64       `json:"uptime"`
	Tags     string      `json:"tags"`
	Template json.Number `json:"template"`
}

// Guests lists this node's virtual machines and containers (templates
// left out). pvesh is slow on small machines (1-3 seconds a call), so one
// call lists every guest, the list is reused for ListAge and addresses
// are looked up at most every IPAge.
func (r *Reader) Guests(ctx context.Context) ([]discovery.Guest, error) {
	listAge := r.ListAge
	if listAge <= 0 {
		listAge = time.Minute
	}
	r.mu.Lock()
	if r.list != nil && r.now().Sub(r.listedAt) < listAge {
		out := append([]discovery.Guest(nil), r.list...)
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	node, err := r.node()
	if err != nil {
		return nil, err
	}
	var rows []listed
	if err := r.get(ctx, "/cluster/resources", &rows, "--type", "vm"); err != nil {
		return nil, err
	}
	backups, backupOK := r.backupTimes(ctx, node)
	out := []discovery.Guest{}
	var todo []lookup
	for _, row := range rows {
		if row.Node != node || row.Template.String() == "1" {
			continue
		}
		typ, dir := "vm", "qemu-server"
		if row.Type == "lxc" {
			typ, dir = "ct", "lxc"
		} else if row.Type != "qemu" {
			continue
		}
		id, err := strconv.Atoi(row.VMID.String())
		if err != nil {
			continue
		}
		g := discovery.Guest{
			Type: typ, VMID: id, Name: row.Name, Status: row.Status, CPUs: row.CPUs,
			MemMax: row.MaxMem, MemUsed: row.Mem, DiskMax: row.MaxDisk, Uptime: row.Uptime,
		}
		if typ == "ct" {
			g.DiskUsed = row.Disk
		}
		g.Tags = strings.FieldsFunc(row.Tags, func(c rune) bool { return c == ';' || c == ',' || c == ' ' })
		conf := r.config(node, dir, id)
		g.OnBoot = conf["onboot"] == "1"
		if backupOK {
			g.BackupChecked = true
			if at, ok := backups[id]; ok {
				g.LastBackup = &at
			}
		}
		if g.Name == "" {
			g.Name = conf["hostname"]
		}
		if g.Status == "running" {
			if g.IP = staticIP(conf); g.IP == "" {
				key := row.Type + "/" + strconv.Itoa(id)
				ip, fresh := r.cachedIP(key)
				g.IP = ip
				if !fresh {
					todo = append(todo, lookup{key: key, node: node, path: row.Type, id: id, conf: conf})
				}
			}
		}
		out = append(out, g)
	}
	r.mu.Lock()
	r.list, r.listedAt = out, r.now()
	if len(todo) > 0 && !r.looking {
		// Asking a guest for its address takes seconds each, so it never
		// holds up the report: the next lists get the answers.
		r.looking = true
		r.lookups.Add(1)
		go r.lookUp(todo)
	}
	r.mu.Unlock()
	return append([]discovery.Guest(nil), out...), nil
}

// backupTimes finds the newest vzdump backup of each guest in this node's
// storages that hold backups. ok is false when they could not be read.
// Shared storages (a Proxmox Backup Server, NFS) show up here too.
func (r *Reader) backupTimes(ctx context.Context, node string) (map[int]time.Time, bool) {
	age := r.BackupAge
	if age <= 0 {
		age = 10 * time.Minute
	}
	r.mu.Lock()
	if !r.backupAt.IsZero() && r.now().Sub(r.backupAt) < age {
		m, ok := r.backups, r.backupOK
		r.mu.Unlock()
		return m, ok
	}
	r.mu.Unlock()

	newest := map[int]time.Time{}
	ok := true
	var stores []struct {
		Storage string `json:"storage"`
		Active  int    `json:"active"`
	}
	if err := r.get(ctx, "/nodes/"+node+"/storage", &stores, "--content", "backup"); err != nil {
		ok = false
	}
	for _, s := range stores {
		if s.Active != 1 {
			continue
		}
		var items []struct {
			VMID  json.Number `json:"vmid"`
			CTime int64       `json:"ctime"`
		}
		if err := r.get(ctx, "/nodes/"+node+"/storage/"+s.Storage+"/content", &items, "--content", "backup"); err != nil {
			ok = false
			continue
		}
		for _, it := range items {
			id, err := strconv.Atoi(it.VMID.String())
			if err != nil || it.CTime <= 0 {
				continue
			}
			at := time.Unix(it.CTime, 0).UTC()
			if at.After(newest[id]) {
				newest[id] = at
			}
		}
	}
	r.mu.Lock()
	r.backups, r.backupOK, r.backupAt = newest, ok, r.now()
	r.mu.Unlock()
	return newest, ok
}

// WaitLookups waits for background address lookups and makes the next
// Guests call read a fresh list that includes them.
func (r *Reader) WaitLookups() {
	r.lookups.Wait()
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

func (r *Reader) cachedIP(key string) (ip string, fresh bool) {
	age := r.IPAge
	if age <= 0 {
		age = 10 * time.Minute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.ips[key]
	return e.ip, ok && r.now().Sub(e.at) < age
}

func (r *Reader) lookUp(todo []lookup) {
	defer r.lookups.Done()
	for _, l := range todo {
		ip := r.reportedIP(context.Background(), l.node, l.path, l.id, l.conf)
		r.mu.Lock()
		if r.ips == nil {
			r.ips = map[string]ipEntry{}
		}
		r.ips[l.key] = ipEntry{ip: ip, at: r.now()}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.looking = false
	r.mu.Unlock()
}

// config reads the main section of a guest's config file.
func (r *Reader) config(node, dir string, id int) map[string]string {
	base := r.ConfDir
	if base == "" {
		base = "/etc/pve"
	}
	f, err := os.Open(filepath.Join(base, "nodes", node, dir, strconv.Itoa(id)+".conf"))
	if err != nil {
		return map[string]string{}
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") {
			break // snapshots follow
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(k, "#") {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// staticIP reads "ip=192.168.1.50/24" from a container's net0 line.
func staticIP(conf map[string]string) string {
	for _, part := range strings.Split(conf["net0"], ",") {
		if v, ok := strings.CutPrefix(part, "ip="); ok {
			if ip, _, err := net.ParseCIDR(v); err == nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return ""
}

type iface struct {
	Name string `json:"name"`
	IPs  []struct {
		IP   string `json:"ip-address"`
		Type string `json:"ip-address-type"`
	} `json:"ip-addresses"`
}

func (r *Reader) reportedIP(ctx context.Context, node, path string, id int, conf map[string]string) string {
	// A VM without a running guest agent can keep pvesh waiting; give up soon.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var ifs []iface
	switch path {
	case "lxc":
		if err := r.get(ctx, fmt.Sprintf("/nodes/%s/lxc/%d/interfaces", node, id), &ifs); err != nil {
			return ""
		}
	case "qemu":
		if a := conf["agent"]; a == "" || strings.HasPrefix(a, "0") || strings.Contains(a, "enabled=0") {
			return "" // no guest agent: Proxmox cannot know the address
		}
		var res struct {
			Result []iface `json:"result"`
		}
		if err := r.get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", node, id), &res); err != nil {
			return ""
		}
		ifs = res.Result
	}
	return pickIP(ifs)
}

// pickIP prefers the main network card over Docker bridges and the like.
func pickIP(ifs []iface) string {
	best, bestRank := "", 99
	for _, f := range ifs {
		rank := 2
		switch {
		case f.Name == "lo", strings.HasPrefix(f.Name, "docker"), strings.HasPrefix(f.Name, "br-"),
			strings.HasPrefix(f.Name, "veth"), strings.HasPrefix(f.Name, "hassio"), strings.HasPrefix(f.Name, "tailscale"):
			continue
		case strings.HasPrefix(f.Name, "eth"), strings.HasPrefix(f.Name, "en"):
			rank = 1
		}
		for _, a := range f.IPs {
			ip := net.ParseIP(a.IP)
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if rank < bestRank {
				best, bestRank = ip.String(), rank
			}
			break
		}
	}
	return best
}
