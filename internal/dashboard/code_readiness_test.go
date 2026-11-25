package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Laky-64/appleservices/internal/control"
)

func TestDegradedDeviceSessionCanGenerateCodeWithoutBecomingHealthy(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{Dir: dir, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.listener.Close()
	worker, err := control.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	worker.SetReady(true)
	state := Status{PID: os.Getpid(), Running: true, PushConnected: true, State: "refresh_failed", RenewalHTTPStatus: 401, AutomaticRefreshPaused: true}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	view := s.snapshot(context.Background())
	if !view.CodeReady || !view.ControlReady || view.Status.SessionVerified || view.Status.Authenticated || view.Status.RenewalHTTPStatus != 401 {
		t.Fatal("code ability was coupled to device health")
	}
	go func() {
		cmd := <-worker.Commands
		cmd.Reply <- control.Result{OK: cmd.Action.Action == "code", Code: "123456"}
	}()
	r := httptest.NewRequest("POST", s.URL()+"/api/action", strings.NewReader(`{"action":"code"}`))
	r.Header.Set("X-App-Token", s.token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var result control.Result
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.OK || result.Code != "123456" {
		t.Fatal("manual code proxy failed")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if string(after) != string(data) {
		t.Fatal("code generation changed device health")
	}
	// A legacy worker must not silently promise this independent capability.
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/action" {
			t.Error("legacy code action was forwarded")
		}
		json.NewEncoder(w).Encode(control.View{Status: data, CodeReady: true, Capabilities: []string{"health-v2"}})
	}))
	defer legacy.Close()
	record, _ := json.Marshal(control.Record{PID: os.Getpid(), URL: legacy.URL, Token: worker.Record.Token})
	os.WriteFile(filepath.Join(dir, "control.json"), record, 0600)
	if s.snapshot(context.Background()).CodeReady {
		t.Fatal("legacy readiness was trusted")
	}
	r = httptest.NewRequest("POST", s.URL()+"/api/action", strings.NewReader(`{"action":"code"}`))
	r.Header.Set("X-App-Token", s.token)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "code_provider_unavailable") {
		t.Fatal("legacy code action not rejected")
	}
}
