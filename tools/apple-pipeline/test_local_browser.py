import unittest
from unittest.mock import patch
import test_pipeline as fixtures
p = fixtures.p


class LocalBrowserTests(unittest.TestCase):
    setUp = fixtures.PipelineTests.setUp
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
