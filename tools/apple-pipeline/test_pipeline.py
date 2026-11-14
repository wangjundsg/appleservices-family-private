import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("pipeline", Path(__file__).with_name("pipeline.py"))
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


def config():
    return {"job_id": "test-job", "email": "owner@example.test", "password": "Test-Only123!", "accept_terms": True,
            "email_code_file": str(Path(tempfile.gettempdir()) / "unused-test-code.txt"), "sms": {"activation": "test-activation", "key": "test-key", "phonecc": "GB",
            "phone": "7700900123", "expires_at": p.iso(p.time.time() + 600)},
            "roxy": {"token": "test-token", "workspaceId": "test-workspace", "projectId": "test-project",
            "locale": "en-GB", "timeZone": "Europe/London", "proxyInfo": {"protocol": "SOCKS5", "host": "localhost", "port": 1080}}}


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.job = p.Job(self.root, config())
        state_patch = patch.object(p, "STATE", self.root / "state")
        state_patch.start()
        self.addCleanup(state_patch.stop)

    def test_budget_survives_restarts_and_refuses_third_request(self):
        p.reserve_code(self.job.cfg["sms"], self.root)
        p.reserve_code(dict(self.job.cfg["sms"]), self.root)
        with self.assertRaises(p.PipelineError): p.reserve_code(self.job.cfg["sms"], self.root)

    def test_budget_disk_failure_prevents_request(self):
        with patch.object(p, "atomic", side_effect=OSError("disk full")):
            with self.assertRaises(OSError): p.reserve_code(self.job.cfg["sms"], self.root)

    def test_lock_blocks_other_process_and_releases(self):
        path = self.root / "job.lock"
        script = "import sys;sys.path.insert(0,sys.argv[1]);import pipeline\nwith pipeline.lock(sys.argv[2]):pass"
        args = [sys.executable, "-c", script, str(Path(__file__).parent), str(path)]
        with p.lock(path):
            self.assertNotEqual(subprocess.run(args, capture_output=True).returncode, 0)
        self.assertEqual(subprocess.run(args, capture_output=True).returncode, 0)

    def test_restart_after_unknown_create_never_calls_create(self):
        self.job.update(status="unknown", attempts={"create": 1})
        with patch.object(self.job, "create") as create, patch.object(self.job, "authenticate", side_effect=p.PipelineError("test-stop")):
            self.job.run(resume=True)
        create.assert_not_called()
        self.assertEqual(self.job.status["stage"], "login")

    def test_ambiguous_device_write_is_not_replayed(self):
        self.job.update(stage="device", status="running", attempts={"device": 1})
        with patch.object(self.job, "device") as device:
            self.job.run(resume=True)
        device.assert_not_called()
        self.assertEqual(self.job.status["status"], "needs_review")

    def test_full_chain_advances_each_stage_once(self):
        def advance(stage): return lambda: self.job.update(stage=stage, status="queued")
        with patch.object(self.job, "create", side_effect=advance("login")) as create, \
             patch.object(self.job, "authenticate", side_effect=advance("gsa")) as web, \
             patch.object(self.job, "gsa", side_effect=advance("device")) as gsa, \
             patch.object(self.job, "device", side_effect=advance("keepalive")) as device, \
             patch.object(self.job, "keepalive", side_effect=lambda: self.job.update(status="ready")) as alive:
            self.job.run()
            self.job.run()
        for method in (create, web, gsa, device, alive): method.assert_called_once()
        self.assertEqual(self.job.status["status"], "ready")

    def test_receipt_contains_no_credentials(self):
        self.job.update(status="waiting_input")
        receipt = (self.root / "result.json").read_text()
        for secret in ("Test-Only123!", "test-key", "test-token", "test-activation", "owner@example.test"):
            self.assertNotIn(secret, receipt)

    def test_original_registration_code_cannot_feed_login(self):
        (self.root / "create-sms.txt").write_text("123456")
        with patch.object(p, "sms_call", return_value="STATUS_OK:123456"):
            self.assertEqual(self.job.next_code(), "")
        with patch.object(p, "sms_call", return_value="STATUS_OK:654321"):
            self.assertEqual(self.job.next_code(), "654321")
            self.assertEqual(self.job.next_code(), "")

    def test_unknown_create_keeps_number_until_expiry(self):
        self.job.update(status="unknown", number_owned=True)
        with patch.object(p, "sms_call") as sms:
            self.job.settle_number()
        sms.assert_not_called()

    def test_refund_cooldown_is_persisted_without_secret(self):
        self.job.update(status="failed", number_owned=True)
        with patch.object(p, "sms_call", return_value="EARLY_CANCEL_DENIED"):
            self.job.settle_number()
        self.assertTrue(self.job.status["refund_pending"])
        self.assertGreater(p.timestamp(self.job.status["refund_after"]), p.time.time())

    def test_process_consumes_split_events_and_never_publishes_raw_output(self):
        script = 'import sys,time;print("private-token");sys.stdout.write(\'{"state":"suc\');sys.stdout.flush();time.sleep(.05);print(\'cess"}\')'
        code, event = self.job.process([sys.executable, "-c", script], 5)
        self.assertEqual((code, event["state"]), (0, "success"))
        self.assertFalse((self.root / "events.jsonl").exists())

    def test_manifest_is_validated_before_resources_are_used(self):
        p.validate(config())
        cfg = config(); cfg["roxy"]["proxyInfo"]["protocol"] = "HTTP"
        with self.assertRaises(p.PipelineError): p.validate(cfg)
        cfg = config(); cfg["password"] = "NoSpecial123"
        with self.assertRaises(p.PipelineError): p.validate(cfg)

    def test_duplicate_run_and_second_job_for_same_account_do_not_run_again(self):
        manifest = self.root / "request.json"
        cfg = config()
        p.atomic(manifest, cfg)
        (self.root / "bin").mkdir()
        for name in ("login.exe", "appleweb.exe", "trusted-device.exe"):
            (self.root / "bin" / name).touch()
        def run(job, **kwargs): job.update(status="ready")
        with patch.object(p, "ROOT", self.root), patch.object(p, "STATE", self.root / "state"), patch.object(p.Job, "run", autospec=True, side_effect=run) as runner:
            first = p.execute("run", input_file=manifest)
            self.assertEqual(p.execute("run", input_file=manifest), first)
            runner.assert_called_once()
            cfg["job_id"] = "second-job"
            p.atomic(manifest, cfg)
            with self.assertRaisesRegex(p.PipelineError, "account_already_has_a_job"):
                p.execute("run", input_file=manifest)

    def test_budget_failure_does_not_start_login(self):
        with patch.object(p, "reserve_code", side_effect=p.PipelineError("budget_unavailable")), patch.object(self.job, "process") as process:
            with self.assertRaises(p.PipelineError): self.job.gsa()
        process.assert_not_called()

    def test_direct_exit_clears_inherited_proxies(self):
        self.job.cfg["egress_mode"] = "direct"
        del self.job.cfg["roxy"]["proxyInfo"]
        p.validate(self.job.cfg)
        with patch.dict(p.os.environ, {"HTTP_PROXY": "http://wrong", "all_proxy": "http://wrong"}):
            self.assertFalse(any(k.upper().endswith("_PROXY") for k in self.job.env()))

    def test_cancel_stops_child_without_refunding_an_unknown_registration(self):
        self.job.update(registration_started=True, number_owned=True)
        def cancel():
            p.time.sleep(.15)
            (self.root / "cancel.request").write_text("cancel")
        thread = p.threading.Thread(target=cancel)
        thread.start()
        started = p.time.monotonic()
        with self.assertRaisesRegex(p.PipelineError, "cancelled_requires_readback"):
            self.job.process([sys.executable, "-c", "import time; time.sleep(60)"], 65)
        thread.join()
        self.assertLess(p.time.monotonic() - started, 10)
        self.job.update(status="cancelled")
        with patch.object(p, "sms_call") as sms: self.job.settle_number()
        sms.assert_not_called()
        self.assertTrue(self.job.status["refund_deferred"])

    def test_cancel_before_submission_refunds_and_does_not_repeat_refund(self):
        (self.root / "cancel.request").write_text("cancel")
        with patch.object(self.job, "create") as create, patch.object(p, "sms_call", return_value="ACCESS_CANCEL") as sms:
            self.job.run()
            self.job.settle_number()
        create.assert_not_called()
        sms.assert_called_once()
        self.assertEqual(self.job.status["status"], "cancelled")
        self.assertTrue(self.job.status["refund_confirmed"])

    def test_rate_limit_stops_chain_and_persists_cooldown(self):
        self.job.update(stage="gsa")
        with patch.object(self.job, "gsa", side_effect=p.PipelineError("code_rate_limited", "rate_limited")), patch.object(self.job, "device") as device:
            self.job.run()
        device.assert_not_called()
        self.assertEqual(self.job.status["status"], "rate_limited")
        self.assertGreater(p.timestamp(self.job.status["cooldown_until"]), p.time.time() + 3500)

    def test_maintenance_recovers_crash_without_replaying_registration(self):
        folder = p.STATE / self.job.cfg["job_id"]
        p.atomic(folder / "input.json", self.job.cfg)
        p.atomic(folder / "result.json", {**self.job.status, "active": True, "registration_started": True})
        with patch.object(p.Job, "create") as create:
            p.maintain_once()
        create.assert_not_called()
        self.assertEqual(p.read(folder / "result.json")["status"], "needs_review")

    def test_maintenance_skips_a_job_held_by_an_active_child(self):
        folder = p.STATE / self.job.cfg["job_id"]
        p.atomic(folder / "input.json", self.job.cfg)
        p.atomic(folder / "result.json", {**self.job.status, "active": True})
        with p.lock(folder / ".lock"): p.maintain_once()
        self.assertTrue(p.read(folder / "result.json")["active"])

    def test_exit_four_probes_account_without_replaying_form(self):
        self.job.cfg["sms"]["expires_at"] = p.iso(p.time.time() - 1)
        with patch.object(self.job, "process", return_value=(0, {"state": "account_exists"})) as process:
            with self.assertRaisesRegex(p.PipelineError, "account_exists_activation_expired"):
                self.job.authenticate()
        self.assertIn("probe", process.call_args.args[0])
        self.assertTrue(self.job.status["account_created"])

    def test_cancelled_unknown_checks_account_at_expiry_before_refund(self):
        self.job.cfg["sms"]["expires_at"] = p.iso(p.time.time() - 1)
        self.job.update(status="cancelled", registration_started=True, number_owned=True)
        (self.root / "cancel.request").write_text("cancel")
        with patch.object(self.job, "process", return_value=(0, {"state": "account_exists"})) as process, patch.object(p, "sms_call") as sms:
            self.job.settle_number()
        sms.assert_not_called()
        self.assertIn("probe", process.call_args.args[0])
        self.assertTrue(process.call_args.kwargs["ignore_cancel"])
        self.assertTrue(self.job.status["account_created"])


    def test_dashboard_handoff_uses_existing_panel_and_no_legacy_executors(self):
        self.job.cfg["handoff"]="dashboard"
        seen=[]
        observations=iter([{"waiting_sms":True},{"gsa_verified":True},{"activated":True,"online":True}])
        def panel(action, **body):
            seen.append(action)
            if action=="observe": return {"ok":True,**next(observations)}
            if action=="plan":return {"ok":True,"terms":"fixture-terms"}
            return {"ok":True}
        with patch.object(self.job,"panel_call",side_effect=panel), patch.object(self.job,"next_code",return_value="002026"), \
             patch.object(p.time,"sleep"), patch.object(self.job,"process") as process:
            self.job.authenticate()
        process.assert_not_called()
        self.assertEqual(seen,["attach","start","observe","sms","observe","stop","plan","activate","resume","observe"])
        self.assertEqual(self.job.status["status"],"ready")
        self.assertTrue(self.job.status["handoff_complete"])
        self.assertNotIn("002026",(self.root/"result.json").read_text())

    def test_handoff_unknown_and_changed_intent_cannot_replay(self):
        with patch.object(self.job,"panel_call",side_effect=p.PipelineError("transport_result_unknown")) as call:
            for _ in range(2):
                with self.assertRaises(p.PipelineError):self.job.panel_once("activate",terms="v1")
            self.assertEqual(call.call_count,1)
        self.job.status["operations"]={}
        with patch.object(self.job,"panel_call",return_value={"ok":True}) as call:
            self.job.panel_once("activate",terms="v1")
            with self.assertRaises(p.PipelineError):self.job.panel_once("activate",terms="v2")
            self.assertEqual(call.call_count,1)

    def test_dashboard_unknown_registration_does_not_start_another_login(self):
        self.job.cfg["handoff"]="dashboard"
        with patch.object(self.job,"create",side_effect=p.PipelineError("transport_result_unknown")), \
             patch.object(self.job,"authenticate") as auth,patch.object(self.job,"dashboard_handoff") as handoff, \
             patch.object(self.job,"settle_number"):
            self.job.run()
        auth.assert_not_called();handoff.assert_not_called()

    def test_followup_sms_intent_prevents_retry_after_timeout(self):
        (self.root/"create-sms.txt").write_text("123456")
        with patch.object(p,"sms_call",side_effect=p.PipelineError("transport_result_unknown")) as call:
            for _ in range(2):
                with self.assertRaises(p.PipelineError):self.job.prepare_followup_sms()
            self.assertEqual(call.call_count,1)

    def test_safe_number_replacement_stops_after_second_failure(self):
        self.job.cfg["sms"].update(auto_purchase=True,max_price="1.10",max_total="2.20",currency="USD")
        with patch.object(self.job,"create",side_effect=p.PipelineError("number_cannot_receive_followup","failed")) as create, \
             patch.object(self.job,"settle_number"):
            with self.assertRaisesRegex(p.PipelineError,"account_number_limit"):self.job.create_with_replacement()
        self.assertEqual(create.call_count,2)

    def test_legacy_purchase_cannot_bypass_explicit_gate(self):
        self.job.cfg["sms"].pop("activation")
        with patch.object(p,"sms_call") as call:
            with self.assertRaisesRegex(p.PipelineError,"purchase_policy_required"):self.job.number()
        call.assert_not_called()

if __name__ == "__main__": unittest.main()
