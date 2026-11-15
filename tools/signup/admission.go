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
	"sort"
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
