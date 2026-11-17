package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/secrets"
)

func TestShareMachines(t *testing.T) {
	s, st := newServer(t)
	box, _ := secrets.OpenBox(filepath.Join(t.TempDir(), "k"))
	s.Settings = &config.Settings{Store: st, Box: box}
	get := func(key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/share/machines", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := get("x"); rec.Code != 404 {
		t.Fatalf("off: status %d", rec.Code)
	}
	key := strings.Repeat("k", 32)
	if err := s.Settings.Set(context.Background(), config.ShareToken, key); err != nil {
		t.Fatal(err)
	}
	if rec := get("wrong-key-wrong-key-wrong-key-1"); rec.Code != 401 {
		t.Fatalf("wrong key: status %d", rec.Code)
	}
	rec := get(key)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Machines []ShareMachine `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Machines) != 1 || out.Machines[0].Name != "core" || !out.Machines[0].Local ||
		out.Machines[0].MemTotal != 32<<30 || out.Machines[0].DiskFree != 300<<30 || !out.Machines[0].Online {
		t.Fatalf("unexpected: %+v", out.Machines)
	}
	if out.Machines[0].Storage == nil {
		t.Fatal("storage must be an empty list, not null")
	}
	if strings.Contains(rec.Body.String(), "address") {
		t.Fatal("must not contain addresses")
	}
}
