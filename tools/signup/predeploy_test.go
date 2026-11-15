package main

// Regression tests for the six pre-deploy findings (R1-R6). They started life as
// an offline overlay; they live here now so the guarantees stay enforced.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

// R1: a preflight failure must be on disk, with its status and paused state.
func TestPreflightFailurePersistsAcrossRestart(t *testing.T) {
	base := deviceFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/validation/appleid" {
			w.WriteHeader(429)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight",
		"-profile-mode", "new", "-profile", "chrome", "-egress-country", "US", "-egress-source", "explicit")
	if code == 0 || receipt["reason"] != "preflight_unavailable" {
		t.Fatalf("setup: %v", receipt)
	}
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Failure != "preflight_unavailable" || st.HTTPStatus != 429 || st.Status != "rate_limited" {
		t.Fatalf("failure not persisted: failure=%q http=%d status=%q", st.Failure, st.HTTPStatus, st.Status)
	}
	if st.CooldownUntil.IsZero() {
		t.Fatal("a 429 must set a cooldown")
	}
}

// R2: a paused task must not send anything before it reports the pause.
func TestPausedTaskSendsNoRequests(t *testing.T) {
	var calls atomic.Int64
	base := deviceFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight"); code != 0 {
		t.Fatalf("setup: %v", receipt)
	}
	st, _ := loadState(path)
	st.Status = "rate_limited"
	st.Failure = "server_requires_cooldown"
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}

	before := calls.Load()
	code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight",
		"-human", "-journey=false", "-pacing-min", "0s", "-pacing-max", "0s", "-warmup-assets", "0")
	if code == 0 {
		t.Fatalf("paused task ran: %v", receipt)
	}
	if got := calls.Load() - before; got != 0 {
		t.Fatalf("paused task issued %d requests before reporting %v", got, receipt["reason"])
	}
	if receipt["reason"] != "server_requires_cooldown" {
		t.Fatalf("reason = %v, want server_requires_cooldown", receipt["reason"])
	}
}

// R3: an explicit legacy flag may not downgrade a submitted new-mode task.
func TestExplicitLegacyCannotDowngrade(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight",
		"-profile-mode", "new", "-profile", "chrome", "-egress-country", "US", "-egress-source", "explicit"); code != 0 {
		t.Fatalf("setup: %v", receipt)
	}
	st, _ := loadState(path)
	st.Profile = nil
	st.Operations = map[string]operation{"validate": {Status: "succeeded"}}
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}

	code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight", "-profile-mode", "legacy")
	if code == 0 || receipt["reason"] != "profile_blocked" {
		t.Fatalf("explicit legacy downgrade accepted: exit=%d reason=%v transport=%v", code, receipt["reason"], receipt["transport"])
	}
}

// R4: a task is bound to one device; switching -device must be refused, and the
// other device's record must stay untouched.
func TestDeviceSwitchIsRejected(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "task.json")
	devices := filepath.Join(dir, "devices")
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight",
		"-device", "original", "-device-dir", devices,
		"-profile-mode", "new", "-profile", "chrome", "-egress-country", "US", "-egress-source", "explicit"); code != 0 {
		t.Fatalf("setup: %v", receipt)
	}
	other, err := loadDevice(devices, "other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	foreign := profileForEngineTest(t, fingerprint.KindFirefox)
	other.Profile = &foreign
	if err := other.save(devices); err != nil {
		t.Fatal(err)
	}

	code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight", "-device", "other", "-device-dir", devices)
	after, err := loadDevice(devices, "other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if after.Profile == nil || after.Profile.ID != foreign.ID {
		t.Fatalf("other device was rewritten: exit=%d reason=%v", code, receipt["reason"])
	}
	if code == 0 || receipt["reason"] != "device_mismatch" {
		t.Fatalf("device switch not refused: exit=%d reason=%v", code, receipt["reason"])
	}
}

// R6: a stale lock held open by a live process must not hang the caller.
func TestStaleLockIsBounded(t *testing.T) {
	if path := os.Getenv("APPLE_LOCK_CHILD"); path != "" {
		_ = withLock(path, func() error { return nil })
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")
	handle, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path+".lock", old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStaleLockIsBounded$")
	child.Env = append(os.Environ(), "APPLE_LOCK_CHILD="+path)
	_ = child.Run()
	if ctx.Err() != nil {
		t.Fatal("lock exceeded its deadline while a stale lock file stayed open")
	}
}

// An operation that was started but never confirmed (a crashed process leaves
// exactly that) must stop the task before warm-up or telemetry send anything.
func TestUnresolvedIntentBlocksBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name   string
		status string
		extra  []string
	}{
		{"running", "running", nil},
		{"telemetry", "running", []string{"-telemetry", ""}},
		{"expiredCooldown", "rate_limited", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			base := deviceFixture(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				base.ServeHTTP(w, r)
			}))
			defer srv.Close()

			path := filepath.Join(t.TempDir(), "task.json")
			if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight"); code != 0 {
				t.Fatalf("setup: %v", receipt)
			}
			st, _ := loadState(path)
			st.Status = tc.status
			if tc.status == "rate_limited" {
				// Expired cooldown: the pause alone no longer applies, the intent does.
				st.CooldownUntil = time.Now().Add(-time.Minute)
			}
			st.Operations = map[string]operation{"validate": {Status: "intent"}}
			if err := writeState(path, st); err != nil {
				t.Fatal(err)
			}

			args := []string{"-stage", "preflight", "-human", "-journey=false",
				"-pacing-min", "0s", "-pacing-max", "0s", "-warmup-assets", "0"}
			if tc.name == "telemetry" {
				args = append(args, "-telemetry", writeCapture(t, sampleCapture()))
			}
			before := calls.Load()
			code, receipt := invokeSignup(t, path, srv.URL, args...)
			if code == 0 {
				t.Fatalf("unresolved intent ran: %v", receipt)
			}
			if got := calls.Load() - before; got != 0 {
				t.Fatalf("unresolved intent sent %d requests (%v)", got, receipt["reason"])
			}
			if receipt["reason"] != "unresolved_operation" {
				t.Fatalf("reason = %v, want unresolved_operation", receipt["reason"])
			}
		})
	}
}

// Refusing a resume must not rewrite the original cause. Two refusals in a row
// keep it, and the refusal itself is reported separately.
func TestRefusalKeepsOriginalFailure(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight"); code != 0 {
		t.Fatalf("setup: %v", receipt)
	}
	st, _ := loadState(path)
	st.Status = "needs_review"
	st.Failure = "final_account_binding_unconfirmed"
	st.HTTPStatus = 400
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight")
		if code == 0 {
			t.Fatalf("resume %d ran a needs_review task: %v", i, receipt)
		}
		if receipt["failure"] != "final_account_binding_unconfirmed" {
			t.Fatalf("resume %d receipt lost the cause: %v", i, receipt["failure"])
		}
		if receipt["blockedReason"] != "task_needs_review" {
			t.Fatalf("resume %d did not report the refusal: %v", i, receipt["blockedReason"])
		}
		after, err := loadState(path)
		if err != nil {
			t.Fatal(err)
		}
		if after.Failure != "final_account_binding_unconfirmed" || after.HTTPStatus != 400 {
			t.Fatalf("resume %d overwrote the cause: failure=%q http=%d", i, after.Failure, after.HTTPStatus)
		}
		if after.BlockedReason != "task_needs_review" {
			t.Fatalf("resume %d did not persist the refusal: %q", i, after.BlockedReason)
		}
	}
}

// Disabling certificate verification is an offline-only affordance: a copied
// TLS test command must not be able to point it at a real host.
func TestInsecureRequiresOfflineTarget(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	// Production-looking -base: refused before any request (already stricter).
	path := filepath.Join(t.TempDir(), "task.json")
	code, receipt := invokeSignup(t, path, "https://appleid.apple.com", "-stage", "preflight", "-engine-insecure")
	if code == 0 || receipt["reason"] != "base_override_must_be_loopback" {
		t.Fatalf("insecure against a real host: exit=%d reason=%v", code, receipt["reason"])
	}

	// Production default base (no -base at all) with -engine-insecure: refused.
	var out, diag bytes.Buffer
	if rc := runCLI(context.Background(), []string{
		"-email", "fixture@example.test", "-state", filepath.Join(t.TempDir(), "default.json"),
		"-stage", "preflight", "-engine-insecure", "-json",
	}, &out, &diag); rc == 0 {
		t.Fatalf("insecure against the production base was accepted: %s", out.String())
	} else {
		var r map[string]any
		_ = json.Unmarshal(out.Bytes(), &r)
		if r["reason"] != "insecure_requires_offline_target" {
			t.Fatalf("reason = %v, want insecure_requires_offline_target", r["reason"])
		}
	}

	// Loopback fixture: still allowed.
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight"); code != 0 {
		t.Fatalf("loopback sanity: %v", receipt)
	}
	if code, receipt := invokeSignup(t, filepath.Join(t.TempDir(), "tls.json"), srv.URL,
		"-stage", "preflight", "-engine-insecure"); code != 0 {
		t.Fatalf("loopback with insecure should pass: %v", receipt)
	}
}

// I1-A: the registration path is chosen at creation, frozen afterwards, and old
// tasks without the field are browser tasks.
func TestRegistrationMethodIsFrozenAtCreation(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "task.json")

	// Old task without the field → browser, and the field is persisted.
	if code, receipt := invokeSignup(t, path, srv.URL, "-stage", "preflight"); code != 0 {
		t.Fatalf("legacy task: %v", receipt)
	}
	st, _ := loadState(path)
	if st.RegistrationMethod != methodBrowser {
		t.Fatalf("legacy task method = %q, want browser", st.RegistrationMethod)
	}

	// A protocol task, created explicitly.
	protoPath := filepath.Join(dir, "proto.json")
	code, receipt := invokeSignup(t, protoPath, srv.URL, "-stage", "preflight", "-registration-method", "protocol")
	if code != 0 {
		t.Fatalf("protocol task: %v", receipt)
	}
	if receipt["registrationMethod"] != methodProtocol {
		t.Fatalf("receipt method = %v", receipt["registrationMethod"])
	}

	// Resume without the flag keeps it; asking for the other path is refused.
	if code, receipt := invokeSignup(t, protoPath, srv.URL, "-stage", "preflight"); code != 0 {
		t.Fatalf("resume without flag: %v", receipt)
	}
	code, receipt = invokeSignup(t, protoPath, srv.URL, "-stage", "preflight", "-registration-method", "browser")
	if code == 0 || receipt["reason"] != "registration_method_change_rejected" {
		t.Fatalf("path switch accepted: exit=%d reason=%v", code, receipt["reason"])
	}
	if code, receipt := invokeSignup(t, protoPath, srv.URL, "-stage", "preflight", "-registration-method", "protocol"); code != 0 {
		t.Fatalf("same-path resume refused: %v", receipt)
	}

	// An unknown value never becomes a task.
	code, receipt = invokeSignup(t, filepath.Join(dir, "bad.json"), srv.URL, "-stage", "preflight", "-registration-method", "carrier-pigeon")
	if code == 0 || receipt["reason"] != "registration_method_invalid" {
		t.Fatalf("invalid method accepted: exit=%d reason=%v", code, receipt["reason"])
	}
}

// I1-C: a protocol task keeps its private artefacts under <task dir>/protocol;
// browser tasks keep their historical directory.
func TestProtocolTaskUsesItsOwnDirectory(t *testing.T) {
	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()

	dir := t.TempDir()
	protoPath := filepath.Join(dir, "proto.json")
	if code, receipt := invokeSignup(t, protoPath, srv.URL, "-stage", "preflight",
		"-registration-method", "protocol", "-device", "proto-dev"); code != 0 {
		t.Fatalf("protocol task: %v", receipt)
	}
	want := filepath.Join(dir, methodProtocol, "devices", "proto-dev.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("protocol device record not under its own directory: %v", err)
	}

	browserPath := filepath.Join(dir, "browser.json")
	if code, receipt := invokeSignup(t, browserPath, srv.URL, "-stage", "preflight", "-device", "browser-dev"); code != 0 {
		t.Fatalf("browser task: %v", receipt)
	}
	// Browser tasks keep the historical default; nothing was migrated.
	if _, err := os.Stat(filepath.Join(dir, methodBrowser, "devices", "browser-dev.json")); err == nil {
		t.Fatal("browser task was moved into a new directory")
	}
}

// I1 exit-code-4 acceptance: waiting for a code must keep the task, its session
// and its number, and a resume must continue the same stage rather than jump to
// the login path.
func TestWaitingInputKeepsTaskSessionAndPhone(t *testing.T) {
	fixture := newFlowFixture("fixture@example.test")
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "task.json")
	base := []string{"-stage", "all", "-phone", "4155550134", "-phonecc", "US",
		"-captcha-answer", "ABCDE", "-registration-method", "protocol"}

	code, receipt := invokeSignup(t, path, srv.URL, base...)
	if code != 4 {
		t.Fatalf("waiting for the email code should be exit 4, got %d (%v)", code, receipt)
	}
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Stage != "email-sent" || st.Phone == "" || st.Session == nil || st.Session.Scnt == "" {
		t.Fatalf("waiting state lost: stage=%q phone=%q session=%v", st.Stage, st.Phone, st.Session != nil)
	}
	if st.RegistrationMethod != methodProtocol {
		t.Fatalf("method lost while waiting: %q", st.RegistrationMethod)
	}
	phone, scnt := st.Phone, st.Session.Scnt

	emailCode := filepath.Join(dir, "email.txt")
	if err := os.WriteFile(emailCode, []byte("012345"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, receipt = invokeSignup(t, path, srv.URL, append(base, "-email-code-file", emailCode)...)
	if code != 4 {
		t.Fatalf("waiting for the SMS code should be exit 4, got %d (%v)", code, receipt)
	}
	st, _ = loadState(path)
	if st.Stage != "phone-sent" || st.Phone != phone || st.Session == nil || st.Session.Scnt == "" {
		t.Fatalf("resume did not continue the same task: stage=%q phone=%q", st.Stage, st.Phone)
	}
	if st.Session.Scnt == scnt {
		t.Fatal("session was never refreshed while waiting")
	}
	if st.RegistrationMethod != methodProtocol {
		t.Fatalf("method changed across a wait: %q", st.RegistrationMethod)
	}

	phoneCode := filepath.Join(dir, "phone.txt")
	if err := os.WriteFile(phoneCode, []byte("065432"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, receipt = invokeSignup(t, path, srv.URL, append(base, "-email-code-file", emailCode, "-phone-code-file", phoneCode)...)
	if code != 0 {
		t.Fatalf("final resume failed: %d %v", code, receipt)
	}
	if receipt["accountExists"] != true {
		t.Fatalf("final receipt: %v", receipt)
	}
}
