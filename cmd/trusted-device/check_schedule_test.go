package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/gsa"
)

func TestCheckRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		err         error
		retry       bool
	}{
		{"temporary", "device_query", &gsa.DeviceTransportError{Retryable: true}, true},
		{"503", "device_query", &gsa.DeviceRequestError{HTTPStatus: 503}, true},
		{"401", "device_query", &gsa.DeviceRequestError{HTTPStatus: 401}, false},
		{"403", "device_query", &gsa.DeviceRequestError{HTTPStatus: 403}, false},
		{"rate-limit", "device_query", &gsa.DeviceRequestError{HTTPStatus: 429}, false},
		{"explicit-server-code", "device_query", &gsa.DeviceRequestError{HTTPStatus: 503, Code: -20755}, false},
		{"unknown", "device_query", errors.New("fixture"), false},
		{"canceled", "device_query", &gsa.DeviceTransportError{Retryable: true, Canceled: true}, false},
		{"identity", "account_binding", &gsa.DeviceTransportError{Retryable: true}, false},
		{"save", "save_session", &gsa.DeviceTransportError{Retryable: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientCheckFailure(&renewalError{tc.stage, tc.err}); got != tc.retry {
				t.Fatalf("retry=%t want %t", got, tc.retry)
			}
		})
	}
}

func TestRetryBudgetAndPauseSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	s := runtimeStatus{}
	failure := &renewalError{"device_query", &gsa.DeviceRequestError{HTTPStatus: 503}}
	for i, delay := range checkRetryDelays {
		s.finishCheck(failure, now, 10*time.Minute)
		if s.CheckFailures != i+1 || s.AutomaticRefreshPaused || !s.NextCheckAt.Equal(now.Add(delay)) || s.Authenticated {
			t.Fatal("incorrect backoff or health")
		}
		if err := s.save(dir); err != nil {
			t.Fatal(err)
		}
		restored := runtimeStatus{}
		restoreCheckSchedule(dir, &restored)
		if restored.checkDue(now) || !restored.checkDue(now.Add(delay)) || restored.CheckFailures != s.CheckFailures {
			t.Fatal("restart lost budget or deadline")
		}
		s = restored
		now = now.Add(delay)
	}
	s.finishCheck(failure, now, 10*time.Minute)
	if !s.AutomaticRefreshPaused || !s.NextCheckAt.IsZero() {
		t.Fatal("retry budget not bounded")
	}
	if err := s.save(dir); err != nil {
		t.Fatal(err)
	}
	restored := runtimeStatus{}
	restoreCheckSchedule(dir, &restored)
	if restored.checkDue(now.Add(24 * time.Hour)) {
		t.Fatal("restart cleared exhausted budget")
	}
	// An explicit successful check can resume; time alone cannot.
	restored.finishCheck(nil, now, 10*time.Minute)
	if restored.CheckFailures != 0 || restored.AutomaticRefreshPaused || !restored.Authenticated || !restored.NextCheckAt.Equal(now.Add(10*time.Minute)) {
		t.Fatal("success did not restore normal schedule")
	}
}

func TestRestartRetainsAuthenticationRefusalAndInterruptedAttempt(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		dir := t.TempDir()
		now := time.Now().UTC()
		s := runtimeStatus{CheckInFlight: interrupted}
		if !interrupted {
			s.finishCheck(&renewalError{"device_query", &gsa.DeviceRequestError{HTTPStatus: 401}}, now, 10*time.Minute)
		}
		if err := s.save(dir); err != nil {
			t.Fatal(err)
		}
		restored := runtimeStatus{}
		restoreCheckSchedule(dir, &restored)
		if !restored.AutomaticRefreshPaused || restored.checkDue(now.Add(time.Hour)) {
			t.Fatal("restart replays refused or interrupted request")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	s := runtimeStatus{}
	restoreCheckSchedule(dir, &s)
	if !s.AutomaticRefreshPaused {
		t.Fatal("unreadable schedule failed open")
	}
}

type cancelingLogin struct {
	fakeMaintainedLogin
	cancel context.CancelFunc
}

func (l *cancelingLogin) AccountDevicesContext(ctx context.Context) ([]appleservices.AccountDevice, error) {
	l.cancel()
	return nil, ctx.Err()
}
func TestSessionCheckCancellationAndRefusalNeverSaveOrRegister(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	login := &cancelingLogin{fakeMaintainedLogin: fakeMaintainedLogin{account: "fixture"}, cancel: cancel}
	_, _, err := renewSessionObservedContext(ctx, func() (maintainedLogin, error) { return login, nil }, "fixture", appleservices.DeviceRegistration{}, false, func(string, string) {})
	if !errors.Is(err, context.Canceled) || login.saved != 0 || login.registered != 0 {
		t.Fatal("cancellation saved or registered a session")
	}
	refused := &fakeMaintainedLogin{account: "fixture", listErr: &gsa.DeviceRequestError{HTTPStatus: 401}}
	var steps []renewalStep
	_, _, err = renewSessionObservedContext(context.Background(), func() (maintainedLogin, error) { return refused, nil }, "fixture", appleservices.DeviceRegistration{}, false, func(stage, result string) { steps = append(steps, renewalStep{stage, result}) })
	want := []renewalStep{{"session_restore", "completed"}, {"service_session", "available"}, {"device_query", "failed"}}
	if err == nil || refused.saved != 0 || refused.registered != 0 || !reflect.DeepEqual(steps, want) {
		t.Fatal("refusal continued beyond device query")
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	s := runtimeStatus{NextCheckAt: time.Now().Add(-time.Second)}
	resetCheckTimer(ctx, timer, &s)
	select {
	case <-timer.C:
		t.Fatal("canceled worker scheduled another attempt")
	default:
	}
}
