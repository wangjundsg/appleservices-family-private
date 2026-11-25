package dashboard

import (
	"github.com/Laky-64/appleservices/internal/accountguard"
	"github.com/Laky-64/appleservices/internal/control"
	"time"
)

func protectedResult() control.Result {
	return control.Result{Stage: "account_protected", Message: accountguard.Message}
}

func normalizeHealth(dir string, status Status) Status {
	status.Protected = accountguard.Check(dir) != nil
	status.SessionVerified = status.Running && !status.Protected && control.SessionFresh(status.State, status.Authenticated, status.LastDeviceQueryAt, time.Now())
	if !status.SessionVerified {
		status.Authenticated = false
	}
	if status.Protected {
		status.State = "account_locked"
		status.FailureReason = "account_protected"
		status.VerificationRequired = false
	}
	return status
}
