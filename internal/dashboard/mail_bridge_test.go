package dashboard

import (
	"encoding/json"
	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/control"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMailBridgeRejectsUnauthenticatedAndUnreadyAccount(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.listener.Close()
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/api/mail-bridge/accounts", "", 403},
		{"/api/mail-bridge/credentials?account=missing", s.token, 409},
		{"/api/mail-bridge/accounts", s.token, 200},
	} {
		r := httptest.NewRequest("POST", s.URL()+tc.path, strings.NewReader("{}"))
		r.Header.Set("X-App-Token", tc.token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("status %d want %d", w.Code, tc.want)
		}
		if strings.Contains(w.Body.String(), "password") {
			t.Fatal("listing/error exposed credential field")
		}
	}
}

func TestMailBridgeOnlyExportsReadyFamilyAccounts(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{ID: "01-account", AppleID: "ready@example.test", Dir: dir}
	s := &Server{config: Config{Worker: executable}, accounts: []*Account{account}}
	saved := appleservices.Session{AppleID: account.AppleID, DSID: "fixture-gsa"}
	accepted := acceptedICloudCheckpoint{Session: appleservices.Session{AppleID: account.AppleID, DSID: "fixture-icloud", Tokens: map[string]string{"mmeAuthToken": "fixture"}}, ADSID: saved.DSID, ServiceData: map[string]any{}}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("session.json", saved)
	write("icloud-accepted.json", accepted)
	controller, err := control.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	controller.SetBridgeCredentials(account.AppleID, "fixture-password")
	controller.SetReady(true)
	write("runtime.json", Status{PID: os.Getpid(), Running: true, Authenticated: true, State: "online", LastDeviceQueryAt: time.Now()})
	if _, err := writeStorageCache(account, &icloud.StorageInfo{TotalBytes: 50 << 30}); err != nil {
		t.Fatal(err)
	}
	family := &control.FamilyInfo{Member: true, Self: 1, Organizers: 1, StorageKnown: true, StorageParticipating: true, StorageAvailable: true, StorageEnabled: true, StorageBytes: 50 << 30}
	if _, err := writeFamilyCache(account, family); err != nil {
		t.Fatal(err)
	}
	if !s.mailBridgeReady(account) {
		t.Fatal("ready account not eligible")
	}
	// A live legacy worker with cached healthy data still cannot promise new IPC.
	originalRecord, _ := os.ReadFile(filepath.Join(dir, "control.json"))
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))
		json.NewEncoder(w).Encode(control.View{Status: data, CodeReady: true})
	}))
	write("control.json", control.Record{PID: os.Getpid(), URL: legacy.URL, Token: controller.Record.Token})
	if ready, reason := s.mailBridgeStatus(account); ready || reason != "worker_upgrade_required" {
		t.Fatal("legacy capabilities accepted")
	}
	legacy.Close()
	if err := os.WriteFile(filepath.Join(dir, "control.json"), originalRecord, 0600); err != nil {
		t.Fatal(err)
	}

	family.StorageParticipating = false
	if _, err := writeFamilyCache(account, family); err != nil {
		t.Fatal(err)
	}
	if s.mailBridgeReady(account) {
		t.Fatal("account without sharing exported")
	}
	family.StorageParticipating = true
	write("family-status.json", familyCacheRecord{Version: familyCacheVersion, AccountID: account.ID, CheckedAt: time.Now().Add(-time.Hour), Family: *family})
	write("cn-family-job.json", cnFamilyJob{Account: account.ID, Organizer: "mother", State: "completed"})
	if s.mailBridgeReady(account) {
		t.Fatal("stale cache with historical receipt exported")
	}
	if _, err := writeFamilyCache(account, family); err != nil {
		t.Fatal(err)
	}
	write("storage-status.json", storageCacheRecord{Version: 1, AccountID: account.ID, CheckedAt: time.Now().Add(-time.Hour), Storage: &icloud.StorageInfo{TotalBytes: 50 << 30}})
	if s.mailBridgeReady(account) {
		t.Fatal("stale quota exported")
	}
	writeStorageCache(account, &icloud.StorageInfo{TotalBytes: 50 << 30})
	write("runtime.json", Status{PID: os.Getpid(), Running: true, Authenticated: false})
	if s.mailBridgeReady(account) {
		t.Fatal("unauthenticated device exported")
	}
}
