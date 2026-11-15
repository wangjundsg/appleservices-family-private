"""W3 evidence: the thin adapter's contract, exercised offline.

A fake "signup.exe" is a Python script that prints receipts and exits with the
codes the real CLI uses. Nothing here touches Apple or a provider.
"""

from __future__ import annotations

import json
import os
import stat
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

import signup_adapter as sa


class FakeJob:
    def __init__(self, folder: Path, **status):
        self.folder = folder
        self.method = "protocol"
        self.cfg = {"job_id": "reg-1", "email": "fixture@example.test",
                    "protocol": {"emailCode": str(folder / "email-code.txt"),
                                 "phoneCode": str(folder / "phone-code.txt")}}
        self.status = {"job_id": "reg-1", "stage": "create", "status": "queued",
                       "active_run_id": "run-1", **status}


def script(path: Path, body: str) -> Path:
    path.write_text("#!/usr/bin/env python\n" + textwrap.dedent(body), encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC)
    return path


class AdapterContractTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.folder = Path(self.tmp.name)
        (self.folder / "email-code.txt").write_text("", encoding="utf-8")

    def adapter(self, body: str) -> sa.SignupAdapter:
        return sa.SignupAdapter(binary=str(script(self.folder / "fake-signup.py", body)))

    def test_exit_four_is_waiting_input(self):
        adapter = self.adapter(
            """
            import json
            print(json.dumps({"ok": False, "stage": "email-sent", "challenge": {"id": "c1"}}))
            raise SystemExit(4)
            """)
        outcome = adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome["outcome"], "waiting_input")
        self.assertEqual(outcome["registration_phase"], "email-sent")

    def test_exit_zero_with_bound_evidence_is_registered(self):
        adapter = self.adapter(
            """
            import json
            print(json.dumps({"ok": True, "accountExists": True, "evidenceRef": "e-1"}))
            """)
        outcome = adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome["outcome"], "registered")

    def test_other_exit_is_inconclusive(self):
        adapter = self.adapter(
            """
            import json
            print(json.dumps({"ok": False, "reason": "network_unavailable", "httpStatus": 503}))
            raise SystemExit(9)
            """)
        outcome = adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome["outcome"], "inconclusive")
        self.assertEqual(outcome["http_status"], 503)

    def test_missing_binary_is_not_a_decision(self):
        adapter = sa.SignupAdapter(binary=str(self.folder / "absent.exe"))
        with self.assertRaises(sa.AdapterError) as caught:
            adapter.create(FakeJob(self.folder))
        self.assertEqual(caught.exception.reason, "protocol_adapter_missing")

    def test_timeout_kills_the_tree(self):
        adapter = self.adapter(
            """
            import time
            time.sleep(30)
            """)
        job = FakeJob(self.folder)
        job.cfg["executor_timeout"] = 0.5
        with self.assertRaises(sa.AdapterError) as caught:
            adapter.create(job)
        self.assertEqual(caught.exception.reason, "executor_timeout")

    def test_foreign_receipt_is_rejected(self):
        adapter = self.adapter(
            """
            import json
            print(json.dumps({"ok": True, "accountExists": True, "job_id": "another-job"}))
            """)
        # The adapter stamps the receipt with its own binding, so a receipt cannot
        # claim another task: validate_outcome must reject a mismatched one.
        context = sa.ExecutorContext.from_job(FakeJob(self.folder))
        with self.assertRaises(sa.AdapterError) as caught:
            sa.validate_outcome({"schema": 1, "job_id": "another-job", "account": "x@y.z",
                                 "run_id": context.run_id, "outcome": "registered"}, context)
        self.assertEqual(caught.exception.reason, "outcome_job_mismatch")

    def test_unsupported_schema_is_rejected(self):
        context = sa.ExecutorContext.from_job(FakeJob(self.folder))
        with self.assertRaises(sa.AdapterError) as caught:
            sa.validate_outcome({"schema": 99, "job_id": context.job_id, "outcome": "registered"}, context)
        self.assertEqual(caught.exception.reason, "outcome_schema_unsupported")

    def test_cancel_never_claims_a_refund(self):
        outcome = sa.SignupAdapter(binary="unused").cancel(FakeJob(self.folder))
        self.assertFalse(outcome["outcome_confirmed"])

    def test_context_carries_explicit_paths_and_no_secrets(self):
        job = FakeJob(self.folder)
        context = sa.ExecutorContext.from_job(job)
        self.assertTrue(context.state_path.endswith("state.json"))
        self.assertNotIn("password", json.dumps(context.__dict__).lower())
        self.assertNotIn("otp", json.dumps(context.__dict__).lower())


if __name__ == "__main__":
    unittest.main()
