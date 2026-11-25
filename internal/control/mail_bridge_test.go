package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBridgeCredentialsRequireCapabilityReadinessAndPost(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetBridgeCredentials("fixture@example.test", "fixture-password")
	for _, tc := range []struct {
		method, token string
		ready         bool
		want          int
	}{
		{"POST", "", true, 403}, {"GET", s.Record.Token, true, 404},
		{"POST", s.Record.Token, false, 409}, {"POST", s.Record.Token, true, 200},
	} {
		s.SetReady(tc.ready)
		r := httptest.NewRequest(tc.method, s.Record.URL+"/bridge-credentials", nil)
		r.Header.Set("X-Control-Token", tc.token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("status %d want %d", w.Code, tc.want)
		}
		if (w.Code == 200) != strings.Contains(w.Body.String(), "fixture-password") {
			t.Fatal("credential exposure boundary failed")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("bridge response cacheable")
		}
	}
}
