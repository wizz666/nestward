package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCAIsStable(t *testing.T) {
	dir := t.TempDir()
	a, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Cert) != Fingerprint(b.Cert) || !strings.HasPrefix(Fingerprint(a.Cert), "sha256:") {
		t.Error("the CA must not change between starts")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "ca.key"))
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("ca.key mode %v", fi.Mode().Perm())
		}
	}
}

func TestSignAgent(t *testing.T) {
	ca, _ := EnsureCA(t.TempDir())
	keyPEM, csrPEM, err := NewAgentKey("pve")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, err := ca.SignAgent(csrPEM, "pve")
	if err != nil || serial == "" {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(blk.Bytes)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("agent cert does not verify: %v", err)
	}
	if cert.Subject.CommonName != "pve" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("key and cert do not match: %v", err)
	}
	if _, _, err := ca.SignAgent([]byte("junk"), "pve"); err == nil {
		t.Error("junk request accepted")
	}
}

// Full handshake: pinned enrollment, then mutual TLS.
func TestHandshakes(t *testing.T) {
	ca, _ := EnsureCA(t.TempDir())
	srvCert, err := ca.ServerCert()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.VerifiedChains) > 0 {
			io.WriteString(w, "client:"+r.TLS.VerifiedChains[0][0].Subject.CommonName)
			return
		}
		io.WriteString(w, "anonymous")
	}))
	srv.TLS = ca.ServerConfig(srvCert)
	srv.StartTLS()
	defer srv.Close()

	get := func(cfg *tls.Config) (string, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}

	if got, err := get(PinnedConfig(Fingerprint(ca.Cert))); err != nil || got != "anonymous" {
		t.Errorf("pinned: %q %v", got, err)
	}
	other, _ := EnsureCA(t.TempDir())
	if _, err := get(PinnedConfig(Fingerprint(other.Cert))); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("wrong fingerprint must fail: %v", err)
	}

	keyPEM, csrPEM, _ := NewAgentKey("pve")
	certPEM, _, _ := ca.SignAgent(csrPEM, "pve")
	cfg, err := AgentConfig(ca.PEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := get(cfg); err != nil || got != "client:pve" {
		t.Errorf("mutual TLS: %q %v", got, err)
	}

	// A certificate from another CA is refused by the core.
	ok, csr2, _ := NewAgentKey("evil")
	bad, _, _ := other.SignAgent(csr2, "evil")
	cfg2, _ := AgentConfig(ca.PEM, bad, ok)
	if _, err := get(cfg2); err == nil {
		t.Error("certificate from a foreign CA accepted")
	}
}
