package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/hardware"
)

func TestSaveAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SaveMachine(ctx, "agent", "192.168.1.20", hardware.Report{MachineID: "b", Hostname: "beta"}, &discovery.Result{Available: true, Services: []discovery.Service{{Name: "plex", Kind: "app"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMachine(ctx, "local", "", hardware.Report{MachineID: "a", Hostname: "zeta", CPU: hardware.CPUInfo{Cores: 4}}, nil); err != nil {
		t.Fatal(err)
	}
	// Update replaces the row instead of adding one.
	if err := s.SaveMachine(ctx, "local", "", hardware.Report{MachineID: "a", Hostname: "zeta", CPU: hardware.CPUInfo{Cores: 8}}, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopen: migrations must be idempotent and data kept.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ms, err := s.Machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "a" || ms[0].Hardware.CPU.Cores != 8 || ms[1].Hostname != "beta" {
		t.Errorf("machines = %+v", ms)
	}
	if ms[0].Discovery != nil || ms[1].Discovery == nil || ms[1].Discovery.Services[0].Name != "plex" || ms[1].Address != "192.168.1.20" {
		t.Errorf("discovery/address not kept: %+v / %+v", ms[0], ms[1])
	}
	if err := s.SaveMachine(ctx, "agent", "", hardware.Report{}, nil); err == nil {
		t.Error("empty machine id must be rejected")
	}
}
