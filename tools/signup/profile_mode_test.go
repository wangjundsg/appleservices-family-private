package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/engine"
	"github.com/Laky-64/appleservices/internal/fingerprint"
)

func newProfileSession(t *testing.T) *Session {
	t.Helper()
	s, err := newSession(SessionOptions{Base: "https://127.0.0.1:1", Locale: "en-US", Timezone: "America/New_York", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	return s
}

func newProfileState(email string) *state {
	return &state{Version: 1, Email: email, Stage: "", Status: "", Operations: map[string]operation{}}
}

func TestProfileIsFrozenAndResumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.json")
	st := newProfileState("p@example.com")
	session := newProfileSession(t)

	cfg := profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "verified-egress"}
	profile, engineInfo, err := installProfile(st, session, cfg, "", 5*time.Second)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if profile == nil || st.Profile == nil {
		t.Fatal("profile was not saved into the task state")
	}
	if engineInfo["name"] != "utls" || engineInfo["applied"] != true {
		t.Fatalf("engine receipt = %+v", engineInfo)
	}
	if session.userAgent != profile.UserAgent || session.locale != profile.Locale || session.timezone != profile.Timezone {
		t.Fatal("session identity was not replaced by the profile")
	}
	if _, ok := session.client.Transport.(*engine.Transport); !ok {
		t.Fatalf("session transport is %T, want the engine adapter", session.client.Transport)
	}
	if err := writeState(path, st); err != nil {
		t.Fatalf("write state: %v", err)
	}
	firstID := profile.ID

	// New process: reload from disk and install again.
	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	session2 := newProfileSession(t)
	profile2, _, err := installProfile(reloaded, session2, cfg, "", 5*time.Second)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if profile2.ID != firstID {
		t.Fatalf("resume minted a new profile: %s != %s", profile2.ID, firstID)
	}
	if profile2.TemplateID != profile.TemplateID {
		t.Fatalf("resume changed the template: %s != %s", profile2.TemplateID, profile.TemplateID)
	}
	if session2.userAgent != profile.UserAgent {
		t.Fatal("resumed session lost the profile identity")
	}
}

func TestProfileModeRejectsTamperedProfile(t *testing.T) {
	st := newProfileState("p@example.com")
	session := newProfileSession(t)
	cfg := profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}
	if _, _, err := installProfile(st, session, cfg, "", 5*time.Second); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A template change or hand edit must be caught, not silently accepted.
	st.Profile.UserAgent = "Mozilla/5.0 (X11; Linux x86_64) Chrome/131.0.0.0"
	session2 := newProfileSession(t)
	_, _, err := installProfile(st, session2, cfg, "", 5*time.Second)
	if !errors.Is(err, ErrProfileBlocked) {
		t.Fatalf("tampered profile must be blocked, got %v", err)
	}
}

func TestProfileModeBlocksTaskWithIntentButNoProfile(t *testing.T) {
	st := newProfileState("p@example.com")
	st.Body = map[string]any{"account": map[string]any{"name": "p@example.com"}}
	session := newProfileSession(t)
	cfg := profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}
	_, _, err := installProfile(st, session, cfg, "", 5*time.Second)
	if !errors.Is(err, ErrProfileBlocked) {
		t.Fatalf("a submitted task without a profile must block, got %v", err)
	}
	if st.Profile != nil {
		t.Fatal("a blocked task must not be given a freshly minted profile")
	}
}

func TestProfileModeRejectsRequestedKindMismatch(t *testing.T) {
	st := newProfileState("p@example.com")
	session := newProfileSession(t)
	cfg := profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}
	if _, _, err := installProfile(st, session, cfg, "", 5*time.Second); err != nil {
		t.Fatalf("install: %v", err)
	}
	other := cfg
	other.Kind = "firefox"
	_, _, err := installProfile(st, newProfileSession(t), other, "", 5*time.Second)
	if !errors.Is(err, ErrProfileBlocked) {
		t.Fatalf("changing the profile kind on a task must block, got %v", err)
	}
}

func TestProfileModeRefusesCurlAndChromeFDOnOtherKinds(t *testing.T) {
	st := newProfileState("p@example.com")
	curlSession := newProfileSession(t)
	curlSession.UseCurl = true
	if _, _, err := installProfile(st, curlSession, profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}, "", 5*time.Second); err == nil {
		t.Fatal("curl transport cannot apply an engine hello and must be refused")
	}

	st2 := newProfileState("p@example.com")
	fdSession := newProfileSession(t)
	fdSession.fdClientInfo = `{"U":"x","L":"en-US","Z":"GMT-05:00","V":"1.1","F":"abc"}`
	_, _, err := installProfile(st2, fdSession, profileConfig{Mode: "new", Kind: "firefox", EgressCountry: "US", EgressSource: "explicit"}, "", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "fd-client-info") {
		t.Fatalf("chrome FD on firefox must be refused, got %v", err)
	}
}

func TestProfileModeRequiresRegionInputs(t *testing.T) {
	st := newProfileState("p@example.com")
	_, _, err := installProfile(st, newProfileSession(t), profileConfig{Mode: "new", Kind: "chrome"}, "", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "egress-country") {
		t.Fatalf("missing egress inputs must be rejected, got %v", err)
	}
}

func TestClientHintsFollowTheProfile(t *testing.T) {
	st := newProfileState("p@example.com")
	session := newProfileSession(t)
	if _, _, err := installProfile(st, session, profileConfig{Mode: "new", Kind: "firefox", EgressCountry: "US", EgressSource: "explicit"}, "", 5*time.Second); err != nil {
		t.Fatalf("install firefox: %v", err)
	}
	header, err := session.headers("application/json", false, time.Now())
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if header.Get("Sec-Ch-Ua") != "" || header.Get("Sec-Ch-Ua-Platform") != "" {
		t.Fatalf("firefox session sent chrome client hints: %v", header)
	}

	st2 := newProfileState("p@example.com")
	chrome := newProfileSession(t)
	if _, _, err := installProfile(st2, chrome, profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}, "", 5*time.Second); err != nil {
		t.Fatalf("install chrome: %v", err)
	}
	header2, err := chrome.headers("application/json", false, time.Now())
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if header2.Get("Sec-Ch-Ua") == "" || header2.Get("Sec-Ch-Ua-Platform") != `"Windows"` {
		t.Fatalf("chrome session must send its client hints, got %q %q", header2.Get("Sec-Ch-Ua"), header2.Get("Sec-Ch-Ua-Platform"))
	}
}

func TestProfileReceiptCarriesNoSecrets(t *testing.T) {
	st := newProfileState("p@example.com")
	st.Password = "Super#Secret1"
	session := newProfileSession(t)
	session.fdClientInfo = ""
	if _, _, err := installProfile(st, session, profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}, "socks5://user:pass@127.0.0.1:1", 5*time.Second); err != nil {
		t.Fatalf("install: %v", err)
	}
	receipt := profileReceipt(st.Profile, map[string]any{"name": "utls", "applied": true, "proxySet": true})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"Super#Secret1", "user:pass", "127.0.0.1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("receipt leaks %q: %s", secret, raw)
		}
	}
}

// TestProfileModeRunsTheStateMachineOverTheEngine drives the real CLI against a
// TLS mock: the engine handshake is part of the run, not simulated.
func TestProfileModeRunsTheStateMachineOverTheEngine(t *testing.T) {
	f := &strictSignup{calls: map[string]int{}}
	server := httptest.NewTLSServer(f.handler(t))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	args := []string{
		"-email", "fixture@example.test", "-phone", "4155550134", "-phonecc", "US",
		"-stage", "all", "-json",
		"-profile-mode", "new", "-profile", "chrome",
		"-egress-country", "US", "-egress-source", "verified-egress",
		"-engine-insecure",
		"-captcha-answer", "ABCDE",
		"-state", path,
	}
	code, receipt := invokeSignup(t, path, server.URL, args...)
	if code != 4 {
		t.Fatalf("first run exit = %d (%v), want 4 (waiting for the email code)", code, receipt)
	}
	profile, _ := receipt["profile"].(map[string]any)
	if profile == nil {
		t.Fatalf("receipt has no profile block: %v", receipt)
	}
	if profile["mode"] != "new" || profile["kind"] != "chrome" {
		t.Fatalf("unexpected profile block: %v", profile)
	}
	eng, _ := profile["engine"].(map[string]any)
	if eng == nil || eng["name"] != "utls" || eng["applied"] != true {
		t.Fatalf("engine was not reported as applied: %v", profile["engine"])
	}

	saved, err := loadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if saved.Profile == nil {
		t.Fatal("profile was not persisted with the task")
	}
	if err := fingerprint.Validate(*saved.Profile, engine.Capabilities()); err != nil {
		t.Fatalf("persisted profile does not validate: %v", err)
	}

	dir := t.TempDir()
	emailFile := filepath.Join(dir, "email.txt")
	phoneFile := filepath.Join(dir, "phone.txt")

	common := []string{"-email", "fixture@example.test", "-stage", "all", "-json",
		"-profile-mode", "new", "-profile", "chrome",
		"-egress-country", "US", "-egress-source", "verified-egress",
		"-engine-insecure", "-state", path}

	if err := os.WriteFile(emailFile, []byte("012345"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, receipt = invokeSignup(t, path, server.URL, append(append([]string{}, common...), "-email-code-file", emailFile, "-phone-code-file", phoneFile)...)
	if code != 4 {
		t.Fatalf("email step exit = %d (%v), want 4 (phone code)", code, receipt)
	}
	if err := os.WriteFile(phoneFile, []byte("065432"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, receipt = invokeSignup(t, path, server.URL, append(append([]string{}, common...), "-email-code-file", emailFile, "-phone-code-file", phoneFile)...)
	if code != 0 {
		t.Fatalf("final exit = %d (%v), want 0", code, receipt)
	}
	if receipt["accountExists"] != true {
		t.Fatalf("final receipt does not prove the account: %v", receipt)
	}
	resumed, _ := receipt["profile"].(map[string]any)
	if resumed["id"] != profile["id"] {
		t.Fatalf("resume changed the profile id: %v != %v", resumed["id"], profile["id"])
	}
}

// TestNewTaskDefaultsToProfileModeWithoutTheFlag pins the migration rule: a
// fresh task that supplies egress inputs gets the engine, an existing task does
// not get re-profiled behind its back.
func TestNewTaskDefaultsToProfileModeWithoutTheFlag(t *testing.T) {
	f := &strictSignup{calls: map[string]int{}}
	server := httptest.NewTLSServer(f.handler(t))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	code, receipt := invokeSignup(t, path, server.URL,
		"-email", "fixture@example.test",
		"-phone", "4155550134", "-phonecc", "US",
		"-stage", "all", "-json",
		"-egress-country", "US", "-egress-source", "explicit",
		"-engine-insecure", "-captcha-answer", "ABCDE",
		"-state", path)
	if code != 4 {
		t.Fatalf("exit = %d (%v), want 4", code, receipt)
	}
	profile, _ := receipt["profile"].(map[string]any)
	if profile == nil || profile["mode"] != "new" {
		t.Fatalf("a fresh task with egress inputs should default to the profile engine: %v", receipt)
	}

	// A second task without egress inputs stays legacy. Legacy mode uses the
	// stock transport, so it needs a plain-HTTP fixture (the TLS one is only
	// reachable through the engine adapter's -engine-insecure switch).
	f2 := &strictSignup{calls: map[string]int{}}
	plain := httptest.NewServer(f2.handler(t))
	defer plain.Close()
	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	code, receipt = invokeSignup(t, legacyPath, plain.URL,
		"-email", "fixture@example.test",
		"-phone", "4155550134", "-phonecc", "US",
		"-stage", "all", "-json", "-captcha-answer", "ABCDE", "-state", legacyPath)
	if code != 4 {
		t.Fatalf("legacy exit = %d (%v), want 4", code, receipt)
	}
	if legacy, _ := receipt["profile"].(map[string]any); legacy["mode"] != "legacy" {
		t.Fatalf("a task without egress inputs must stay legacy: %v", receipt)
	}
}
