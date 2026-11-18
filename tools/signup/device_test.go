package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeviceRoundTripRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	device, err := loadDevice(dir, "dev-1", now)
	if err != nil {
		t.Fatalf("load fresh: %v", err)
	}
	if device.ID != "dev-1" || !device.CreatedAt.Equal(now.UTC()) {
		t.Fatalf("fresh device = %+v", device)
	}
	device.noteVisit([]string{"/account"}, now.Add(2*time.Hour))
	if err := device.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := loadDevice(dir, "dev-1", now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Visits != 1 || len(reloaded.History) != 1 {
		t.Fatalf("reloaded device lost the visit: %+v", reloaded)
	}
	if got := reloaded.ageDays(now.Add(48 * time.Hour)); got != 2 {
		t.Fatalf("ageDays = %d, want 2", got)
	}

	// A file for another id, or a corrupt file, must not be silently replaced.
	path, _ := devicePath(dir, "dev-1")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDevice(dir, "dev-1", now); err == nil {
		t.Fatal("corrupt device state must be refused")
	}
	if _, err := loadDevice(dir, "../escape", now); err == nil {
		t.Fatal("device ids must be restricted to a safe character set")
	}
}

func TestDeviceWarmsCookiesIntoSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	device, _ := loadDevice(dir, "dev-cookie", now)
	device.Cookies = []cookieEvent{{
		URL: "https://appleid.apple.com/",
		Cookies: []*http.Cookie{
			{Name: "dslang", Value: "US-EN", Path: "/", Domain: "appleid.apple.com"},
			{Name: "site", Value: "USA", Path: "/", Domain: "appleid.apple.com"},
		},
	}}

	session, err := newSession(SessionOptions{Base: "https://appleid.apple.com", Locale: "en-US", Timezone: "America/New_York"})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	loaded, err := device.warmSession(session)
	if err != nil {
		t.Fatalf("warmSession: %v", err)
	}
	if loaded != 2 {
		t.Fatalf("loaded %d cookies, want 2", loaded)
	}
	jar, ok := session.client.Jar.(*recordedJar)
	if !ok {
		t.Fatalf("session jar is %T, want the recording jar", session.client.Jar)
	}
	if len(jar.Events) != 1 {
		t.Fatalf("journal events = %d, want 1", len(jar.Events))
	}
	for _, cookie := range jar.Cookies(mustURL(t, "https://appleid.apple.com/")) {
		if cookie.Name == "dslang" && cookie.Value == "US-EN" {
			return
		}
	}
	t.Fatal("restored cookie is not readable from the jar")
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return parsed
}

func TestDeviceReceiptHasNoCookieValues(t *testing.T) {
	now := time.Now()
	device := &deviceState{
		SchemaVersion: 1, ID: "dev-1", CreatedAt: now.Add(-72 * time.Hour),
		Visits: 3, Tasks: []string{"a@b.c"},
		Cookies: []cookieEvent{{URL: "https://appleid.apple.com/", Cookies: []*http.Cookie{{Name: "dslang", Value: "SECRET-VALUE"}}}},
	}
	raw, _ := json.Marshal(deviceReceipt(device, now, 2, true))
	if strings.Contains(string(raw), "SECRET-VALUE") {
		t.Fatalf("receipt leaks a cookie value: %s", raw)
	}
	for _, key := range []string{"ageDays", "visits", "cookiesLoaded", "profileReused"} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("receipt lost %s: %s", key, raw)
		}
	}
}

func TestDeviceStateSurvivesAcrossCLIRuns(t *testing.T) {
	server := httptest.NewServer(deviceFixture(t))
	defer server.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "task-1.json")
	deviceDir := filepath.Join(dir, "devices")
	args := []string{
		"-email", "fixture@example.test",
		"-stage", "preflight", "-json",
		"-profile-mode", "new", "-profile", "chrome",
		"-egress-country", "US", "-egress-source", "explicit",
		"-device", "dev-cli", "-device-dir", deviceDir,
		"-state", statePath,
	}
	code, receipt := invokeSignup(t, statePath, server.URL, args...)
	if code != 0 {
		t.Fatalf("first run exit = %d (%v)", code, receipt)
	}
	first, _ := receipt["device"].(map[string]any)
	if first == nil {
		t.Fatalf("first receipt has no device block: %v", receipt)
	}
	firstProfile := ""
	if profile, ok := receipt["profile"].(map[string]any); ok {
		firstProfile, _ = profile["id"].(string)
	}
	if firstProfile == "" {
		t.Fatalf("first run did not select a profile: %v", receipt)
	}

	// Second task on the same device: same fingerprint, history carried over.
	statePath2 := filepath.Join(dir, "task-2.json")
	args2 := append([]string{}, args...)
	for i := range args2 {
		if args2[i] == statePath {
			args2[i] = statePath2
		}
	}
	code, receipt = invokeSignup(t, statePath2, server.URL, args2...)
	if code != 0 {
		t.Fatalf("second run exit = %d (%v)", code, receipt)
	}
	second, _ := receipt["device"].(map[string]any)
	if second == nil {
		t.Fatalf("second receipt has no device block: %v", receipt)
	}
	if visits, _ := second["visits"].(float64); visits < 2 {
		t.Fatalf("device visits = %v, want at least 2", second["visits"])
	}
	if reused, _ := second["profileReused"].(bool); !reused {
		t.Fatalf("second task did not reuse the device profile: %v", second)
	}
	profile2, _ := receipt["profile"].(map[string]any)
	if id, _ := profile2["id"].(string); id != firstProfile {
		t.Fatalf("device profile changed between tasks: %s != %s", id, firstProfile)
	}
}

// deviceFixture plays the happy path and tolerates a second visit, because a
// warmed device legitimately re-bootstraps under an existing cookie.
func deviceFixture(t *testing.T) http.Handler {
	var mu sync.Mutex
	scnt, session := "dev-1", "dev-session"
	phase := ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("scnt", scnt)
		w.Header().Set("X-Apple-ID-Session-Id", session)
		switch {
		case r.URL.Path == "/bootstrap/portal":
			fmt.Fprint(w, `{"serviceKey":"fixture-widget"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/account":
			http.SetCookie(w, &http.Cookie{Name: "bound", Value: session, Path: "/", HttpOnly: true})
		case r.URL.Path == "/account/validation/appleid":
			fmt.Fprintf(w, `{"valid":true,"used":%t}`, phase == "created")
		case r.URL.Path == "/account/validate/password":
			w.WriteHeader(204)
		case r.URL.Path == "/captcha":
			fmt.Fprintf(w, `{"id":1,"token":"t","payload":{"content":%q}}`, base64.StdEncoding.EncodeToString([]byte("img")))
		case r.URL.Path == "/account/validate":
			phase = "validated"
			fmt.Fprint(w, `{}`)
		case r.URL.Path == "/account/verification" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"verificationId":"v-1"}`)
		case r.URL.Path == "/account/verification":
			fmt.Fprint(w, `{}`)
		case r.URL.Path == "/account/verification/phone":
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
}
