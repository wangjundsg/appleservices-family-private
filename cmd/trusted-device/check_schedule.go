package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Laky-64/appleservices/gsa"
)

var checkRetryDelays = [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// Only device-query transport failures and server 5xx responses can be retried.
// Unknown errors, authentication failures, identity changes and saves stop here.
func transientCheckFailure(err error) bool {
	var phase *renewalError
	if !errors.As(err, &phase) || phase.stage != "device_query" || errors.Is(err, context.Canceled) {
		return false
	}
	var transport *gsa.DeviceTransportError
	if errors.As(err, &transport) {
		return transport.Retryable && !transport.Canceled
	}
	var request *gsa.DeviceRequestError
	return errors.As(err, &request) && request.Code == 0 && request.HTTPStatus >= 500 && request.HTTPStatus <= 599
}

// Schedule state lives in the existing atomic runtime record. Reserving an
// in-flight attempt before network I/O prevents restart from silently replaying it.
func (s *runtimeStatus) finishCheck(err error, now time.Time, interval time.Duration) {
	s.CheckInFlight = false
	if err == nil {
		s.CheckFailures = 0
		s.AutomaticRefreshPaused = false
		s.NextCheckAt = now.Add(interval)
		s.State, s.Authenticated = "online", true
		s.RenewalStage, s.RenewalError, s.RenewalHTTPStatus = "complete", "", 0
		return
	}
	s.State, s.Authenticated = "refresh_failed", false
	s.RenewalStage, s.RenewalError, s.RenewalHTTPStatus = renewalDiagnostic(err)
	s.CheckFailures++
	s.AutomaticRefreshPaused = true
	s.NextCheckAt = time.Time{}
	if transientCheckFailure(err) && s.CheckFailures <= len(checkRetryDelays) {
		s.AutomaticRefreshPaused = false
		s.NextCheckAt = now.Add(checkRetryDelays[s.CheckFailures-1])
	}
}

func restoreCheckSchedule(dir string, s *runtimeStatus) {
	data, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if os.IsNotExist(err) {
		return
	}
	var old runtimeStatus
	if err != nil || len(data) > 128<<10 || json.Unmarshal(data, &old) != nil || old.CheckFailures < 0 {
		s.AutomaticRefreshPaused = true
		s.RenewalError = "schedule_unreadable"
		return
	}
	s.CheckFailures, s.NextCheckAt, s.AutomaticRefreshPaused = old.CheckFailures, old.NextCheckAt, old.AutomaticRefreshPaused
	s.RenewalStage, s.RenewalError, s.RenewalHTTPStatus = old.RenewalStage, old.RenewalError, old.RenewalHTTPStatus
	s.LastDeviceQueryAt, s.LastRefreshAt, s.RefreshCount = old.LastDeviceQueryAt, old.LastRefreshAt, old.RefreshCount
	s.LastAttemptAt, s.RenewalSteps = old.LastAttemptAt, old.RenewalSteps
	if old.CheckFailures > len(checkRetryDelays) {
		s.AutomaticRefreshPaused = true
		s.NextCheckAt = time.Time{}
	}
	if old.CheckInFlight {
		s.AutomaticRefreshPaused = true
		s.NextCheckAt = time.Time{}
		s.RenewalError = "check_interrupted"
	}
}

func (s *runtimeStatus) checkDue(now time.Time) bool {
	return !s.AutomaticRefreshPaused && !s.CheckInFlight && (s.NextCheckAt.IsZero() || !s.NextCheckAt.After(now))
}

func resetCheckTimer(ctx context.Context, timer *time.Timer, s *runtimeStatus) {
	timer.Stop()
	if ctx.Err() == nil && !s.AutomaticRefreshPaused && !s.NextCheckAt.IsZero() {
		timer.Reset(max(time.Duration(0), time.Until(s.NextCheckAt)))
	}
}
