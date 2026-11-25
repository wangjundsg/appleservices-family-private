package control

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProtectionAndExpiryRejectBeforeDispatch(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetReady(true)
	s.SetPending(&Pending{ID: "fixture", ReceivedAt: time.Now().Add(-PendingMaxAge - time.Second)})
	request := func(path, body string) int {
		r := httptest.NewRequest("POST", s.Record.URL+path, strings.NewReader(body))
		r.Header.Set("X-Control-Token", s.Record.Token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("/action", `{"action":"allow","request_id":"fixture"}`); got != 409 {
		t.Fatalf("expired request got %d", got)
	}
	os.WriteFile(filepath.Join(dir, "account-guard.json"), []byte(`{"version":1,"blocked":true}`), 0600)
	for _, action := range []string{"code", "refresh", "validate-account", "allow"} {
		if got := request("/action", `{"action":"`+action+`"}`); got != 423 {
			t.Fatalf("protected %s got %d", action, got)
		}
	}
	if got := request("/bridge-credentials", `{}`); got != 423 {
		t.Fatalf("protected bridge got %d", got)
	}
}

func TestSessionFreshNeverUsesHistoricalAuthenticationAlone(t *testing.T) {
	now := time.Now()
	for _, state := range []string{"refresh_failed", "device_query_failed", "account_locked"} {
		if SessionFresh(state, true, now, now) {
			t.Fatal("failed state accepted")
		}
	}
	if SessionFresh("online", true, now.Add(-time.Hour), now) || SessionFresh("online", true, time.Time{}, now) || SessionFresh("online", true, now.Add(time.Minute), now) {
		t.Fatal("unverified timestamps accepted")
	}
	if !SessionFresh("online", true, now, now) {
		t.Fatal("fresh check rejected")
	}
}
