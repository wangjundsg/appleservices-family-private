package main

// Boundary checks for the second review round: device adoption, device
// concurrency, and session bindings that the loose fixture used to skip.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 1. A submitted task whose profile went missing must be blocked, not repaired
// by the device it happens to point at.
func TestSubmittedTaskDoesNotBorrowDeviceProfile(t *testing.T) {
	f := newFlowFixture("fixture@example.test")
	server := newTLSFixture(t, f.handler())
	defer server.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "task.json")
	deviceDir := filepath.Join(dir, "devices")
	base := []string{
		"-email", "fixture@example.test", "-password", "Str0ng#Pass1",
		"-phone", "4155550134", "-phonecc", "US", "-stage", "preflight", "-json",
		"-base", server.URL, "-engine-insecure",
		"-profile-mode", "new", "-profile", "chrome",
		"-egress-country", "US", "-egress-source", "explicit",
		"-device", "borrow-dev", "-device-dir", deviceDir,
		"-state", statePath,
	}
	if code, receipt := invokeSignup(t, statePath, server.URL, base...); code != 0 {
		t.Fatalf("first run exit = %d (%v)", code, receipt)
	}

	// Simulate a submitted task that lost its profile.
	saved, err := loadState(statePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	saved.Profile = nil
	saved.Operations = map[string]operation{"validate": {Digest: "legacy", Status: "succeeded"}}
	if err := writeState(statePath, saved); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A device record that still has a profile must not be used to patch it back.
	code, receipt := invokeSignup(t, statePath, server.URL,
		"-email", "fixture@example.test", "-stage", "preflight", "-json",
		"-base", server.URL, "-engine-insecure",
		"-device", "borrow-dev", "-device-dir", deviceDir,
		"-state", statePath)
	if code == 0 {
		t.Fatalf("a submitted task without a profile must be blocked, got exit 0: %v", receipt)
	}
	if reason, _ := receipt["reason"].(string); reason != "profile_blocked" && reason != "profile_configuration_invalid" {
		t.Fatalf("unexpected reason %q (%v)", reason, receipt)
	}
	if saved, _ := loadState(statePath); saved != nil && saved.Profile != nil {
		t.Fatal("the blocked task was handed a profile anyway")
	}
}

// 2. Two runs that each record a visit must both survive: read-modify-write has
// to happen inside one critical section.
func TestDeviceUpdatesDoNotLoseVisits(t *testing.T) {
	dir := t.TempDir()
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := updateDevice(dir, "race-dev", time.Now(), func(d *deviceState) error {
				d.noteVisit([]string{"/account"}, time.Now())
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update failed: %v", err)
		}
	}
	device, err := loadDevice(dir, "race-dev", time.Now())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if device.Visits != writers {
		t.Fatalf("device visits = %d, want %d (lost updates)", device.Visits, writers)
	}
	if len(device.History) != writers {
		t.Fatalf("device history = %d entries, want %d", len(device.History), writers)
	}
}

// 3. The cross-process fixture must actually police the session bindings.
func TestFlowFixtureValidatesSessionBindings(t *testing.T) {
	f := newFlowFixture("fixture@example.test")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "bind.json")
	emailCode := filepath.Join(dir, "email.txt")
	phoneCode := filepath.Join(dir, "phone.txt")

	base := []string{
		"-email", "fixture@example.test", "-password", "Str0ng#Pass1",
		"-phone", "4155550134", "-phonecc", "US", "-stage", "all", "-json",
		"-base", srv.URL, "-captcha-answer", "ABCDE", "-state", statePath,
	}
	run := func(extra ...string) (int, map[string]any, string) {
		var out, diag bytes.Buffer
		code := runCLI(context.Background(), append(append([]string{}, base...), extra...), &out, &diag)
		var receipt map[string]any
		_ = json.Unmarshal(out.Bytes(), &receipt)
		return code, receipt, diag.String()
	}

	if code, receipt, diag := run(); code != 4 {
		t.Fatalf("stage 1 exit = %d (%v) diag=%s", code, receipt, diag)
	}
	// The code files must be newer than the challenge they answer.
	if err := os.WriteFile(emailCode, []byte("012345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, receipt, diag := run("-email-code-file", emailCode); code != 4 {
		t.Fatalf("stage 2 exit = %d (%v) diag=%s", code, receipt, diag)
	}
	if err := os.WriteFile(phoneCode, []byte("065432"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, receipt, diag := run("-email-code-file", emailCode, "-phone-code-file", phoneCode)
	if code != 0 {
		t.Fatalf("stage 3 exit = %d (%v) diag=%s hits=%v", code, receipt, diag, f.hits)
	}
	if receipt["accountExists"] != true {
		t.Fatalf("flow did not prove the account: %v", receipt)
	}
	if f.violations != 0 {
		t.Fatalf("fixture recorded %d binding violations: %v", f.violations, f.violationNotes)
	}
	if !f.sawVerificationID {
		t.Fatal("fixture never saw the verificationId in the submit body")
	}
	if !f.sawRotatedScnt {
		t.Fatal("fixture never saw scnt rotation carried by the client")
	}
	if !f.sawSessionCookie {
		t.Fatal("fixture never saw the bound cookie on an API call")
	}
}
