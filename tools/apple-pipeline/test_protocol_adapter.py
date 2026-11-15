"""Protocol adapter boundary and waiting semantics (offline).

These tests drive the real pipeline with a fake adapter injected through
APPLESERVICES_PROTOCOL_ADAPTER, and count what the pipeline does while a job is
waiting for input: it must not cancel the number, release the mailbox or jump to
the login stage.
"""

from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path

import pipeline
import protocol_adapter


class FakeAdapter:
    """Records calls on the class; the registry builds a new instance per resolve."""

    calls = {"create": 0, "resume": 0, "cancel": 0}
    next_outcome = {"state": "waiting_input", "stage": "email-sent"}
    last_inputs = None

    @classmethod
    def reset(cls, outcome=None):
        cls.calls = {"create": 0, "resume": 0, "cancel": 0}
        cls.next_outcome = outcome or {"state": "waiting_input", "stage": "email-sent"}
        cls.last_inputs = None

    def create(self, job):
        FakeAdapter.calls["create"] += 1
        return dict(FakeAdapter.next_outcome)

    def resume(self, job, inputs=None):
        FakeAdapter.calls["resume"] += 1
        FakeAdapter.last_inputs = dict(inputs or {})
        return dict(FakeAdapter.next_outcome)

    def cancel(self, job):
        FakeAdapter.calls["cancel"] += 1
        outcome = dict(FakeAdapter.next_outcome)
        if outcome.get("state") != "cancelled":
            outcome = {"state": "cancelled"}
        return outcome


def make_config(folder: Path, **overrides):
    cfg = {
        "job_id": "reg-test",
        "email": "fixture@example.test",
        "password": "Str0ng#Pass1",
        "accept_terms": True,
        "handoff": "dashboard",
        "email_code_file": str(folder / "email-code.txt"),
        "captcha_solver": "manual",
        "egress_mode": "direct",
        "roxy": {"token": "t", "workspaceId": "w", "projectId": "p", "locale": "en-US", "timeZone": "America/New_York"},
        "sms": {"activation": "12345", "phone": "4155550134", "phonecc": "US",
                "expires_at": "2099-01-01T00:00:00Z", "key": "k"},
        "registration_method": "protocol",
    }
    cfg.update(overrides)
    return cfg


class ProtocolAdapterBoundaryTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        Path(self.folder / "email-code.txt").write_text("", encoding="utf-8")
        FakeAdapter.reset()
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def test_missing_adapter_stops_before_resources(self):
        cfg = make_config(self.folder)
        pipeline.validate(cfg)
        with self.assertRaises(protocol_adapter.AdapterMissing) as caught:
            protocol_adapter.resolve(cfg.get("protocol"))
        self.assertEqual(caught.exception.reason, "protocol_adapter_missing")

    def test_missing_adapter_becomes_a_pipeline_error(self):
        # The runner maps AdapterMissing onto a needs_review PipelineError, which
        # is what stops the job before the reservation and the number purchase.
        with self.assertRaises(pipeline.PipelineError) as caught:
            try:
                protocol_adapter.resolve(None)
            except protocol_adapter.AdapterMissing as missing:
                raise pipeline.PipelineError(missing.reason, "needs_review")
        self.assertEqual(caught.exception.reason, "protocol_adapter_missing")
        self.assertEqual(caught.exception.state, "needs_review")

    def test_waiting_survives_a_restart(self):
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        cfg = make_config(self.folder)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(number_owned=True, number="4155550134", stage="create", status="queued")
        self.assertTrue(job.protocol_step())

        restored = pipeline.Job(self.folder, cfg, pipeline.read(self.folder / "result.json"))
        self.assertEqual(restored.status["status"], "waiting_input")
        self.assertEqual(restored.status["number"], "4155550134")
        self.assertTrue(restored.status["number_owned"])
        self.assertEqual(restored.status["stage"], "email-sent")

    def test_waiting_pauses_the_run_loop_for_both_number_modes(self):
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        for label, sms in (
            ("existing", {"activation": "12345", "phone": "4155550134", "phonecc": "US",
                          "expires_at": "2099-01-01T00:00:00Z", "key": "k"}),
            ("auto_purchase", {"auto_purchase": True, "phonecc": "US", "key": "k", "api": "https://fixture.invalid/api"}),
        ):
            with self.subTest(label):
                FakeAdapter.reset()
                folder = self.folder / label
                folder.mkdir(parents=True, exist_ok=True)
                cfg = make_config(folder, sms=sms)
                job = pipeline.Job(folder, cfg)
                job.status.update(stage="create", status="queued")
                job.save = lambda **kw: None

                counts = {"claim": 0, "settle": 0, "browser": 0, "login": 0}
                job.number = lambda: (job.status.update(number_owned=True, number="4155550134"),
                                      counts.__setitem__("claim", counts["claim"] + 1))[1]
                job.settle_number = lambda: counts.__setitem__("settle", counts["settle"] + 1)
                job.cleanup_browser = lambda: counts.__setitem__("browser", counts["browser"] + 1)
                job.authenticate = lambda: counts.__setitem__("login", counts["login"] + 1)
                job.begin = lambda stage: None

                job.run()
                self.assertEqual(job.status["status"], "waiting_input")
                self.assertEqual(counts["settle"], 0, "the number was released while waiting")
                self.assertEqual(counts["browser"], 0, "browser cleanup ran for a protocol job")
                self.assertEqual(counts["login"], 0, "the job jumped to the login stage")
                self.assertEqual(counts["claim"], 1, "the number must be bought/claimed before the adapter")
                self.assertEqual(FakeAdapter.calls["create"], 1)

    def test_cancel_is_explicit(self):
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        adapter = protocol_adapter.resolve(None)
        result = adapter.cancel(pipeline.Job(self.folder, make_config(self.folder)))
        self.assertEqual(result["state"], "cancelled")
        self.assertEqual(FakeAdapter.calls["cancel"], 1)

    def test_waiting_keeps_resources(self):
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        cfg = make_config(self.folder)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(number_owned=True, number="4155550134", stage="create", status="queued")
        job.update(**{})

        counters = {"cancel_order": 0, "release_email": 0, "switch_login": 0}
        original_settle = job.settle_number
        original_auth = job.authenticate

        def settle():
            counters["cancel_order"] += 1
            return original_settle()

        def authenticate():
            counters["switch_login"] += 1
            return original_auth()

        job.settle_number = settle
        job.authenticate = authenticate
        job.cleanup_browser = lambda: counters.__setitem__("release_email", counters["release_email"] + 1)

        waiting = job.protocol_step()
        self.assertTrue(waiting)
        self.assertEqual(job.status["status"], "waiting_input")
        self.assertEqual(job.status["stage"], "email-sent")
        self.assertEqual(FakeAdapter.calls["create"], 1)
        for name, value in counters.items():
            self.assertEqual(value, 0, f"{name} ran while waiting")
        self.assertEqual(job.status.get("number"), "4155550134")

    def test_browser_method_is_unchanged(self):
        cfg = make_config(self.folder, registration_method="browser")
        job = pipeline.Job(self.folder, cfg)
        self.assertEqual(job.method, "browser")

    def test_invalid_method_is_refused(self):
        cfg = make_config(self.folder, registration_method="carrier-pigeon")
        with self.assertRaises(pipeline.PipelineError) as caught:
            pipeline.validate(cfg)
        self.assertEqual(caught.exception.reason, "invalid_registration_method")


if __name__ == "__main__":
    unittest.main()


class ProtocolResumeTest(unittest.TestCase):
    """A waiting job resumes through the adapter, not through the login handoff."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        Path(self.folder / "email-code.txt").write_text("", encoding="utf-8")
        FakeAdapter.reset()
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def _resume_job(self, outcome):
        cfg = make_config(self.folder)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", account_created=False)
        handoffs = {"count": 0}

        def handoff():
            handoffs["count"] += 1
            job.update(status="ready", stage="done")

        job.dashboard_handoff = handoff
        job.begin = lambda stage: None
        FakeAdapter.reset(outcome)
        job.run()
        return job, handoffs

    def test_resume_goes_back_to_the_adapter(self):
        job, handoffs = self._resume_job({"state": "waiting_input", "stage": "phone-sent"})
        self.assertEqual(FakeAdapter.calls["resume"], 1, "resume did not reach the adapter")
        self.assertEqual(FakeAdapter.calls["create"], 0, "resume repeated the create stage")
        self.assertEqual(handoffs["count"], 0, "a waiting job was handed to login")
        self.assertEqual(job.status["status"], "waiting_input")
        self.assertEqual(job.status["stage"], "phone-sent")

    def test_handover_happens_only_after_ready(self):
        job, handoffs = self._resume_job({"state": "ready", "reason": "protocol_registration_complete"})
        self.assertEqual(FakeAdapter.calls["resume"], 1)
        self.assertEqual(handoffs["count"], 1, "a finished protocol job was not handed over")
        self.assertEqual(job.status["status"], "ready")
        self.assertTrue(job.status["account_created"])

    def test_needs_review_does_not_settle_the_number(self):
        cfg = make_config(self.folder)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(stage="create", status="queued", number_owned=True, number="4155550134")
        settled = {"count": 0}
        job.number = lambda: None
        job.settle_number = lambda: settled.__setitem__("count", settled["count"] + 1)
        job.begin = lambda stage: None
        job.save = lambda **kw: None
        FakeAdapter.reset({"state": "failed", "status": "needs_review", "reason": "adapter_needs_review"})
        job.run()
        self.assertEqual(job.status["status"], "needs_review")
        self.assertEqual(settled["count"], 0, "a needs_review job released its number")
        self.assertTrue(job.status.get("refund_deferred"))


class ProtocolMethodFieldsTest(unittest.TestCase):
    def test_conflicting_fields_are_refused(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        folder = Path(tmp.name)
        Path(folder / "email-code.txt").write_text("", encoding="utf-8")
        cfg = make_config(folder, registrationMethod="browser")
        with self.assertRaises(pipeline.PipelineError) as caught:
            pipeline.validate(cfg)
        self.assertEqual(caught.exception.reason, "registration_method_conflict")


if __name__ == "__main__":
    unittest.main()

class WaitingResumeWithRealBeginTest(unittest.TestCase):
    """The waiting flag must be read before begin() rewrites the status."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        Path(self.folder / "email-code.txt").write_text("", encoding="utf-8")
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        FakeAdapter.reset()

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def test_real_begin_still_resumes(self):
        cfg = make_config(self.folder)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", challenge_at=pipeline.iso())
        handoffs = {"count": 0}

        def handoff():
            handoffs["count"] += 1
            job.update(status="ready", stage="done")

        job.dashboard_handoff = handoff
        # begin() is NOT replaced: it flips the status to running, which is exactly
        # what used to defeat the waiting check.
        FakeAdapter.reset({"state": "waiting_input", "stage": "phone-sent"})
        job.run()
        self.assertEqual(FakeAdapter.calls["resume"], 1, "resume did not reach the adapter")
        self.assertEqual(FakeAdapter.calls["create"], 0)
        self.assertEqual(handoffs["count"], 0, "a waiting job was handed to login")
        self.assertEqual(job.status["status"], "waiting_input")


class MaintenanceSettlementTest(unittest.TestCase):
    """The maintenance sweep must not settle an unresolved job."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self._state = pipeline.STATE
        self._sms = pipeline.sms_call
        self.calls = []
        pipeline.STATE = self.root
        pipeline.sms_call = lambda *a, **kw: (self.calls.append(kw), {"ok": True})[1]

    def tearDown(self):
        pipeline.STATE = self._state
        pipeline.sms_call = self._sms
        self.tmp.cleanup()

    def test_needs_review_with_deferred_refund_is_not_settled(self):
        folder = self.root / "reg-maint"
        folder.mkdir(parents=True, exist_ok=True)
        cfg = make_config(folder)
        cfg["sms"] = {"activation": "12345", "phone": "4155550134", "phonecc": "US",
                      "expires_at": "2099-01-01T00:00:00Z", "key": "k"}
        pipeline.atomic(folder / "input.json", cfg)
        pipeline.atomic(folder / "result.json", {
            "job_id": cfg["job_id"], "stage": "create", "status": "needs_review",
            "number_owned": True, "account_created": False, "refund_deferred": True,
            "registration_started": True,
        })
        pipeline.maintain_once()
        releases = [c for c in self.calls if c.get("action") == "setStatus"]
        self.assertEqual(releases, [], "the sweep released an unresolved number")


class ExpiredNumberTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        Path(self.folder / "email-code.txt").write_text("", encoding="utf-8")
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        FakeAdapter.reset()

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def test_expired_number_does_not_repurchase(self):
        cfg = make_config(self.folder, sms={"auto_purchase": True, "phonecc": "US", "key": "k",
                                            "api": "https://fixture.invalid/api"})
        cfg["sms"]["expires_at"] = pipeline.iso(pipeline.time.time() - 60)
        job = pipeline.Job(self.folder, cfg)
        job.status.update(stage="create", status="waiting_input", number_owned=True, number="4155550134")
        purchases = {"count": 0}
        job.number = lambda: purchases.__setitem__("count", purchases["count"] + 1)
        with self.assertRaises(pipeline.PipelineError) as caught:
            job.protocol_step()
        self.assertEqual(caught.exception.reason, "number_expired")
        self.assertEqual(job.status["status"], "needs_review")
        self.assertEqual(purchases["count"], 0, "an expired job bought another number")


class BoundInputTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        self.code = self.folder / "email-code.txt"
        self.code.write_text("012345", encoding="utf-8")
        self.cfg = make_config(self.folder, protocol={"emailCode": str(self.code),
                                                      "phoneCode": str(self.folder / "phone-code.txt")})
        self.job = pipeline.Job(self.folder, self.cfg)
        # The challenge started before the code arrived, otherwise every code would
        # be "older than the challenge" by a few milliseconds.
        self.job.status.update(challenge_at=pipeline.iso(pipeline.time.time() - 30))

    def tearDown(self):
        self.tmp.cleanup()

    def test_stale_code_is_refused(self):
        old = pipeline.time.time() - 600
        os.utime(self.code, (old, old))
        with self.assertRaises(pipeline.PipelineError) as caught:
            self.job.read_input("email")
        self.assertEqual(caught.exception.reason, "input_predates_challenge")

    def test_consumed_code_is_refused(self):
        self.assertEqual(self.job.read_input("email"), "012345")
        with self.assertRaises(pipeline.PipelineError) as caught:
            self.job.read_input("email")
        self.assertEqual(caught.exception.reason, "input_already_consumed")

    def test_code_outside_the_task_is_refused(self):
        outside = Path(tempfile.mkdtemp()) / "code.txt"
        outside.write_text("999999", encoding="utf-8")
        self.job.cfg["protocol"]["emailCode"] = str(outside)
        with self.assertRaises(pipeline.PipelineError) as caught:
            self.job.read_input("email")
        self.assertEqual(caught.exception.reason, "input_outside_task")

class InputClosureTest(unittest.TestCase):
    """A resume carries the code that answers the current challenge."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        self.code = self.folder / "email-code.txt"
        self.code.write_text("012345", encoding="utf-8")
        self.cfg = make_config(self.folder, protocol={"emailCode": str(self.code),
                                                      "phoneCode": str(self.folder / "phone-code.txt")})
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        FakeAdapter.reset()

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def test_resume_receives_the_code(self):
        job = pipeline.Job(self.folder, self.cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", challenge_at=pipeline.iso(pipeline.time.time() - 30))
        FakeAdapter.reset({"state": "waiting_input", "stage": "phone-sent"})
        job.protocol_step(waiting=True)
        self.assertEqual(FakeAdapter.last_inputs, {"email": "012345"})
        self.assertEqual(job.status["stage"], "phone-sent")
        # The consumed code cannot answer the next challenge.
        with self.assertRaises(pipeline.PipelineError) as caught:
            job.read_input("email")
        self.assertEqual(caught.exception.reason, "input_already_consumed")

    def test_no_code_keeps_waiting(self):
        self.code.unlink()
        job = pipeline.Job(self.folder, self.cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", challenge_at=pipeline.iso())
        FakeAdapter.reset({"state": "waiting_input", "stage": "email-sent"})
        job.protocol_step(waiting=True)
        self.assertEqual(FakeAdapter.last_inputs, {})
        self.assertEqual(job.status["status"], "waiting_input")

    def test_ready_clears_the_challenge(self):
        job = pipeline.Job(self.folder, self.cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", challenge_at=pipeline.iso())
        FakeAdapter.reset({"state": "ready", "reason": "protocol_registration_complete"})
        job.dashboard_handoff = lambda: None
        job.protocol_step(waiting=True)
        self.assertIsNone(job.status.get("challenge_at"))


class CancelClosureTest(unittest.TestCase):
    """cancel is a request; a refund needs a confirmed outcome."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.folder = Path(self.tmp.name)
        Path(self.folder / "email-code.txt").write_text("", encoding="utf-8")
        self.cfg = make_config(self.folder)
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        FakeAdapter.reset()

    def tearDown(self):
        os.environ.pop("APPLESERVICES_PROTOCOL_ADAPTER", None)
        self.tmp.cleanup()

    def _job_with_cancel(self):
        (self.folder / "cancel.request").write_text("1", encoding="utf-8")
        job = pipeline.Job(self.folder, self.cfg)
        job.status.update(stage="email-sent", status="waiting_input", number_owned=True,
                          number="4155550134", challenge_at=pipeline.iso())
        settled = {"count": 0}
        job.settle_number = lambda force=False: settled.__setitem__("count", settled["count"] + 1)
        return job, settled

    def test_unconfirmed_cancel_defer_the_refund(self):
        job, settled = self._job_with_cancel()
        FakeAdapter.reset({"state": "cancelled"})  # no outcome_confirmed
        self.assertTrue(job.protocol_step(waiting=True))
        self.assertEqual(job.status["status"], "needs_review")
        self.assertEqual(job.status["reason"], "cancel_outcome_unconfirmed")
        self.assertTrue(job.status["refund_deferred"])
        self.assertEqual(settled["count"], 0)

    def test_confirmed_cancel_allows_the_refund(self):
        job, settled = self._job_with_cancel()
        FakeAdapter.reset({"state": "cancelled", "outcome_confirmed": True})
        self.assertTrue(job.protocol_step(waiting=True))
        self.assertEqual(job.status["status"], "cancelled")
        self.assertFalse(job.status["refund_deferred"])
        self.assertEqual(FakeAdapter.calls["cancel"], 1)