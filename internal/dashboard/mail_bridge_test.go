package dashboard

import (
	"encoding/json"
	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/control"
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
	write("runtime.json", Status{PID: os.Getpid(), Running: true, Authenticated: true})
	family := &control.FamilyInfo{Member: true, Self: 1, Organizers: 1, StorageKnown: true, StorageParticipating: true, StorageAvailable: true, StorageEnabled: true, StorageBytes: 50 << 30}
	if _, err := writeFamilyCache(account, family); err != nil {
		t.Fatal(err)
	}
	if !s.mailBridgeReady(account) {
		t.Fatal("ready account not eligible")
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
	if s.mailBridgeReady(account) {
		t.Fatal("stale cache without completed receipt exported")
	}
	if _, err := writeFamilyCache(account, family); err != nil {
		t.Fatal(err)
	}
	write("runtime.json", Status{PID: os.Getpid(), Running: true, Authenticated: false})
	if s.mailBridgeReady(account) {
		t.Fatal("unauthenticated device exported")
	}
}
