import unittest
from unittest.mock import patch
import test_pipeline as fixtures
p = fixtures.p


class LocalBrowserTests(unittest.TestCase):
    setUp = fixtures.PipelineTests.setUp

    def test_creator_failure_never_restarts_local_browser_or_buys_again(self):
        self.job.cfg.update(browser_mode="local", egress_mode="direct")
        self.job.cfg["sms"]["auto_purchase"] = True
        self.job.status["creation_rejected"] = True
        with patch.object(self.job,"create",side_effect=p.PipelineError("registration_rejected","failed")) as create, \
             patch.object(self.job,"settle_number") as settle:
            with self.assertRaisesRegex(p.PipelineError,"registration_rejected"):
                self.job.create_with_replacement()
        self.assertEqual(create.call_count,1)
        settle.assert_not_called()

    def test_local_input_rejection_does_not_replace_or_settle_number(self):
        self.job.cfg.update(browser_mode="local", egress_mode="direct", captcha_solver="manual", handoff="dashboard")
        self.job.cfg["sms"].update(auto_purchase=True, can_get_another_sms=True)
        with patch.object(self.job, "number"), patch.object(self.job, "process", return_value=(2, {"state":"usage"})), \
             patch.object(self.job, "settle_number") as settle, patch.object(self.job, "replace_number") as replace:
            with self.assertRaisesRegex(p.PipelineError, "registration_input_invalid"):
                self.job.create_with_replacement()
        settle.assert_not_called()
        replace.assert_not_called()
        self.assertFalse(self.job.status["registration_started"])
        self.assertFalse(self.job.status["creation_rejected"])

    def test_input_rejection_is_resumable_in_original_task_without_login(self):
        self.job.cfg.update(browser_mode="local", egress_mode="direct", handoff="dashboard")
        self.job.status.update(stage="create",status="needs_review",attempts={"create":1},create_exit_code=2,registration_started=False)
        with patch.object(self.job, "claim_number"), patch.object(self.job, "cleanup_browser"), \
             patch.object(self.job, "create_with_replacement", side_effect=lambda:self.job.update(status="ready")) as create, \
             patch.object(self.job, "authenticate") as login:
            self.job.run(resume=True)
        create.assert_called_once()
        login.assert_not_called()

    def test_input_usage_preserves_paid_number_at_task_exit(self):
        self.job.cfg["sms"]["auto_purchase"]=True
        with patch.object(self.job, "claim_number"), \
             patch.object(self.job, "create_with_replacement", side_effect=p.PipelineError("registration_input_invalid","usage")), \
             patch.object(self.job, "settle_number") as settle:
            self.job.run()
        settle.assert_not_called()
        self.assertEqual(self.job.status["reason"],"registration_input_invalid")

    def test_local_remote_ocr_and_legacy_setting_never_invoke_local_model(self):
        for setting in ["yes","local-first"]:
            with self.subTest(setting=setting):
                self.job.cfg.update(browser_mode="local",egress_mode="direct",captcha_solver=setting,yescaptcha_key="fixture-key")
                with patch.object(self.job,"number"),patch.object(self.job,"process",return_value=(0,{})):
                    self.job.create()
                actual=p.read(self.root/"create-input.json")
                command=p.json.loads(actual["captcha-cmd"])
                self.assertEqual(command,["python","tools/captcha/solver.py","--provider","yes","--skip-local"])
                self.assertTrue(actual["captcha-remote-only"])
                self.assertNotIn("captcha-fallback-cmd",actual)

    def test_remote_ocr_missing_key_fails_preflight_even_for_legacy_setting(self):
        self.job.cfg.update(browser_mode="local",egress_mode="direct",captcha_solver="local-first")
        with patch.dict(p.os.environ,{},clear=True),patch.object(p,"_secret_candidates",return_value=[]):
            with self.assertRaisesRegex(p.PipelineError,"captcha_key_required"):
                p.validate(self.job.cfg)

    def test_local_field_mismatch_is_script_failure_not_registration_rejection(self):
        self.job.cfg.update(browser_mode="local",egress_mode="local",local_browser_proxy="http://127.0.0.1:7897",captcha_solver="local-first")
        with patch.object(self.job,"number"),patch.object(self.job,"process",return_value=(1,{"state":"failed","error":"field_readback_failed","evidence":"field_mismatch:country"})) as process:
            with self.assertRaisesRegex(p.PipelineError,"registration_field_mismatch"):
                self.job.create()
        self.assertFalse(self.job.status["creation_rejected"])
        self.assertEqual(process.call_count,1)

    def test_local_captcha_failure_is_not_registration_rejection(self):
        self.job.cfg.update(browser_mode="local",egress_mode="local",local_browser_proxy="http://127.0.0.1:7897",captcha_solver="local-first")
        with patch.object(self.job,"number"),patch.object(self.job,"process",return_value=(3,{"state":"unknown","evidence":"captcha_refresh_failed"})) as process:
            with self.assertRaisesRegex(p.PipelineError,"registration_captcha_unresolved"):
                self.job.create()
        self.assertFalse(self.job.status["creation_rejected"])
        self.assertEqual(process.call_count,1)

    def test_email_replacement_rebinds_identity_and_reader_but_keeps_sms_ledger(self):
        old=self.job.cfg["email"]
        self.job.cfg.update(browser_mode="local",otpgmail_order_file=str(self.root/"old-mail.json"))
        self.job.allow_purchase=True
        def acquire(*args):
            p.atomic(self.root/"replacement-otpgmail-order.json",{"email":"newfixture@gmail.com"})
            p.atomic(self.root/"replacement-email.json",{"previous_email":old,"email":"newfixture@gmail.com",
                "order_file":str(self.root/"replacement-otpgmail-order.json")})
            return 0,{}
        with patch.object(p,"STATE",self.root/"isolated-state"),patch.object(self.job,"process",side_effect=acquire) as buy:
            self.job.replace_email()
            self.job.replace_email()
        self.assertEqual(buy.call_count,1)
        self.assertEqual(self.job.cfg["email"],"newfixture@gmail.com")
        self.assertEqual(self.job.cfg["sms_account_email"],old)
        self.assertEqual(p.read(self.root/"input.json")["email"],"newfixture@gmail.com")
        self.assertTrue((self.root/"replacement-email-ready.json").exists())
        self.assertEqual(self.job.status["email_replacements"],1)

    def test_email_replacement_without_purchase_authority_does_not_spend(self):
        self.job.cfg.update(browser_mode="local",otpgmail_order_file=str(self.root/"old-mail.json"))
        self.job.allow_purchase=False
        with patch.object(self.job,"process") as buy:
            with self.assertRaisesRegex(p.PipelineError,"replacement_email_requires_input"):
                self.job.replace_email()
        buy.assert_not_called()
    def test_local_browser_uses_existing_executor_without_forge(self):
        self.job.cfg.update(browser_mode="local", egress_mode="local", local_browser_proxy="http://127.0.0.1:7897", captcha_solver="manual")
        self.job.cfg["roxy"] = {}
        p.validate(self.job.cfg)
        with patch.object(self.job, "forge", side_effect=AssertionError("local called Forge")), patch.object(self.job, "process", return_value=(0, {})) as process:
            self.job.create()
        self.assertEqual([c.args[0] for c in process.call_args_list], [["node", "tools/capture/create-account.cjs"]])
        inp = p.read(self.root / "create-input.json")
        self.assertTrue(inp["native-browser"])
        self.assertTrue(inp["headed"])
        self.assertNotIn("cdp-port", inp)
        self.assertNotIn("captcha-cmd", inp)
        self.assertEqual(inp["proxy"], self.job.cfg["local_browser_proxy"])
        self.assertEqual(inp["replacement-number-file"], str(self.root / "replacement-number.json"))
        self.assertEqual(self.job.status["stage"], "login")
        self.assertTrue(self.job.status["account_created"])

    def test_local_proxy_invalid_before_number(self):
        self.job.cfg.update(browser_mode="local", egress_mode="local", local_browser_proxy="http://user:secret@127.0.0.1:7897")
        with patch.object(self.job, "number") as number, patch.object(self.job, "forge", side_effect=AssertionError("unexpected Forge call")):
            with self.assertRaisesRegex(p.PipelineError, "local_browser_proxy_invalid"):
                self.job.create()
        number.assert_not_called()

    def test_local_env_uses_frozen_proxy_without_forge(self):
        self.job.cfg.update(browser_mode="local", egress_mode="local", local_browser_proxy="http://127.0.0.1:7897", roxy={})
        env = self.job.env()
        self.assertEqual(env["HTTPS_PROXY"], self.job.cfg["local_browser_proxy"])

    def test_local_creation_hands_off_new_code_on_replacement_order(self):
        self.job.cfg.update(browser_mode="local", egress_mode="direct", captcha_solver="manual", handoff="dashboard")
        self.job.cfg["sms"].update(auto_purchase=True, phonecc="PT", can_get_another_sms=True)
        replacement = {**self.job.cfg["sms"], "activation": "replacement-order", "phone": "912345678", "can_get_another_sms": True}
        supply = unittest.mock.Mock()
        supply.purchase.return_value = replacement
        def execute(args, *unused):
            self.job.replace_number()
            (self.root / "create-sms.txt").write_text("123456")
            return 0, {}
        observations = iter([{}, {"waiting_sms": True}, {"waiting_sms": True}, {"gsa_verified": True}, {"activated": True, "online": True}])
        actions = []
        def panel(action, **body):
            actions.append((action, body))
            if action == "observe":
                return {"ok": True, **next(observations)}
            return {"ok": True, "terms": "fixture-terms"}
        with patch.object(self.job, "number"), patch.object(self.job, "claim_number"), patch.object(self.job, "supply", return_value=supply), \
             patch.object(self.job, "process", side_effect=execute), patch.object(self.job, "forge", side_effect=AssertionError("unexpected Forge")), \
             patch.object(self.job, "panel_call", side_effect=panel), patch.object(p, "sms_call", side_effect=["ACCESS_RETRY_GET", "STATUS_OK:123456", "STATUS_OK:654321"]) as sms, \
             patch.object(p, "reserve_code"), patch.object(p.time, "sleep"):
            self.job.create()
            self.job.dashboard_handoff()
        self.assertEqual([body["code"] for action, body in actions if action == "sms"], ["654321"])
        self.assertTrue(all(call.kwargs["id"] == "replacement-order" for call in sms.call_args_list))
        self.assertEqual([action for action, body in actions if action != "observe"], ["attach", "start", "sms", "stop", "plan", "activate", "resume"])
        self.assertTrue(self.job.status["handoff_complete"])
        self.assertEqual(self.job.status["status"], "ready")


if __name__ == "__main__":
    unittest.main()
