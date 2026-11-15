package main

// Boundary checks raised by review. Each one pins a failure mode the happy-path
// tests would not catch.

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2. A resume that omits -profile-mode must keep the task's engine. The old
// default silently fell back to legacy while the receipt still said utls.
func TestResumeKeepsProfileModeFromState(t *testing.T) {
	f := &strictSignup{calls: map[string]int{}}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	code, first := invokeSignup(t, path, server.URL,
		"-email", "fixture@example.test", "-stage", "preflight", "-json",
		"-profile-mode", "new", "-profile", "firefox",
		"-egress-country", "US", "-egress-source", "explicit")
	if code != 0 {
		t.Fatalf("first run exit = %d (%v)", code, first)
	}
	profile1, _ := first["profile"].(map[string]any)
	if profile1["kind"] != "firefox" {
		t.Fatalf("first run did not select firefox: %v", profile1)
	}

	// Second run: no -profile-mode, no -profile, no egress inputs.
	code, second := invokeSignup(t, path, server.URL,
		"-email", "fixture@example.test", "-stage", "preflight", "-json")
	if code != 0 {
		t.Fatalf("resume exit = %d (%v)", code, second)
	}
	profile2, _ := second["profile"].(map[string]any)
	if profile2["mode"] != "new" {
		t.Fatalf("resume dropped the profile mode: %v", profile2)
	}
	if profile2["id"] != profile1["id"] || profile2["kind"] != "firefox" {
		t.Fatalf("resume changed the profile: %v != %v", profile2, profile1)
	}
	if second["transport"] != "utls" {
		t.Fatalf("receipt transport = %v, want utls", second["transport"])
	}
}

// 3. Device cookies may only seed a fresh task; a resume uses its own snapshot.
func TestDeviceOnlySeedsFreshTask(t *testing.T) {
	fresh := &state{Operations: map[string]operation{}}
	if !deviceSeedsTask(fresh) {
		t.Fatal("a fresh task should be seedable")
	}
	resumed := &state{Stage: "phone-sent", Session: &sessionState{Scnt: "x"}}
	if deviceSeedsTask(resumed) {
		t.Fatal("a resumed task must not take device cookies")
	}
}

// 4. A FD blob minted for another UA is a contradiction once the profile is set.
func TestProfileRejectsForeignFD(t *testing.T) {
	st := newProfileState("fd@example.com")
	session := newProfileSession(t)
	session.fdClientInfo = `{"U":"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/141.0.0.0","L":"en-US","Z":"GMT-05:00","V":"1.1","F":"abc"}`
	_, _, err := installProfile(st, session, profileConfig{Mode: "new", Kind: "chrome", EgressCountry: "US", EgressSource: "explicit"}, "", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "fd-client-info") {
		t.Fatalf("a FD minted for another UA must be refused, got %v", err)
	}
}

// 5. A capture that names a business endpoint must be refused outright.
func TestTelemetryRejectsBusinessEndpoints(t *testing.T) {
	capture := sampleCapture()
	capture["envelopes"] = []map[string]any{{
		"method": "POST", "path": "/account", "contentType": "application/json",
		"body": `{"account":{"name":"x@y.z"}}`,
	}}
	if _, err := loadTelemetryCapture(writeCapture(t, capture)); err == nil {
		t.Fatal("a capture carrying POST /account must be refused, not silently filtered")
	}
}

// 6. Replay may re-stamp existing fields; it must not add fields an event never
// had, because that invents shape.
func TestTelemetryDoesNotInventFields(t *testing.T) {
	capture := sampleCapture()
	capture["envelopes"] = []map[string]any{{
		"method": "POST", "path": "/v2/jslogs", "contentType": "application/json",
		"body": `[{"eventType":"view","componentName":"account"}]`,
	}}
	loaded, err := loadTelemetryCapture(writeCapture(t, capture))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	plan, err := planRebase(loaded, time.Now())
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(plan.Envelopes[0].Body), &events); err != nil {
		t.Fatalf("rebase broke the JSON: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count changed: %d", len(events))
	}
	if _, ok := events[0]["eventId"]; ok {
		t.Fatalf("replay invented an eventId: %v", events[0])
	}
	if _, ok := events[0]["timestamp"]; ok {
		t.Fatalf("replay invented a timestamp: %v", events[0])
	}
}
