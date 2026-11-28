import unittest
from unittest.mock import patch
import test_pipeline as fixtures
p = fixtures.p


class MailboxAcquisitionTests(unittest.TestCase):
    def setUp(self):
        fixtures.PipelineTests.setUp(self)
        cfg = self.job.cfg
        cfg.update(email="", browser_mode="local", egress_mode="direct", captcha_solver="manual", handoff="dashboard",
                   mail_purchase={"credentialFile": str(self.root / "key.dpapi"), "warehouse": 2, "max_price_vnd": 700},
                   otpgmail_order_file=str(self.root / "otpgmail-order.json"))
        cfg.pop("email_code_file")
        cfg["sms"] = {"auto_purchase": True, "phonecc": "PT"}
        self.job = p.Job(self.root, cfg, allow_purchase=True)
        self.order = {"provider": "otpgmail", "email": "fixture@gmail.com", "orderId": "fixture-order", "warehouse": 2,
                      "credentialFile": cfg["mail_purchase"]["credentialFile"], "priceVnd": 700, "status": "waiting_code"}
        p.atomic(self.root / "input.json", cfg)

    def acquired(self, *args):
        p.atomic(self.root / "otpgmail-order.json", self.order)
        return 0, {"state": "mailbox_ready"}

    def test_new_task_accepts_pending_mailbox_but_keeps_other_validation(self):
        p.validate(self.job.cfg)
        self.assertEqual(self.job.status["stage"], "mailbox")
        for field, value in [("warehouse", 3), ("max_price_vnd", 701), ("credentialFile", "relative")]:
            with patch.dict(self.job.cfg["mail_purchase"], {field: value}):
                with self.assertRaises(p.PipelineError): p.validate(self.job.cfg)
        with patch.dict(self.job.cfg, {"password": "bad"}):
            with self.assertRaises(p.PipelineError): p.validate(self.job.cfg)
        with patch.dict(self.job.cfg, {"blocked_mailbox_domains": ["gmail.com"]}):
            with self.assertRaises(p.PipelineError): p.validate(self.job.cfg)

    def test_duplicate_run_reads_original_task_instead_of_buying(self):
        (self.root / "bin").mkdir()
        for name in ["login.exe", "appleweb.exe", "trusted-device.exe"]:
            (self.root / "bin" / name).touch()
        manifest = self.root / "request.json"
        p.atomic(manifest, self.job.cfg)
        with patch.object(p, "ROOT", self.root), patch.object(p.Job, "run") as run:
            first = p.execute("run", input_file=manifest, allow_purchase=True)
            second = p.execute("run", input_file=manifest, allow_purchase=True)
        run.assert_called_once()
        self.assertEqual(first, second)
        self.assertEqual(first["stage"], "mailbox")

    def test_mailbox_is_bound_before_number_browser_and_activation(self):
        events = []
        def process(*args):
            events.append("mailbox")
            return self.acquired()
        def create():
            events.append("number-and-browser")
            cfg = p.read(self.root / "input.json")
            self.assertEqual(cfg["email"], self.order["email"])
            reservation = p.STATE / "accounts" / (p.hashlib.sha256(cfg["email"].encode()).hexdigest()+".json")
            self.assertEqual(p.read(reservation), cfg["job_id"])
            self.job.update(stage="login", status="queued", account_created=True)
        def handoff():
            events.append("activation")
            self.job.update(status="ready")
        with patch.object(self.job, "process", side_effect=process), patch.object(self.job, "create_with_replacement", side_effect=create), \
             patch.object(self.job, "dashboard_handoff", side_effect=handoff), patch.object(self.job, "settle_number"):
            self.job.run()
        self.assertEqual(events, ["mailbox", "number-and-browser", "activation"])
        self.assertEqual(self.job.status["status"], "ready")

    def test_uncertain_stock_and_pending_purchase_never_buy_a_number(self):
        for reason in ["mailbox_purchase_requires_readback", "mailbox_stock_unavailable", "mailbox_order_pending"]:
            with self.subTest(reason=reason), patch.object(self.job, "process", return_value=(3, {"reason": reason})), \
                 patch.object(self.job, "create_with_replacement") as create, patch.object(self.job, "settle_number") as settle:
                self.job.run(resume=True)
                create.assert_not_called()
                settle.assert_not_called()
                self.assertEqual(self.job.status["stage"], "mailbox")
                self.assertEqual(self.job.status["reason"], reason)

    def test_recovery_after_binding_crash_queries_original_order_and_never_changes_identity(self):
        self.job.cfg["email"] = self.order["email"]
        p.atomic(self.root / "input.json", self.job.cfg)
        with patch.object(self.job, "process", side_effect=self.acquired) as process:
            self.job.acquire_mailbox()
        self.assertEqual(process.call_args.args[0][-1], str(self.root / "otpgmail-order.json"))
        self.assertEqual(self.job.status["stage"], "create")

    def test_mailbox_cannot_belong_to_two_tasks(self):
        reservation = p.STATE / "accounts" / (p.hashlib.sha256(self.order["email"].encode()).hexdigest()+".json")
        p.atomic(reservation, "another-job")
        with patch.object(self.job, "process", side_effect=self.acquired), patch.object(self.job, "create_with_replacement") as create:
            self.job.run()
        create.assert_not_called()
        self.assertEqual(p.read(reservation), "another-job")
        self.assertEqual(self.job.status["status"], "needs_review")

    def test_purchase_authority_and_cancellation_stop_before_provider(self):
        self.job.allow_purchase = False
        with patch.object(self.job, "process") as process:
            self.job.run()
            process.assert_not_called()
        self.job.allow_purchase = True
        (self.root / "cancel.request").write_text("cancel")
        with patch.object(self.job, "process") as process, patch.object(self.job, "settle_number"):
            self.job.run(resume=True)
            process.assert_not_called()
        self.assertEqual(self.job.status["status"], "cancelled")
