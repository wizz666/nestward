package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultGateway(t *testing.T) {
	dir := t.TempDir()
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010011AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	p := filepath.Join(dir, "route")
	if err := os.WriteFile(p, []byte(route), 0o600); err != nil {
		t.Fatal(err)
	}
	if gw := defaultGateway(p); gw != "172.17.0.1" {
		t.Errorf("gateway = %q, want 172.17.0.1", gw)
	}

	noDefault := "Iface\tDestination\tGateway \tFlags\n" + "eth0\t000011AC\t00000000\t0001\n"
	if err := os.WriteFile(p, []byte(noDefault), 0o600); err != nil {
		t.Fatal(err)
	}
	if gw := defaultGateway(p); gw != "" {
		t.Errorf("no default route: gateway = %q", gw)
	}
	if gw := defaultGateway(filepath.Join(dir, "missing")); gw != "" {
		t.Errorf("missing file: gateway = %q", gw)
	}
}
