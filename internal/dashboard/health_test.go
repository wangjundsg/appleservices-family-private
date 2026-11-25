package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

func TestProtectedAccountRejectsStartActionsAndBackground(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "child")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "account-guard.json"), []byte(`{"version":1,"blocked":true}`), 0600)
	s, err := New(Config{Dir: root, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.listener.Close()
	account := &Account{ID: "child", AppleID: "fixture@example.test", Dir: dir}
	s.accounts = []*Account{account}
	for _, resume := range []bool{true, false} {
		if result := s.startInMode(context.Background(), dir, credentials{AppleID: account.AppleID, Password: "fixture"}, account, resume); result.Stage != "account_protected" {
			t.Fatalf("start escaped gate: %+v", result)
		}
	}
	for _, path := range []string{"/api/action?account=child", "/api/cn-family"} {
		r := httptest.NewRequest("POST", s.URL()+path, strings.NewReader(`{"account":"child","action":"code","step":"activate"}`))
		if strings.Contains(path, "/api/cn-family") {
			r = httptest.NewRequest("POST", s.URL()+path, strings.NewReader(`{"account":"child","step":"activate"}`))
		}
		r.Header.Set("X-App-Token", s.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if !strings.Contains(w.Body.String(), "account_protected") {
			t.Fatalf("action escaped gate: %s", w.Body.String())
		}
	}
	result := s.queueCNFamily(account, "mother", false)
	if result["reason"] != "account_protected" {
		t.Fatal("background gate missing")
	}
	var output control.Result
	if s.callFor(context.Background(), dir, "/action", control.Action{Action: "refresh"}, &output) == nil {
		t.Fatal("IPC bypassed guard")
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.out.log")); !os.IsNotExist(err) {
		t.Fatal("protected start touched runtime")
	}
	if ready, reason := s.mailBridgeStatus(account); ready || reason != "account_protected" {
		t.Fatal("bridge gate missing")
	}
}

func TestHealthNormalizationPreservesLivenessButNotStaleTrust(t *testing.T) {
	dir := t.TempDir()
	status := Status{Running: true, PushConnected: true, Authenticated: true, State: "refresh_failed", LastDeviceQueryAt: time.Now()}
	got := normalizeHealth(dir, status)
	if !got.Running || !got.PushConnected || got.Authenticated || got.SessionVerified || got.Protected {
		t.Fatal("liveness conflated with trust or lock")
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "blocked": true})
	os.WriteFile(filepath.Join(dir, "account-guard.json"), data, 0600)
	status.State = "online"
	got = normalizeHealth(dir, status)
	if got.State != "account_locked" || !got.Protected || got.SessionVerified {
		t.Fatal("live status overwrote guard")
	}
}

func TestWorker404ExplainsCapabilityMismatch(t *testing.T) {
	if !strings.Contains(workerHTTPError(404, nil, nil).Error(), "版本") {
		t.Fatal("404 lost capability diagnosis")
	}
}
