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
