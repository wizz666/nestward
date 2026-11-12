// Package agent is the small program that runs on each machine and
// reports its hardware and services to the core over an encrypted
// connection. It opens no ports itself: nothing can connect to it.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/pki"
	"github.com/wizz666/nestward/internal/version"
)

// Payload is what an agent sends to the core.
type Payload struct {
	Hardware  hardware.Report   `json:"hardware"`
	Discovery *discovery.Result `json:"discovery,omitempty"`
}

// Identity is what an enrolled agent keeps in its data folder.
type Identity struct {
	Core      string `json:"core"` // https://host:port of the core's agent port
	MachineID string `json:"machine_id"`
}

// Files in the agent's data folder.
const (
	keyFile  = "agent.key"
	certFile = "agent.crt"
	caFile   = "ca.crt"
	idFile   = "identity.json"
	// removedFile marks that the core removed this machine.
	removedFile = "removed"
)

// ErrNotEnrolled means the agent has not been added to a core yet.
var ErrNotEnrolled = errors.New("this machine is not added to a Nestward core yet")

// ErrRemoved means the core removed this machine: its certificate no
// longer works, and only a new add-machine code can bring it back.
var ErrRemoved = errors.New("this machine was removed in Nestward and sends nothing more; " +
	"to add it again, press \"Add machine\" in Nestward and run the new command")

// MarkRemoved remembers that the core removed this machine, so the agent
// stays quiet after a restart instead of knocking on a closed door.
func MarkRemoved(dataDir string) error {
	return os.WriteFile(filepath.Join(dataDir, removedFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Removed reports whether the core has removed this machine.
func Removed(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, removedFile))
	return err == nil
}

// Enroll adds this machine to a core: it makes a key, sends a
// certificate request with the one-time code, checks that the core is the
// one in the command (fingerprint) and saves the certificate it gets.
func Enroll(ctx context.Context, dataDir, core, code, fingerprint, machineID, hostname string) error {
	core = strings.TrimRight(core, "/")
	if !strings.HasPrefix(core, "https://") {
		return errors.New("the core address must start with https://")
	}
	keyPEM, csrPEM, err := pki.NewAgentKey(machineID)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"code": code, "machine_id": machineID, "hostname": hostname, "csr": string(csrPEM)})
	tr := &http.Transport{TLSClientConfig: pki.PinnedConfig(fingerprint)}
	c := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, core+"/api/v1/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nestward-agent/"+version.Version)
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the core: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Cert  string `json:"cert"`
		CA    string `json:"ca"`
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		if out.Error == "bad_code" {
			return errors.New("the code is wrong, used or older than 30 minutes; make a new one in Nestward")
		}
		return fmt.Errorf("the core refused: %s %s", resp.Status, out.Error)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	id, _ := json.MarshalIndent(Identity{Core: core, MachineID: machineID}, "", "  ")
	for name, data := range map[string][]byte{keyFile: keyPEM, certFile: []byte(out.Cert), caFile: []byte(out.CA), idFile: id} {
		if err := os.WriteFile(filepath.Join(dataDir, name), data, 0o600); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(dataDir, removedFile))
	return nil
}

// Load reads an enrolled agent's identity and builds its TLS client.
func Load(dataDir string) (Identity, *http.Client, error) {
	var id Identity
	raw, err := os.ReadFile(filepath.Join(dataDir, idFile))
	if errors.Is(err, os.ErrNotExist) {
		return id, nil, ErrNotEnrolled
	}
	if err != nil {
		return id, nil, err
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return id, nil, err
	}
	read := func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dataDir, n)) }
	keyPEM, err := read(keyFile)
	if err != nil {
		return id, nil, err
	}
	certPEM, err := read(certFile)
	if err != nil {
		return id, nil, err
	}
	caPEM, err := read(caFile)
	if err != nil {
		return id, nil, err
	}
	cfg, err := pki.AgentConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return id, nil, err
	}
	return id, &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

// Agent sends reports to a core.
type Agent struct {
	CoreURL  string
	Interval time.Duration
	Collect  func(ctx context.Context) Payload
	Client   *http.Client // with the agent's certificate
	Log      *slog.Logger
}

// Send collects one report and posts it.
func (a *Agent) Send(ctx context.Context) error {
	body, err := json.Marshal(a.Collect(ctx))
	if err != nil {
		return err
	}
	url := strings.TrimRight(a.CoreURL, "/") + "/api/v1/agent/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nestward-agent/"+version.Version)
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusUnauthorized && bytes.Contains(msg, []byte("certificate_revoked")) {
			return ErrRemoved
		}
		return fmt.Errorf("core answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Run sends a report now and then every Interval until ctx ends. It stops
// with ErrRemoved when the core has removed this machine.
func (a *Agent) Run(ctx context.Context) error {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	every := a.Interval
	if every <= 0 {
		every = 30 * time.Second
	}
	failing := false
	for {
		if err := a.Send(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if errors.Is(err, ErrRemoved) {
				return err
			}
			log.Warn("could not send report", "err", err)
			failing = true
		} else if failing {
			log.Info("reports reach the core again", "core", a.CoreURL)
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}
