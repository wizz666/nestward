package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wizz666/nestward/internal/agent"
	"github.com/wizz666/nestward/internal/pki"
	"github.com/wizz666/nestward/internal/store"
)

// AgentGate is the encrypted door for agents: a TLS port where agents
// enroll with a one-time code and then report with their own certificate.
type AgentGate struct {
	CA    *pki.CA
	Port  int          // the agent port, shown in the add-machine command
	Store *store.Store // where codes live; nil keeps them in memory (tests)

	mu    sync.Mutex
	codes map[string]time.Time // code -> expiry, only without a Store
	fails []time.Time
}

// codeLife is how long an add-machine code works.
const codeLife = 30 * time.Minute

// codeHash is how a code is kept: never the code itself.
func codeHash(code string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

// makeCode returns a fresh random code like 1A2B-3C4D-5E6F.
func makeCode() string {
	b := make([]byte, 6)
	rand.Read(b)
	s := strings.ToUpper(hex.EncodeToString(b))
	return s[:4] + "-" + s[4:8] + "-" + s[8:]
}

// NewAgentCode makes a one-time add-machine code and stores it. It is
// used by the web page and by `nestward agent-code` on the core's machine.
func NewAgentCode(ctx context.Context, st *store.Store) (string, time.Time, error) {
	code := makeCode()
	exp := time.Now().Add(codeLife)
	return code, exp, st.AddAgentCode(ctx, codeHash(code), exp)
}

// AgentCommands are the ready-to-paste commands for adding a machine.
func AgentCommands(core, code, fingerprint string) (program, docker string) {
	args := "agent --core " + core + " --code " + code + " --fingerprint " + fingerprint
	return "nestward " + args,
		"docker run -d --name nestward-agent --restart unless-stopped " +
			"-v nestward-agent:/data -v /:/host:ro -v /var/run/docker.sock:/var/run/docker.sock:ro " +
			"-e NESTWARD_DATA=/data -e NESTWARD_HOST_ROOT=/host nestward " + args
}

// NewCode makes a one-time enrollment code.
func (g *AgentGate) NewCode() (string, time.Time) {
	if g.Store != nil {
		code, exp, err := NewAgentCode(context.Background(), g.Store)
		if err != nil {
			return "", time.Time{}
		}
		return code, exp
	}
	code := makeCode()
	exp := time.Now().Add(codeLife)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.codes == nil {
		g.codes = map[string]time.Time{}
	}
	for c, e := range g.codes {
		if time.Now().After(e) {
			delete(g.codes, c)
		}
	}
	g.codes[codeHash(code)] = exp
	return code, exp
}

// useCode spends a code; false if unknown, used or expired. Too many
// wrong codes close the door for a while.
func (g *AgentGate) useCode(code string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	recent := g.fails[:0]
	for _, f := range g.fails {
		if now.Sub(f) < 15*time.Minute {
			recent = append(recent, f)
		}
	}
	g.fails = recent
	if len(g.fails) >= 10 {
		return false
	}
	h := codeHash(code)
	ok := false
	if g.Store != nil {
		ok, _ = g.Store.UseAgentCode(context.Background(), h)
	} else if exp, found := g.codes[h]; found && now.Before(exp) {
		delete(g.codes, h)
		ok = true
	}
	if !ok {
		g.fails = append(g.fails, now)
	}
	return ok
}

// maxAgentBody limits an agent report's size.
const maxAgentBody = 1 << 20

// AgentHandler serves the agent port.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agent/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/v1/agent/report", s.handleAgentReport)
	return mux
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code      string `json:"code"`
		MachineID string `json:"machine_id"`
		Hostname  string `json:"hostname"`
		CSR       string `json:"csr"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	if !s.Agents.useCode(in.Code) {
		s.logger().Warn("agent enrollment refused: bad code", "ip", clientIP(r))
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_code"})
		return
	}
	if in.MachineID == "" || len(in.MachineID) > 128 || len(in.Hostname) > 255 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_machine"})
		return
	}
	// An agent must never take the core's own machine identity.
	if s.Detect != nil && in.MachineID == s.Local(r.Context()).Hardware.MachineID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "same_as_core"})
		return
	}
	certPEM, serial, err := s.Agents.CA.SignAgent([]byte(in.CSR), in.MachineID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_csr"})
		return
	}
	if err := s.Store.AddAgentCert(r.Context(), store.AgentCert{Serial: serial, MachineID: in.MachineID, Hostname: in.Hostname}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	s.logger().Info("agent enrolled", "host", in.Hostname, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"cert": string(certPEM), "ca": string(s.Agents.CA.PEM)})
}

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "certificate_required"})
		return
	}
	leaf := r.TLS.VerifiedChains[0][0]
	machineID := leaf.Subject.CommonName
	ok, err := s.Store.AgentCertValid(r.Context(), leaf.SerialNumber.Text(16), machineID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "certificate_revoked"})
		return
	}
	var p agent.Payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentBody)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_report"})
		return
	}
	// The certificate, not the report, says which machine this is.
	if p.Hardware.MachineID != "" && p.Hardware.MachineID != machineID {
		s.logger().Warn("agent reported as another machine", "cert", machineID, "claimed", p.Hardware.MachineID)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong_machine"})
		return
	}
	p.Hardware.MachineID = machineID
	if len(p.Hardware.Hostname) > 255 || (p.Discovery != nil && len(p.Discovery.Services) > 2000) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_report"})
		return
	}
	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		addr = ""
	}
	if err := s.Store.SaveMachine(r.Context(), "agent", addr, p.Hardware, p.Discovery); err != nil {
		s.logger().Error("save agent report", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// registerAgents adds the add-machine endpoints to the (logged-in) web API.
func (s *Server) registerAgents(mux *http.ServeMux) {
	if s.Agents == nil {
		return
	}
	mux.HandleFunc("POST /api/v1/agents/code", func(w http.ResponseWriter, r *http.Request) {
		code, exp := s.Agents.NewCode()
		if code == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		core := "https://" + host + ":" + strconv.Itoa(s.Agents.Port)
		fp := pki.Fingerprint(s.Agents.CA.Cert)
		program, docker := AgentCommands(core, code, fp)
		writeJSON(w, http.StatusOK, map[string]any{
			"code": code, "expires": exp, "core": core, "fingerprint": fp,
			"command": program, "docker": docker,
		})
	})
	mux.HandleFunc("GET /api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Store.AgentCerts(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("DELETE /api/v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.Store.RemoveAgent(r.Context(), id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		s.logger().Info("agent removed", "machine", id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
	})
}
