package main

// Regression tests for the six pre-deploy findings (R1-R6). They started life as
// an offline overlay; they live here now so the guarantees stay enforced.

import (
	"context"
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
