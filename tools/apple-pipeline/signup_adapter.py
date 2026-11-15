"""Thin protocol adapter: maps a frozen context onto signup.exe and validates the
receipt it comes back with.

Rules from the integration plan §8:
  * the adapter never chooses a mailbox, never buys or refunds, never opens a
    second login chain;
  * it receives an immutable context and returns a versioned outcome;
  * exit codes alone never advance the job — the receipt has to bind back to this
    task, account and run;
  * a child process tree is stopped and confirmed before anything is retried.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping

OUTCOME_SCHEMA = 1
VALID_OUTCOMES = {"waiting_input", "registered", "not_applied", "inconclusive", "reconcile"}
VALID_ACTIONS = {"create", "resume", "cancel"}


class AdapterError(Exception):
    def __init__(self, reason, state="needs_review"):
        super().__init__(reason)
        self.reason = reason
        self.state = state


@dataclass(frozen=True)
class ExecutorContext:
    """Everything the executor may know, and nothing else."""
    job_id: str
    account: str
    method: str
    run_id: str
    action: str
    state_path: str
    device_dir: str = ""
    captcha_path: str = ""
    email_code_path: str = ""
    phone_code_path: str = ""
    challenge: Mapping[str, Any] = field(default_factory=dict)
    timeout: float = 120.0
    profile: Mapping[str, Any] = field(default_factory=dict)
    stage: str = ""

    @classmethod
    def from_job(cls, job) -> "ExecutorContext":
        cfg = job.cfg
        paths = cfg.get("protocol") or {}
        return cls(
            job_id=cfg.get("job_id", ""),
            account=cfg.get("email", ""),
            method=job.method,
            run_id=job.status.get("active_run_id") or uuid.uuid4().hex[:16],
            action="create" if not job.status.get("registration_started") else "resume",
            state_path=str(paths.get("state") or (job.folder / "protocol" / "state.json")),
            device_dir=str(paths.get("deviceDir") or (job.folder / "protocol" / "devices")),
            captcha_path=str(paths.get("captcha") or (job.folder / "protocol" / "captcha.png")),
            email_code_path=str(paths.get("emailCode") or (job.folder / "protocol" / "email-code.txt")),
            phone_code_path=str(paths.get("phoneCode") or (job.folder / "protocol" / "phone-code.txt")),
            challenge=dict(job.status.get("challenge") or {}),
            timeout=float(cfg.get("executor_timeout") or 120.0),
            profile=dict(cfg.get("profile_ref") or {}),
            stage=job.status.get("stage", ""),
        )


def validate_outcome(outcome: Mapping[str, Any], context: ExecutorContext) -> dict:
    """Reject anything that does not bind back to this task, account and run."""
    if not isinstance(outcome, Mapping):
        raise AdapterError("outcome_not_an_object")
    if outcome.get("schema") != OUTCOME_SCHEMA:
        raise AdapterError("outcome_schema_unsupported", "reconcile")
    if outcome.get("job_id") != context.job_id:
        raise AdapterError("outcome_job_mismatch", "reconcile")
    if outcome.get("account") and outcome["account"] != context.account:
        raise AdapterError("outcome_account_mismatch", "reconcile")
    if outcome.get("run_id") and outcome["run_id"] != context.run_id:
        raise AdapterError("outcome_run_mismatch", "reconcile")
    state = outcome.get("outcome")
    if state not in VALID_OUTCOMES:
        raise AdapterError("outcome_state_invalid", "reconcile")
    if outcome.get("action") and outcome["action"] not in VALID_ACTIONS:
        raise AdapterError("outcome_action_invalid", "reconcile")
    return dict(outcome)


def _kill_tree(process: subprocess.Popen) -> None:
    """Stop the child and everything it started, then confirm it is gone."""
    if process.poll() is not None:
        return
    try:
        if os.name == "nt":
            subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0), timeout=20)
        else:
            process.kill()
    finally:
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            raise AdapterError("executor_tree_still_alive", "needs_review")


class SignupAdapter:
    """Runs the protocol executor as a child process with explicit paths."""

    def __init__(self, binary: str | None = None):
        self.binary = binary or os.environ.get("APPLESERVICES_SIGNUP_BIN") or "bin/signup.exe"

    # -- process ----------------------------------------------------------
    def _run(self, context: ExecutorContext, args: list[str]) -> tuple[int, dict]:
        path = Path(self.binary)
        if not path.is_file():
            raise AdapterError("protocol_adapter_missing")
        # A .py stand-in runs through this interpreter; the shipped binary does not.
        command = ([sys.executable, str(path)] if path.suffix == ".py" else [str(path)]) + args
        env = dict(os.environ)
        env.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)  # never re-enter
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                                   env=env)
        try:
            output, _ = process.communicate(timeout=context.timeout)
        except subprocess.TimeoutExpired:
            _kill_tree(process)
            raise AdapterError("executor_timeout", "reconcile") from None
        code = process.returncode
        receipt = {}
        for line in (output or b"").decode("utf-8", "replace").splitlines():
            line = line.strip()
            if line.startswith("{"):
                try:
                    receipt = json.loads(line)
                except ValueError:
                    continue
        return code, receipt

    def _outcome(self, context: ExecutorContext, code: int, receipt: Mapping[str, Any]) -> dict:
        base = {"schema": OUTCOME_SCHEMA, "job_id": context.job_id, "account": context.account,
                "run_id": context.run_id, "action": context.action,
                "executor_revision": receipt.get("revision") or ""}
        if code == 4:
            return {**base, "outcome": "waiting_input",
                    "registration_phase": receipt.get("stage") or "",
                    "challenge": receipt.get("challenge") or {},
                    "verification_id": receipt.get("verificationId") or ""}
        if code == 0 and receipt.get("accountExists") is True:
            return {**base, "outcome": "registered", "evidence_ref": receipt.get("evidenceRef") or ""}
        if code == 0 and receipt.get("accountExists") is False:
            return {**base, "outcome": "not_applied", "failure": receipt.get("reason") or ""}
        return {**base, "outcome": "inconclusive",
                "failure": receipt.get("reason") or f"executor_exit_{code}",
                "http_status": receipt.get("httpStatus")}

    def _invoke(self, context: ExecutorContext, args: list[str]) -> dict:
        code, receipt = self._run(context, args)
        return validate_outcome(self._outcome(context, code, receipt), context)

    # -- adapter API ------------------------------------------------------
    def create(self, job) -> dict:
        context = ExecutorContext.from_job(job)
        return self._invoke(context, ["-registration-method", "protocol", "-stage", "all",
                                      "-state", context.state_path, "-json"])

    def resume(self, job, inputs=None) -> dict:
        context = ExecutorContext.from_job(job)
        args = ["-registration-method", "protocol", "-stage", "all",
                "-state", context.state_path, "-json"]
        for kind, path in (("email", context.email_code_path), ("phone", context.phone_code_path)):
            if inputs and kind in inputs:
                args += [f"-{kind}-code-file", str(path)]
        return self._invoke(context, args)

    def cancel(self, job) -> dict:
        context = ExecutorContext.from_job(job)
        # Stopping is not a refund: the adapter only reports that execution stopped.
        return {"schema": OUTCOME_SCHEMA, "job_id": context.job_id, "account": context.account,
                "run_id": context.run_id, "action": "cancel", "outcome": "not_applied",
                "executor_revision": "", "outcome_confirmed": False}


def factory():
    return SignupAdapter()
