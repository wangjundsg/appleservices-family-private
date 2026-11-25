package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/gsa"
)

func TestRenewalDiagnosticsKeepStageWithoutLeakingError(t *testing.T) {
	login := &fakeMaintainedLogin{account: "fixture", listErr: &gsa.DeviceRequestError{HTTPStatus: 401, ServerMessage: "private-token"}}
	var steps []renewalStep
	_, _, err := renewSessionObserved(func() (maintainedLogin, error) { return login, nil }, "fixture", appleservices.DeviceRegistration{}, false, func(stage, result string) { steps = append(steps, renewalStep{stage, result}) })
	stage, reason, status := renewalDiagnostic(err)
	if stage != "device_query" || reason != "device_request_rejected" || status != 401 || login.saved != 0 || login.registered != 0 {
		t.Fatal("incorrect phase or mutation")
	}
	if err.Error() != "session check failed at device_query" {
		t.Fatal("raw error exposed")
	}
	want := []renewalStep{{"session_restore", "completed"}, {"service_session", "available"}, {"device_query", "failed"}}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps: %+v", steps)
	}
	_, _, err = renewSessionMode(func() (maintainedLogin, error) { return nil, errors.New("private-password") }, "fixture", appleservices.DeviceRegistration{}, false)
	stage, reason, _ = renewalDiagnostic(err)
	if stage != "session_restore" || reason != "unconfirmed" {
		t.Fatal("restore failure misclassified")
	}
}

func TestRuntimeSaveCannotClearPersistentProtection(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "account-guard.json"), []byte(`{"version":1,"blocked":true}`), 0600)
	status := runtimeStatus{Running: true, Authenticated: true, State: "online"}
	if err := status.save(dir); err != nil {
		t.Fatal(err)
	}
	if status.State != "account_locked" || status.Authenticated {
		t.Fatal("cache replaced lock")
	}
}
