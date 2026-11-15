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
    """Records calls; returns the configured outcome and never touches the network."""

    instances = []

    def __init__(self):
        self.calls = {"create": 0, "resume": 0, "cancel": 0}
        self.outcome = {"state": "waiting_input", "stage": "email-sent"}
        FakeAdapter.instances.append(self)

    def create(self, job):
        self.calls["create"] += 1
        return dict(self.outcome)

    def resume(self, job, inputs=None):
        self.calls["resume"] += 1
        return dict(self.outcome)

    def cancel(self, job):
        self.calls["cancel"] += 1
        return {"state": "cancelled"}


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
        FakeAdapter.instances.clear()
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
        self.assertTrue(job.protocol_create())

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
                folder = self.folder / label
                folder.mkdir(parents=True, exist_ok=True)
                cfg = make_config(folder, sms=sms)
                job = pipeline.Job(folder, cfg)
                job.status.update(stage="create", status="queued")
                job.save = lambda **kw: None

                counts = {"claim": 0, "settle": 0, "browser": 0, "login": 0}
                job.claim_number = lambda: counts.__setitem__("claim", counts["claim"] + 1)
                job.settle_number = lambda: counts.__setitem__("settle", counts["settle"] + 1)
                job.cleanup_browser = lambda: counts.__setitem__("browser", counts["browser"] + 1)
                job.authenticate = lambda: counts.__setitem__("login", counts["login"] + 1)
                job.begin = lambda stage: None

                job.run()
                self.assertEqual(job.status["status"], "waiting_input")
                self.assertEqual(counts["settle"], 0, "the number was released while waiting")
                self.assertEqual(counts["browser"], 0, "browser cleanup ran for a protocol job")
                self.assertEqual(counts["login"], 0, "the job jumped to the login stage")
                if label == "auto_purchase":
                    self.assertEqual(counts["claim"], 1, "auto purchase should happen before the adapter")

    def test_cancel_is_explicit(self):
        os.environ["APPLESERVICES_PROTOCOL_ADAPTER"] = "test_protocol_adapter:FakeAdapter"
        adapter = protocol_adapter.resolve(None)
        result = adapter.cancel(pipeline.Job(self.folder, make_config(self.folder)))
        self.assertEqual(result["state"], "cancelled")
        self.assertEqual(FakeAdapter.instances[-1].calls["cancel"], 1)

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

        waiting = job.protocol_create()
        self.assertTrue(waiting)
        self.assertEqual(job.status["status"], "waiting_input")
        self.assertEqual(job.status["stage"], "email-sent")
        self.assertEqual(FakeAdapter.instances[-1].calls["create"], 1)
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
