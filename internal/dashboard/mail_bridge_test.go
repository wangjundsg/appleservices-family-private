package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
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
