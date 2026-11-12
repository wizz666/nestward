package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendJSONWithToken(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	n := Ntfy{URL: srv.URL + "/", Topic: "larm", Token: "tk_abc"}
	err := n.Send(context.Background(), Message{Title: "Plex på Tower svarar inte", Body: "Sedan 12:04", Priority: 4, Tags: []string{"warning"}})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tk_abc" || path != "/" {
		t.Errorf("auth %q path %q", auth, path)
	}
	if got["topic"] != "larm" || got["title"] != "Plex på Tower svarar inte" || got["priority"] != float64(4) {
		t.Errorf("body = %v", got)
	}
}

func TestSendErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	err := Ntfy{URL: srv.URL, Topic: "larm"}.Send(context.Background(), Message{Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v", err)
	}
	if err := (Ntfy{URL: srv.URL}).Send(context.Background(), Message{Body: "x"}); err == nil {
		t.Error("missing topic must fail")
	}
}
