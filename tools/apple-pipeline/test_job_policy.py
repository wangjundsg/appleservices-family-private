"""T01 / T02 / T08 evidence: the decision table answers precisely.

These are pure-function tests on purpose: the table is the contract every entry
point (CLI, panel, maintenance, refund) shares, so it is verified without a job,
a lock or a network.
"""

from __future__ import annotations

import time
import unittest

import job_policy


def iso(offset=0):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() + offset))


def view(**overrides):
    base = {
        "run": {"state": "queued"},
        "status": "queued",
        "operations": {},
        "attempts": {},
        "challenge": {},
    }
    base.update(overrides)
    # the run.state and status carry the same fact; keep the helper honest
    base["run"] = {**base.get("run", {}), "state": base["status"],
                   "cooldown_until": base.get("cooldown_until")}
    return base


class AdmissionTableTest(unittest.TestCase):
    def test_new_job_may_create(self):
        self.assertEqual(job_policy.decide(view(), "run", time.time()).action, "create")

    def test_new_job_that_needs_a_number_acquires_first(self):
        decision = job_policy.decide(view(), "run", time.time(), {"needs_purchase": True})
        self.assertEqual(decision.action, "acquire_then_create")
        self.assertTrue(decision.needs_resources)

    def test_confirmed_waiting_with_a_live_challenge_resumes(self):
        decision = job_policy.decide(
            view(status="waiting_input", registration_started=True, create_confirmed=True,
                 challenge={"id": "c1", "kind": "email", "status": "open"}), "run", time.time())
        self.assertEqual(decision.action, "resume")

    def test_waiting_without_a_live_challenge_is_reviewed(self):
        decision = job_policy.decide(
            view(status="waiting_input", registration_started=True, create_confirmed=True, challenge={}),
            "run", time.time())
        self.assertEqual(decision.action, "reconcile")
        self.assertEqual(decision.preserved.get("reason"), None)

    def test_unknown_and_needs_review_never_run(self):
        for state in ("unknown", "needs_review"):
            with self.subTest(state):
                decision = job_policy.decide(view(status=state, reason="registration_outcome_unconfirmed"),
                                             "run", time.time())
                self.assertEqual(decision.action, "reconcile")
                self.assertFalse(decision.needs_executor)
                self.assertEqual(decision.reason, "registration_outcome_unconfirmed")

    def test_pending_operation_blocks(self):
        decision = job_policy.decide(
            view(status="running", registration_started=True, create_confirmed=True,
                 operations={"phone": {"state": "intent"}},
                 challenge={"id": "c1", "status": "open"}), "run", time.time())
        self.assertEqual(decision.action, "reconcile")
        self.assertEqual(decision.detail["operations"], ["phone"])

    def test_cooldown_holds_and_keeps_its_deadline(self):
        decision = job_policy.decide(view(cooldown_until=iso(600)), "run", time.time())
        self.assertEqual(decision.action, "hold")
        self.assertEqual(decision.reason, "server_requires_cooldown")
        self.assertIn("cooldown_until", decision.preserved)

    def test_expired_number_is_reviewed_not_repurchased(self):
        decision = job_policy.decide(view(), "run", time.time(), {"number_expired": True})
        self.assertEqual(decision.action, "reconcile")
        self.assertEqual(decision.reason, "number_expired")

    def test_proven_registration_hands_over(self):
        decision = job_policy.decide(view(registration={"outcome": "registered"}), "run", time.time())
        self.assertEqual(decision.action, "handoff")
        self.assertFalse(decision.needs_executor)

    def test_stop_outranks_everything(self):
        decision = job_policy.decide(view(status="running"), "run", time.time(), {"stop_requested": True})
        self.assertEqual(decision.action, "stop")

    def test_status_is_read_only(self):
        for command in ("status", "inspect"):
            with self.subTest(command):
                decision = job_policy.decide(view(status="running"), command, time.time())
                self.assertEqual(decision.action, "read_only")

    def test_schema_problem_blocks_before_anything(self):
        decision = job_policy.decide(view(), "run", time.time(), {"schema_problem": "schema_unsupported"})
        self.assertEqual(decision.action, "block")

    def test_terminal_state_holds_without_a_review_decision(self):
        decision = job_policy.decide(view(status="cancelled"), "run", time.time())
        self.assertEqual(decision.action, "hold")


class SettlementTableTest(unittest.TestCase):
    def _job(self, **overrides):
        base = {"registration": {"outcome": "unknown"}, "registration_started": True}
        base.update(overrides)
        return base

    def test_received_otp_is_never_refunded(self):
        decision = job_policy.settlement_decision(self._job(), {"state": "owned"}, {"otp_received": True})
        self.assertEqual(decision.action, "hold")
        self.assertEqual(decision.reason, "otp_received")

    def test_registered_account_keeps_its_number(self):
        decision = job_policy.settlement_decision(
            self._job(registration={"outcome": "registered"}), {"state": "owned"}, {})
        self.assertEqual(decision.action, "hold")
        self.assertEqual(decision.reason, "registration_proven")

    def test_unknown_registration_holds(self):
        decision = job_policy.settlement_decision(self._job(), {"state": "owned"}, {})
        self.assertEqual(decision.action, "hold")

    def test_cancel_before_submission_may_refund(self):
        decision = job_policy.settlement_decision(
            self._job(registration_started=False), {"state": "owned"}, {"otp_received": False})
        self.assertEqual(decision.action, "refund")

    def test_confirmed_absent_is_the_only_override(self):
        decision = job_policy.settlement_decision(
            self._job(), {"state": "owned"}, {"otp_received": False, "confirmed_absent": True})
        self.assertEqual(decision.action, "refund")

    def test_unverifiable_observation_asks_for_reconcile(self):
        decision = job_policy.settlement_decision(self._job(), {"state": "owned"}, {})
        self.assertEqual(decision.action, "hold")


if __name__ == "__main__":
    unittest.main()
