package tailnet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteDevice(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/nGONE"):
			w.WriteHeader(404)
		case strings.HasSuffix(r.URL.Path, "/nDENIED"):
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"API token invalid"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := DeleteDevice(ctx, srv.URL, "tskey-api-x", "n123CNTRL"); err != nil {
		t.Fatal(err)
	}
	if got != "DELETE /api/v2/device/n123CNTRL Bearer tskey-api-x" {
		t.Errorf("request = %q", got)
	}
	if err := DeleteDevice(ctx, srv.URL, "k", "nGONE"); err != nil {
		t.Errorf("already gone must count as done: %v", err)
	}
	if err := DeleteDevice(ctx, srv.URL, "k", "nDENIED"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("denied: %v", err)
	}
}
