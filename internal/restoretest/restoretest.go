// Package restoretest proves that Proxmox backups can be restored. Now and
// then it takes a guest's newest vzdump backup, restores it as a temporary
// copy with every network card, passed-through device and host folder
// removed, starts the copy, checks that it comes up, and deletes it again.
//
// It is off unless the agent is started with -restore-test. It only ever
// stops or deletes guests it created itself: they carry the tag
// "nestward-restore-test", a name starting with "nestward-test-" and an ID
// in the range IDFirst-IDLast, and all three are checked before each step.
package restoretest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
)

// Tag marks every copy this package makes.
const Tag = "nestward-restore-test"

// namePrefix starts the name of every copy.
const namePrefix = "nestward-test-"

// The IDs used for copies.
const (
	IDFirst = 9900
	IDLast  = 9999
)

// Runner runs a Proxmox command (pvesh, pct, qm, qmrestore) and returns
// its output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Tester runs restore tests on one Proxmox node.
type Tester struct {
	Run     Runner // nil runs the real commands
	Node    string // "" = this machine's host name
	ConfDir string // "" = /etc/pve
	DataDir string // where results are kept
	// Every is how often each guest is tested; default 28 days.
	Every time.Duration
	// MaxArchive skips backups bigger than this; default 40 GiB.
	MaxArchive int64
	// Wait is how long a started copy gets to prove it works; default 5
	// minutes for a VM (1 minute for a container).
	Wait time.Duration
	// Poll is the pause between checks; default 5 seconds.
	Poll time.Duration
	// MemAvailable returns the host's free memory in bytes; nil reads
	// /proc/meminfo.
	MemAvailable func() (int64, error)
	Now          func() time.Time
	Log          *slog.Logger

	mu    sync.Mutex
	state *state
	busy  sync.Mutex // one test at a time
}

// state is what is kept between runs.
type state struct {
	Results map[int]discovery.RestoreTest `json:"results"`
	// Pending is a copy that was made and is not deleted yet.
	Pending int `json:"pending,omitempty"`
}

const stateFile = "restore-tests.json"

// Candidate is a guest with a backup that can be tested.
type Candidate struct {
	VMID    int
	Type    string // "ct" or "vm"
	Name    string
	Volid   string // storage:backup/vzdump-...
	Archive string
	Size    int64
	Backup  time.Time
}

func (t *Tester) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tester) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func (t *Tester) confDir() string {
	if t.ConfDir != "" {
		return t.ConfDir
	}
	return "/etc/pve"
}

func (t *Tester) node() (string, error) {
	if t.Node != "" {
		return t.Node, nil
	}
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return strings.SplitN(h, ".", 2)[0], nil
}

// run runs a command with a time limit.
func (t *Tester) run(ctx context.Context, limit time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if t.Run != nil {
		return t.Run(ctx, name, args...)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, tail(stderr.String(), 300))
	}
	return out, nil
}

func (t *Tester) pvesh(ctx context.Context, v any, path string, extra ...string) error {
	args := append([]string{"get", path}, extra...)
	out, err := t.run(ctx, 30*time.Second, "pvesh", append(args, "--output-format", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// --- kept results ---

func (t *Tester) load() *state {
	if t.state != nil {
		return t.state
	}
	st := &state{Results: map[int]discovery.RestoreTest{}}
	if t.DataDir != "" {
		if raw, err := os.ReadFile(filepath.Join(t.DataDir, stateFile)); err == nil {
			json.Unmarshal(raw, st)
			if st.Results == nil {
				st.Results = map[int]discovery.RestoreTest{}
			}
		}
	}
	t.state = st
	return st
}

func (t *Tester) save() {
	if t.DataDir == "" {
		return
	}
	raw, _ := json.MarshalIndent(t.state, "", "  ")
	tmp := filepath.Join(t.DataDir, stateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		os.Rename(tmp, filepath.Join(t.DataDir, stateFile))
	}
}

// Results returns the latest result for each guest, by VMID.
func (t *Tester) Results() map[int]discovery.RestoreTest {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[int]discovery.RestoreTest{}
	for k, v := range t.load().Results {
		out[k] = v
	}
	return out
}

// Attach puts the latest results on the guests of a discovery result.
func (t *Tester) Attach(r *discovery.Result) {
	res := t.Results()
	for i := range r.Services {
		if g := r.Services[i].Guest; g != nil {
			if rt, ok := res[g.VMID]; ok {
				g.RestoreTest = &rt
			}
		}
	}
}

func (t *Tester) record(id int, r discovery.RestoreTest, pending int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.load()
	if id > 0 {
		st.Results[id] = r
	}
	st.Pending = pending
	t.save()
}

// --- what to test ---

// Candidates lists this node's guests with their newest backup.
func (t *Tester) Candidates(ctx context.Context) ([]Candidate, error) {
	node, err := t.node()
	if err != nil {
		return nil, err
	}
	var stores []struct {
		Storage string `json:"storage"`
		Active  int    `json:"active"`
	}
	if err := t.pvesh(ctx, &stores, "/nodes/"+node+"/storage", "--content", "backup"); err != nil {
		return nil, err
	}
	newest := map[int]Candidate{}
	for _, s := range stores {
		if s.Active != 1 {
			continue
		}
		var items []struct {
			Volid   string      `json:"volid"`
			VMID    json.Number `json:"vmid"`
			CTime   int64       `json:"ctime"`
			Size    int64       `json:"size"`
			Subtype string      `json:"subtype"`
		}
		if err := t.pvesh(ctx, &items, "/nodes/"+node+"/storage/"+s.Storage+"/content", "--content", "backup"); err != nil {
			continue
		}
		for _, it := range items {
			id, err := strconv.Atoi(it.VMID.String())
			if err != nil || it.CTime <= 0 || (id >= IDFirst && id <= IDLast) {
				continue
			}
			typ := ""
			switch it.Subtype {
			case "lxc":
				typ = "ct"
			case "qemu":
				typ = "vm"
			default:
				continue
			}
			at := time.Unix(it.CTime, 0).UTC()
			if cur, ok := newest[id]; ok && !at.After(cur.Backup) {
				continue
			}
			_, archive, _ := strings.Cut(it.Volid, "/")
			newest[id] = Candidate{VMID: id, Type: typ, Volid: it.Volid, Archive: archive, Size: it.Size, Backup: at}
		}
	}
	out := []Candidate{}
	for id, c := range newest {
		// Only guests that still exist on this node are tested.
		conf, ok := t.readConf(node, c.Type, id)
		if !ok {
			continue
		}
		c.Name = conf.get("hostname")
		if c.Type == "vm" {
			c.Name = conf.get("name")
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}

// Due picks the guest that has waited longest for a test, or false when
// every guest was tested within Every.
func (t *Tester) Due(cands []Candidate) (Candidate, bool) {
	every := t.Every
	if every <= 0 {
		every = 28 * 24 * time.Hour
	}
	res := t.Results()
	now := t.now()
	var best Candidate
	var bestAt time.Time
	found := false
	for _, c := range cands {
		last, tested := res[c.VMID]
		at := time.Time{}
		if tested {
			at = last.TestedAt
			if now.Sub(at) < every && !(last.Result == discovery.RestoreFailed && now.Sub(at) > 7*24*time.Hour) {
				continue
			}
		}
		if !found || at.Before(bestAt) {
			best, bestAt, found = c, at, true
		}
	}
	return best, found
}

// --- the test ---

// Test restores one candidate, starts it, checks it and deletes it.
func (t *Tester) Test(ctx context.Context, c Candidate) (res discovery.RestoreTest) {
	t.busy.Lock()
	defer t.busy.Unlock()
	start := t.now()
	res = discovery.RestoreTest{TestedAt: start.UTC(), Backup: c.Backup, Archive: c.Archive, Result: discovery.RestoreOK}
	fail := func(stage string, err error) discovery.RestoreTest {
		res.Result, res.Stage = discovery.RestoreFailed, stage
		if err != nil {
			res.Detail = tail(err.Error(), 400)
		}
		return res
	}
	skip := func(stage, detail string) discovery.RestoreTest {
		res.Result, res.Stage, res.Detail = discovery.RestoreSkipped, stage, detail
		return res
	}
	defer func() {
		res.Seconds = int(t.now().Sub(start).Seconds())
		t.log().Info("restore test", "vmid", c.VMID, "name", c.Name, "result", res.Result, "stage", res.Stage, "proof", res.Proof, "seconds", res.Seconds)
	}()

	node, err := t.node()
	if err != nil {
		return fail("restore", err)
	}
	max := t.MaxArchive
	if max <= 0 {
		max = 40 << 30
	}
	if c.Size > max {
		return skip("size", fmt.Sprintf("%d GiB > %d GiB", c.Size>>30, max>>30))
	}
	orig, ok := t.readConf(node, c.Type, c.VMID)
	if !ok {
		return skip("restore", "the guest is gone")
	}
	storage := orig.rootStorage(c.Type)
	if storage == "" {
		return fail("restore", errors.New("could not tell which storage the guest's disk is on"))
	}
	if msg := t.checkSpace(ctx, node, storage, c, orig); msg != "" {
		return skip("space", msg)
	}
	mem := orig.memoryMB()
	limit := int64(2048)
	if c.Type == "vm" {
		limit = 4096
	}
	mem = min(mem, limit)
	startIt := true
	if avail, err := t.memAvailable(); err == nil && avail < (mem+1024)<<20 {
		startIt = false // restore only; starting would squeeze the host
	}

	id, err := t.freeID(ctx, node)
	if err != nil {
		return fail("restore", err)
	}
	t.record(0, discovery.RestoreTest{}, id)

	// 1. Restore.
	var restoreErr error
	if c.Type == "ct" {
		_, restoreErr = t.run(ctx, 3*time.Hour, "pct", "restore", strconv.Itoa(id), c.Volid,
			"--storage", storage, "--unique", "1")
	} else {
		_, restoreErr = t.run(ctx, 3*time.Hour, "qmrestore", c.Volid, strconv.Itoa(id),
			"--storage", storage, "--unique", "1")
	}
	if restoreErr != nil {
		t.cleanup(ctx, node, c.Type, id)
		if spaceProblem(restoreErr) {
			return skip("space", tail(restoreErr.Error(), 300))
		}
		return fail("restore", restoreErr)
	}
	// 2. Cut it off from the world and mark it as ours.
	if err := t.isolate(node, c.Type, id, mem); err != nil {
		t.cleanup(ctx, node, c.Type, id)
		return fail("restore", err)
	}
	// 3. Start and check.
	if startIt {
		res.Proof, err = t.startAndCheck(ctx, node, c.Type, id)
		if err != nil {
			stage := "start"
			if errors.Is(err, errCheck) {
				stage = "check"
			}
			cerr := t.cleanup(ctx, node, c.Type, id)
			r := fail(stage, err)
			if cerr != nil {
				r.Detail += " | cleanup: " + tail(cerr.Error(), 150)
			}
			return r
		}
	} else {
		res.Proof = "restored"
		res.Detail = "memory"
	}
	// 4. Delete the copy.
	if err := t.cleanup(ctx, node, c.Type, id); err != nil {
		return fail("cleanup", err)
	}
	return res
}

var errCheck = errors.New("the copy started but did not answer")

func spaceProblem(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left") || strings.Contains(s, "insufficient free space") ||
		strings.Contains(s, "thin pool") && strings.Contains(s, "full")
}

func (t *Tester) memAvailable() (int64, error) {
	if t.MemAvailable != nil {
		return t.MemAvailable()
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, err := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("no MemAvailable")
}

// checkSpace says why there is not room for the copy, or "" when there is.
func (t *Tester) checkSpace(ctx context.Context, node, storage string, c Candidate, conf conf) string {
	var st struct {
		Type  string `json:"type"`
		Avail int64  `json:"avail"`
		Total int64  `json:"total"`
	}
	if err := t.pvesh(ctx, &st, "/nodes/"+node+"/storage/"+storage+"/status"); err != nil {
		return "storage " + storage + ": " + tail(err.Error(), 200)
	}
	need := conf.diskBytes()
	switch st.Type {
	case "lvmthin", "zfspool", "btrfs", "rbd":
		// Thin storage only uses what is written; a compressed backup
		// rarely grows more than three times.
		need = min(need, c.Size*3+(1<<30))
	}
	if st.Avail-need < st.Total/20 {
		return fmt.Sprintf("%s: %d GiB free, the copy needs about %d GiB", storage, st.Avail>>30, need>>30)
	}
	return ""
}

// freeID finds an unused ID in the copy range.
func (t *Tester) freeID(ctx context.Context, node string) (int, error) {
	for id := IDFirst; id <= IDLast; id++ {
		if _, err := t.run(ctx, 20*time.Second, "pvesh", "get", "/cluster/nextid", "--vmid", strconv.Itoa(id)); err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free ID between %d and %d", IDFirst, IDLast)
}

// Drop these from a copy: network cards, passed-through devices and host
// folders would let it reach or change the real world.
var (
	dropCT = regexp.MustCompile(`^(net\d+|dev\d+|lxc\..*|hookscript|startup|protection|onboot|tags|hostname|memory|swap)$`)
	dropVM = regexp.MustCompile(`^(net\d+|hostpci\d+|usb\d+|parallel\d+|virtiofs\d+|ivshmem|args|hookscript|startup|protection|onboot|tags|name|memory|balloon|shares)$`)
	mpKey  = regexp.MustCompile(`^mp\d+$`)
)

// isolate rewrites the copy's config: no network, no devices, no host
// folders, no autostart, little memory, and our tag and name.
func (t *Tester) isolate(node, typ string, id int, memMB int64) error {
	path := t.confPath(node, typ, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out []string
	main := true
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "[") {
			main = false // snapshot sections: a restore has none, keep as is
		}
		if main {
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue // description
			}
			k, v, _ := strings.Cut(line, ":")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			drop := dropVM
			if typ == "ct" {
				drop = dropCT
			}
			if drop.MatchString(k) {
				continue
			}
			if typ == "ct" && mpKey.MatchString(k) && strings.HasPrefix(v, "/") {
				continue // a folder on the host, not a disk of its own
			}
		}
		out = append(out, line)
	}
	name := namePrefix + strconv.Itoa(id)
	head := []string{"onboot: 0", "tags: " + Tag, "memory: " + strconv.FormatInt(memMB, 10)}
	if typ == "ct" {
		head = append(head, "hostname: "+name, "swap: 0")
	} else {
		head = append(head, "name: "+name, "balloon: 0")
	}
	text := strings.Join(append(head, out...), "\n")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return os.WriteFile(path, []byte(text), 0o640)
}

// ours checks all three marks before a copy is stopped or deleted.
func (t *Tester) ours(node, typ string, id int) bool {
	if id < IDFirst || id > IDLast {
		return false
	}
	c, ok := t.readConf(node, typ, id)
	if !ok {
		return false
	}
	name := c.get("hostname")
	if typ == "vm" {
		name = c.get("name")
	}
	return strings.Contains(";"+c.get("tags")+";", ";"+Tag+";") && strings.HasPrefix(name, namePrefix)
}

func (t *Tester) startAndCheck(ctx context.Context, node, typ string, id int) (string, error) {
	sid := strconv.Itoa(id)
	cmd := "qm"
	if typ == "ct" {
		cmd = "pct"
	}
	if !t.ours(node, typ, id) {
		return "", errors.New("the copy lost its marks; not starting it")
	}
	if _, err := t.run(ctx, 3*time.Minute, cmd, "start", sid); err != nil {
		return "", err
	}
	wait, poll := t.Wait, t.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	if wait <= 0 {
		wait = 5 * time.Minute
		if typ == "ct" {
			wait = time.Minute
		}
	}
	conf, _ := t.readConf(node, typ, id)
	agent := typ == "vm" && conf.agentOn()
	deadline := t.now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
		var out []byte
		var err error
		switch {
		case typ == "ct":
			out, err = t.run(ctx, 20*time.Second, "pct", "exec", sid, "--", "cat", "/proc/1/comm")
			if err == nil && strings.TrimSpace(string(out)) != "" {
				return "init", nil
			}
		case agent:
			if _, err = t.run(ctx, 20*time.Second, "qm", "agent", sid, "ping"); err == nil {
				return "agent", nil
			}
		}
		if !t.now().Before(deadline) {
			if !agent && typ == "vm" {
				// No guest agent: the best proof is that it kept running.
				if t.running(ctx, node, typ, id) {
					return "running", nil
				}
				return "", fmt.Errorf("%w: it stopped by itself", errCheck)
			}
			if err != nil {
				return "", fmt.Errorf("%w: %v", errCheck, err)
			}
			return "", errCheck
		}
	}
}

func (t *Tester) running(ctx context.Context, node, typ string, id int) bool {
	path := "qemu"
	if typ == "ct" {
		path = "lxc"
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := t.pvesh(ctx, &st, fmt.Sprintf("/nodes/%s/%s/%d/status/current", node, path, id)); err != nil {
		return false
	}
	return st.Status == "running"
}

// cleanup stops and deletes a copy, but only one that carries our marks.
func (t *Tester) cleanup(ctx context.Context, node, typ string, id int) error {
	if _, ok := t.readConf(node, typ, id); !ok {
		t.record(0, discovery.RestoreTest{}, 0)
		return nil // never created, or already gone
	}
	if !t.ours(node, typ, id) && !t.pending(id) {
		return fmt.Errorf("guest %d is not marked as a Nestward copy; left alone", id)
	}
	sid := strconv.Itoa(id)
	cmd := "qm"
	if typ == "ct" {
		cmd = "pct"
	}
	if t.running(ctx, node, typ, id) {
		if _, err := t.run(ctx, 3*time.Minute, cmd, "stop", sid); err != nil {
			return err
		}
	}
	args := []string{"destroy", sid, "--purge", "1"}
	if typ == "vm" {
		args = append(args, "--destroy-unreferenced-disks", "1")
	}
	if _, err := t.run(ctx, 10*time.Minute, cmd, args...); err != nil {
		return err
	}
	t.record(0, discovery.RestoreTest{}, 0)
	return nil
}

// pending reports whether id is the copy this tester was making: a restore
// cut short has not got our marks yet, but the ID was free before it.
func (t *Tester) pending(id int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return id >= IDFirst && id <= IDLast && t.load().Pending == id
}

// CleanLeftovers deletes copies left by an earlier run that was cut short
// (power cut, agent restart).
func (t *Tester) CleanLeftovers(ctx context.Context) {
	node, err := t.node()
	if err != nil {
		return
	}
	for id := IDFirst; id <= IDLast; id++ {
		for _, typ := range []string{"ct", "vm"} {
			if _, ok := t.readConf(node, typ, id); !ok || (!t.ours(node, typ, id) && !t.pending(id)) {
				continue
			}
			if err := t.cleanup(ctx, node, typ, id); err != nil {
				t.log().Warn("could not delete an old test copy", "vmid", id, "err", err)
			} else {
				t.log().Info("deleted an old test copy", "vmid", id)
			}
		}
	}
}

// Busy reports whether a backup is running on this node right now.
func (t *Tester) Busy(ctx context.Context) bool {
	node, err := t.node()
	if err != nil {
		return true
	}
	var tasks []json.RawMessage
	if err := t.pvesh(ctx, &tasks, "/nodes/"+node+"/tasks", "--source", "active", "--typefilter", "vzdump"); err != nil {
		return true
	}
	return len(tasks) > 0
}

// RunOnce tests the guest that has waited longest, if any is due.
func (t *Tester) RunOnce(ctx context.Context) (discovery.RestoreTest, bool, error) {
	cands, err := t.Candidates(ctx)
	if err != nil {
		return discovery.RestoreTest{}, false, err
	}
	c, ok := t.Due(cands)
	if !ok {
		return discovery.RestoreTest{}, false, nil
	}
	r := t.Test(ctx, c)
	t.record(c.VMID, r, 0)
	return r, true, nil
}

// TestOne tests one guest now, whether it is due or not.
func (t *Tester) TestOne(ctx context.Context, vmid int) (discovery.RestoreTest, error) {
	cands, err := t.Candidates(ctx)
	if err != nil {
		return discovery.RestoreTest{}, err
	}
	for _, c := range cands {
		if c.VMID == vmid {
			r := t.Test(ctx, c)
			t.record(c.VMID, r, 0)
			return r, nil
		}
	}
	return discovery.RestoreTest{}, fmt.Errorf("guest %d has no backup on this machine", vmid)
}

// Loop runs one test a day at the given hour (local time), after the
// night's backups, until ctx ends.
func (t *Tester) Loop(ctx context.Context, hour int) {
	t.CleanLeftovers(ctx)
	for {
		now := t.now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now)):
		}
		// Wait for a backup that is still running, at most three hours.
		for i := 0; i < 18 && t.Busy(ctx); i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
		if _, _, err := t.RunOnce(ctx); err != nil {
			t.log().Warn("restore test", "err", err)
		}
	}
}

// --- guest config files ---

type conf map[string]string

func (c conf) get(k string) string { return c[k] }

func (t *Tester) confPath(node, typ string, id int) string {
	dir := "qemu-server"
	if typ == "ct" {
		dir = "lxc"
	}
	return filepath.Join(t.confDir(), "nodes", node, dir, strconv.Itoa(id)+".conf")
}

func (t *Tester) readConf(node, typ string, id int) (conf, bool) {
	f, err := os.Open(t.confPath(node, typ, id))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	c := conf{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(k, "#") {
			c[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return c, true
}

var diskKey = regexp.MustCompile(`^(rootfs|mp\d+|scsi\d+|virtio\d+|sata\d+|ide\d+|efidisk\d+|tpmstate\d+)$`)

// rootStorage is the storage of the guest's main disk.
func (c conf) rootStorage(typ string) string {
	keys := []string{"rootfs"}
	if typ == "vm" {
		if b := c["bootdisk"]; b != "" {
			keys = []string{b}
		}
		if bo, ok := strings.CutPrefix(c["boot"], "order="); ok {
			keys = append(strings.Split(bo, ";"), keys...)
		}
		keys = append(keys, "scsi0", "virtio0", "sata0", "ide0")
	}
	for _, k := range keys {
		v := c[k]
		if v == "" || strings.Contains(v, "media=cdrom") {
			continue
		}
		if st, _, ok := strings.Cut(strings.SplitN(v, ",", 2)[0], ":"); ok && st != "" && !strings.HasPrefix(st, "/") {
			return st
		}
	}
	return ""
}

// diskBytes adds up the sizes of the guest's disks.
func (c conf) diskBytes() int64 {
	var sum int64
	for k, v := range c {
		if !diskKey.MatchString(k) || strings.Contains(v, "media=cdrom") || strings.HasPrefix(v, "/") {
			continue
		}
		for _, part := range strings.Split(v, ",") {
			if s, ok := strings.CutPrefix(part, "size="); ok {
				sum += parseSize(s)
			}
		}
	}
	return sum
}

func parseSize(s string) int64 {
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

func (c conf) memoryMB() int64 {
	if v, err := strconv.ParseInt(c["memory"], 10, 64); err == nil && v > 0 {
		return v
	}
	return 512
}

func (c conf) agentOn() bool {
	a := c["agent"]
	return a != "" && !strings.HasPrefix(a, "0") && !strings.Contains(a, "enabled=0")
}
