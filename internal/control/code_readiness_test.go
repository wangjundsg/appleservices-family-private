package control

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeReadinessIndependentOfDeviceQuery(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, _ := json.Marshal(map[string]any{"pid": s.Record.PID, "running": true, "state": "refresh_failed", "authenticated": false, "renewal_http_status": 401})
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	read := func() View {
		t.Helper()
		r := httptest.NewRequest("GET", s.Record.URL+"/view", nil)
		r.Header.Set("X-Control-Token", s.Record.Token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var v View
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("view failed")
		}
		return v
	}
	if read().CodeReady {
		t.Fatal("uninitialized provider advertised ready")
	}
	r := httptest.NewRequest("POST", s.Record.URL+"/action", strings.NewReader(`{"action":"code"}`))
	r.Header.Set("X-Control-Token", s.Record.Token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "code_provider_unavailable") {
		t.Fatal("uninitialized provider accepted an action")
	}
	s.SetReady(true)
	if !read().CodeReady {
		t.Fatal("401 incorrectly disabled local code generation")
	}
	if err := os.WriteFile(filepath.Join(dir, "account-guard.json"), []byte(`{"version":1,"blocked":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if read().CodeReady {
		t.Fatal("protected account advertised code generation")
	}
}
