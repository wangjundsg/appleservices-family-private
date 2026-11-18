package main

// Cross-process acceptance: profile engine + warm-up + pacing + telemetry +
// persistent device, driven through three real CLI processes, against a fixture
// that also validates the session bindings (cookie, scnt rotation,
// verificationId, request bodies).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runChild invokes the CLI in a separate process and returns exit code + receipt.
func runChild(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append([]string{"-test.run=^TestSignupCLIChild$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "SIGNUP_TEST_CHILD=1")
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("child failed: %v %s", err, output)
		}
		code = exitErr.ExitCode()
	}
	var receipt map[string]any
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			var parsed map[string]any
			if json.Unmarshal([]byte(line), &parsed) == nil {
				receipt = parsed
			}
		}
	}
	return code, receipt
}

func TestHumanDeviceTelemetryAcrossProcesses(t *testing.T) {
	fixture := newFlowFixture("fixture@example.test")
	srv := newTLSFixture(t, fixture.handler())
	defer srv.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "task.json")
	deviceDir := filepath.Join(dir, "devices")
	capturePath := writeCapture(t, sampleCapture())
	emailCode := filepath.Join(dir, "email.txt")
	phoneCode := filepath.Join(dir, "phone.txt")

	base := []string{
		"-email", "fixture@example.test",
		"-phone", "4155550134", "-phonecc", "US", "-stage", "all", "-json",
		"-base", srv.URL, "-engine-insecure",
		"-profile-mode", "new", "-profile", "chrome",
		"-egress-country", "US", "-egress-source", "explicit",
		"-human", "-pacing-min", "0s", "-pacing-max", "1ms", "-warmup-assets", "0",
		"-telemetry", capturePath,
		"-device", "flow-dev", "-device-dir", deviceDir,
		"-captcha-answer", "ABCDE",
		"-state", statePath,
	}

	// Process 1: warm-up + create, stops waiting for the email code.
	code, first := runChild(t, base...)
	if code != 4 {
		t.Fatalf("process 1 exit = %d (%v), want 4", code, first)
	}
	warmup, _ := first["warmup"].(map[string]any)
	if warmup == nil || warmup["fetched"].(float64) < 3 {
		t.Fatalf("process 1 did not warm up: %v", first["warmup"])
	}
	tel, _ := first["telemetry"].(map[string]any)
	if tel == nil || tel["sent"].(float64) != 2 {
		t.Fatalf("process 1 did not replay telemetry: %v", first["telemetry"])
	}
	pacingBlock, _ := first["pacing"].(map[string]any)
	if pacingBlock == nil || pacingBlock["enabled"] != true {
		t.Fatalf("pacing not enabled: %v", first["pacing"])
	}
	device1, _ := first["device"].(map[string]any)
	if device1 == nil || device1["visits"].(float64) != 1 {
		t.Fatalf("device block after process 1: %v", first["device"])
	}
	profile1, _ := first["profile"].(map[string]any)
	profileID, _ := profile1["id"].(string)
	if profileID == "" {
		t.Fatalf("process 1 has no profile: %v", first["profile"])
	}

	// Process 2: resume with the email code.
	if err := os.WriteFile(emailCode, []byte("012345"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, second := runChild(t, append(base, "-email-code-file", emailCode)...)
	if code != 4 {
		t.Fatalf("process 2 exit = %d (%v), want 4", code, second)
	}
	if second["emailVerified"] != true {
		t.Fatalf("process 2 did not verify the email: %v", second)
	}
	device2, _ := second["device"].(map[string]any)
	if device2 == nil || device2["visits"].(float64) != 2 {
		t.Fatalf("device visits after process 2: %v", second["device"])
	}
	if loaded, _ := device2["cookiesLoaded"].(float64); loaded != 0 {
		t.Fatalf("resume took device cookies: %v", device2)
	}
	profile2, _ := second["profile"].(map[string]any)
	if id, _ := profile2["id"].(string); id != profileID {
		t.Fatalf("profile drifted across processes: %s != %s", id, profileID)
	}

	// Process 3: resume with the SMS code and finish.
	if err := os.WriteFile(phoneCode, []byte("065432"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, third := runChild(t, append(base, "-email-code-file", emailCode, "-phone-code-file", phoneCode)...)
	if code != 0 {
		t.Fatalf("process 3 exit = %d (%v), want 0", code, third)
	}
	if third["accountExists"] != true {
		t.Fatalf("process 3 did not prove the account: %v", third)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.violations != 0 {
		t.Fatalf("session bindings were violated %d times: %v", fixture.violations, fixture.violationNotes)
	}
	if !fixture.sawSessionCookie {
		t.Fatal("the fixture never saw the bound cookie on an API call")
	}
	if !fixture.sawRotatedScnt {
		t.Fatal("the fixture never saw a correctly rotated scnt")
	}
	if !fixture.sawVerificationID {
		t.Fatal("the fixture never saw the verificationId in the submit body")
	}
	if got := fixture.hits["POST /account"]; got != 1 {
		t.Fatalf("final creation ran %d times", got)
	}
	if got := fixture.hits["POST /jslog"] + fixture.hits["POST /v2/jslogs"]; got != 2 {
		t.Fatalf("telemetry replayed %d envelopes across processes, want 2", got)
	}
	if got := fixture.hits["POST /account/validate"]; got != 1 {
		t.Fatalf("validate ran %d times", got)
	}

	deviceFile, err := loadDevice(deviceDir, "flow-dev", time.Now())
	if err != nil {
		t.Fatalf("device file: %v", err)
	}
	if deviceFile.Visits < 3 {
		t.Fatalf("device visits = %d, want at least 3", deviceFile.Visits)
	}
	if deviceFile.Profile == nil || deviceFile.Profile.ID != profileID {
		t.Fatalf("device profile = %v, want %s", deviceFile.Profile, profileID)
	}
}
