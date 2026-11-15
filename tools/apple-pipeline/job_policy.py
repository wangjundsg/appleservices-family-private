"""Execution permission: one pure decision table for every entry point.

The panel, the CLI, the maintenance sweep and a manual refund all ask the same
question — "may this job do anything right now?" — and they must get the same
answer. Nothing here writes state or touches the network; ``decide`` returns a
``Decision`` and the caller applies it.

Order matters and mirrors docs/REGISTRATION-INTEGRATION-PLAN-2026-11-15.md §5:
a blocked job never reaches a cooldown, a cooldown never becomes a replay
permission, and "not unknown" is never read as "may run".
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

# Terminal or paused states that may only be resolved by an explicit reviewer
# decision, never by the absence of a command file.
HOLD_STATES = {"cancelled", "failed", "stopped"}
REVIEW_STATES = {"unknown", "needs_review"}
READ_ONLY_COMMANDS = {"status", "inspect", "view"}
ALLOWED_ACTIONS = {
    "block", "read_only", "stop", "reconcile", "hold", "busy",
    "handoff", "acquire_then_create", "create", "resume",
}


@dataclass(frozen=True)
class Decision:
    action: str
    reason: str = ""
    needs_executor: bool = False
    needs_resources: bool = False
    # True only when the caller may change business state as a result.
    may_change_state: bool = False
    # Facts the caller must keep untouched (deadline, original reason, ...).
    preserved: Mapping[str, Any] = field(default_factory=dict)
    detail: Mapping[str, Any] = field(default_factory=dict)

    def allowed(self) -> bool:
        return self.action in {"create", "resume", "acquire_then_create", "handoff", "reconcile"}

    def as_view(self) -> dict:
        return {
            "action": self.action,
            "reason": self.reason,
            "needsExecutor": self.needs_executor,
            "needsResources": self.needs_resources,
            "mayChangeState": self.may_change_state,
        }


def _pending_operations(operations: Mapping[str, Any] | None) -> list[str]:
    pending = []
    for key, value in (operations or {}).items():
        state = value.get("state") if isinstance(value, dict) else value
        if state in {"intent", "executing", "inconclusive"}:
            pending.append(key)
    return sorted(pending)


def _cooldown(job: Mapping[str, Any], now: float) -> float | None:
    run = job.get("run") or {}
    raw = run.get("cooldown_until") or job.get("cooldown_until")
    if not raw:
        return None
    try:
        import calendar, time as _time
        parsed = calendar.timegm(_time.strptime(str(raw)[:19], "%Y-%m-%dT%H:%M:%S"))
    except (ValueError, TypeError):
        return float("-inf")  # unparseable deadline is a hold, not permission
    return parsed - now


def decide(job: Mapping[str, Any], command: str = "run", now: float = 0.0, facts: Mapping[str, Any] | None = None) -> Decision:
    facts = facts or {}
    if command in READ_ONLY_COMMANDS:
        return Decision("read_only", "read_only_command")

    # 1. identity / schema / method disagreement blocks everything.
    problem = facts.get("schema_problem") or job.get("schema_problem")
    if problem:
        return Decision("block", str(problem), preserved={"reason": job.get("reason")})
    method_problem = facts.get("method_problem")
    if method_problem:
        return Decision("block", str(method_problem))

    # 2. an explicit stop outranks everything except a dead process.
    if facts.get("stop_requested") or job.get("stop_requested"):
        return Decision("stop", "stop_requested", preserved={"reason": job.get("reason")})

    # 3. states that only a reviewer may clear.
    state = (job.get("run") or {}).get("state") or job.get("status") or ""
    if state in HOLD_STATES:
        return Decision("hold", "stopped_requires_review", preserved={"reason": job.get("reason")})
    if state in REVIEW_STATES:
        return Decision("reconcile", job.get("reason") or "registration_outcome_unconfirmed",
                        preserved={"reason": job.get("reason")})

    # 4. a child that may still be alive blocks a second executor.
    if facts.get("executor_busy"):
        return Decision("busy", "executor_still_running")

    # 5. unresolved operations: read-only reconcile, never a retry.
    pending = _pending_operations(job.get("operations"))
    if pending:
        return Decision("reconcile", "unresolved_operation", detail={"operations": pending},
                        preserved={"reason": job.get("reason")})
    if facts.get("pending_operations"):
        return Decision("reconcile", "unresolved_operation",
                        detail={"operations": facts["pending_operations"]})

    # 6. cooldown: keep the deadline, never add time, never replay.
    remaining = _cooldown(job, now)
    if remaining is not None and remaining > 0:
        return Decision("hold", "server_requires_cooldown", preserved={"cooldown_until": job.get("cooldown_until")})

    # 7. expired resources stop the job; they never trigger a repurchase.
    if facts.get("number_expired"):
        return Decision("reconcile", "number_expired", preserved={"number": job.get("number")})
    if facts.get("challenge_expired"):
        return Decision("reconcile", "challenge_expired", preserved={"challenge": job.get("challenge")})

    # 8. a proven account is handed to activation, never re-registered.
    registration = job.get("registration") or {}
    if registration.get("outcome") == "registered" or job.get("account_created"):
        return Decision("handoff", "registration_proven")

    # 9. a job that never submitted may start.
    intent = bool(job.get("registration_started") or job.get("create_attempted")
                  or (job.get("attempts") or {}).get("create") or job.get("create_confirmed")
                  or state == "waiting_input")
    if not intent:
        return Decision("acquire_then_create" if facts.get("needs_purchase") else "create",
                        "new_job", needs_executor=True, needs_resources=bool(facts.get("needs_purchase")),
                        may_change_state=True)

    # 10. a submitted job may only continue with completion evidence and a live
    # challenge bound to the current stage.
    confirmed = bool(job.get("create_confirmed") or state == "waiting_input")
    challenge = job.get("challenge") or {}
    live = bool(challenge.get("id")) and challenge.get("status") in {None, "open", "sent"}
    if not (confirmed and live):
        return Decision("reconcile", "registration_outcome_unverified",
                        preserved={"reason": job.get("reason")})
    return Decision("resume", "waiting_on_challenge", needs_executor=True, may_change_state=True)


# ---- settlement -----------------------------------------------------------
OTP_RECEIVED = "otp_received"
PAID_NO_OTP = "paid_no_otp"
UNKNOWN = "unknown"


def settlement_decision(job: Mapping[str, Any], order: Mapping[str, Any] | None, observation: Mapping[str, Any] | None) -> Decision:
    """May this order be released back to the provider?

    Stopping is not a refund. Only a confirmed "no OTP arrived" plus a decided
    business outcome justifies releasing a number.
    """
    order = order or {}
    observation = observation or {}
    if observation.get("otp_received") is True:
        return Decision("hold", OTP_RECEIVED, preserved={"cost": order.get("cost")})
    registration = (job.get("registration") or {}).get("outcome")
    if registration == "registered":
        return Decision("hold", "registration_proven")
    if order.get("state") in {"cancelled", "refund_confirmed"}:
        return Decision("hold", "already_settled")
    if observation.get("confirmed_absent") is True:
        return Decision("refund", "refund_eligible", may_change_state=True,
                        detail={"order": order.get("id"), "evidence": observation.get("evidence")})
    # Never submitted: stopping is enough, because no registration exists to lose.
    started = bool(job.get("registration_started") or job.get("create_attempted")
                   or (job.get("attempts") or {}).get("create"))
    if not started:
        return Decision("refund", "stopped_before_submission", may_change_state=True,
                        detail={"order": order.get("id")})
    if registration in {None, "unknown"}:
        return Decision("hold", "registration_outcome_unconfirmed")
    return Decision("reconcile", UNKNOWN, preserved={"order": order.get("id")})
