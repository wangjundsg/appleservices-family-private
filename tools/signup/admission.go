package main

// Admission: decide whether a task may touch the network at all.
//
// Two failure modes made this necessary (pre-deploy review R1/R2):
//   - a 429 was only visible on stdout; after a restart the task file said
//     nothing about why it stopped, so a scheduler could try again immediately;
//   - the pause check lived inside the workflow, after warm-up, telemetry and
//     device bookkeeping had already sent requests.
//
// The rule is now: read the task, decide, and only then create a session.

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rateLimitCooldown is how long a 423/429 pauses the task. It is a local policy,
// not a claim about Apple's own timing.
const rateLimitCooldown = 15 * time.Minute

// blockers are the statuses that must stop a run before it sends anything.
// unresolvedOperations lists side-effecting calls that were started but never
// confirmed. A process that died mid-request leaves exactly that shape, and the
// only safe next move is a human looking at it.
func unresolvedOperations(st *state) []string {
	var pending []string
	for key, op := range st.Operations {
		if op.Status == "succeeded" || op.Status == "" {
			continue
		}
		pending = append(pending, key)
	}
	sort.Strings(pending)
	return pending
}

func admissionBlocked(st *state, now time.Time) (string, bool) {
	if len(unresolvedOperations(st)) > 0 {
		return "unresolved_operation", true
	}
	switch st.Status {
	case "needs_review":
		return "task_needs_review", true
	case "rate_limited":
		if st.CooldownUntil.IsZero() || now.Before(st.CooldownUntil) {
			return "server_requires_cooldown", true
		}
	}
	return "", false
}

// admitTask returns an error when the task must not proceed. It may clear an
// expired cooldown in place, which the caller persists.
func admitTask(st *state, now time.Time) error {
	if reason, blocked := admissionBlocked(st, now); blocked {
		return fmt.Errorf("%s", reason)
	}
	if st.Status == "rate_limited" {
		// Cooldown elapsed: the task may try again, and the stale pause is cleared.
		st.Status = ""
		st.Failure = ""
		st.HTTPStatus = 0
		st.CooldownUntil = time.Time{}
		st.BlockedReason = ""
	}
	return nil
}

// reasonOfAdmission maps the admission error onto the receipt code.
func reasonOfAdmission(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if publicReasons[message] {
		return message
	}
	switch {
	case errors.Is(err, errAdmission):
		return message
	}
	return "task_blocked"
}

var errAdmission = errors.New("admission")

// effectiveBase resolves the base a session will actually use.
func effectiveBase(base string) string {
	if strings.TrimSpace(base) == "" {
		return defaultBase
	}
	return base
}

const (
	methodBrowser  = "browser"
	methodProtocol = "protocol"
)

var errRegistrationMethod = errors.New("registration_method_invalid")

// resolveRegistrationMethod freezes the path on first use and refuses changes.
// Old tasks (no field) are browser tasks and get the field persisted so a later
// resume cannot quietly claim protocol.
func resolveRegistrationMethod(st *state, requested string) (string, error) {
	if requested != "" && requested != methodBrowser && requested != methodProtocol {
		return "", errRegistrationMethod
	}
	if st.RegistrationMethod == "" {
		chosen := requested
		if chosen == "" {
			chosen = methodBrowser
		}
		if st.RegistrationMethod != chosen {
			st.RegistrationMethod = chosen
		}
		return chosen, nil
	}
	if requested != "" && requested != st.RegistrationMethod {
		return st.RegistrationMethod, errMethodChange
	}
	return st.RegistrationMethod, nil
}

var errMethodChange = errors.New("registration_method_change_rejected")

// methodDir is the private directory of one task's path: <task dir>/<method>.
// Browser tasks keep their historical directories; nothing is migrated.
func methodDir(statePath, method string) string {
	dir := filepath.Dir(statePath)
	if dir == "" || dir == "." {
		dir = ".cache/signup"
	}
	return filepath.Join(dir, method)
}
