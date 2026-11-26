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
from unittest.mock import patch

import signup_adapter as sa
import pipeline


class FakeJob:
    def __init__(self, folder: Path, **status):
        self.folder = folder
        self.method = "protocol"
        self.cfg = {"job_id": "reg-1", "email": "fixture@example.test", "password": "Fixture-only123!",
                    "first": "Audrey", "last": "Bennett", "birthday": "1994-03-09",
                    "egress_mode": "direct", "roxy": {},
                    "account_country": "US", "sms": {"phone": "7700900123", "phonecc": "GB"},
                    "protocol": {"emailCode": str(folder / "email-code.txt"),
                                 "phoneCode": str(folder / "phone-code.txt")}}
        self.status = {"job_id": "reg-1", "stage": "create", "status": "queued",
                       "active_run_id": "run-1", **status}

    env = pipeline.Job.env


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

    def waiting_state(self, **changes):
        saved={'email':'fixture@example.test','registrationMethod':'protocol','stage':'email-sent','status':'waiting_input',
               'emailRequestedAt':pipeline.iso(pipeline.time.time()-5),'verificationId':'fixture-verification',
               'operations':{'email-send':{'Digest':'fixture','Status':'succeeded'}},**changes}
        state=self.folder/'protocol'/'state.json';state.parent.mkdir(exist_ok=True)
        state.write_text(json.dumps(saved),encoding='utf8')
        return saved

    def test_exit_four_is_waiting_input(self):
        saved=self.waiting_state()
        adapter = self.adapter(
            """
            import json
            print(json.dumps({"ok": False, "stage": "email-sent", "challenge": {"id": "c1"}}))
            raise SystemExit(4)
            """)
        outcome = adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome["outcome"], "waiting_input")
        self.assertEqual(outcome["registration_phase"], "email-sent")
        self.assertEqual(outcome['sent_at'],saved['emailRequestedAt'])
        self.assertEqual(outcome['verification_id'],'fixture-verification')

    def test_exit_zero_with_bound_evidence_is_registered(self):
        adapter = self.adapter(
            """
            import json,sys,pathlib
            p=pathlib.Path(sys.argv[sys.argv.index('-state')+1]);p.parent.mkdir(parents=True,exist_ok=True)
            p.write_text(json.dumps({'email':'fixture@example.test','created':True,'status':'ready',
                'registrationMethod':'protocol','operations':{'finalize':{'Digest':'fixture','Status':'succeeded'}}}))
            print(json.dumps({"ok": True, "accountExists": True, "state":"ready", "evidenceRef": "e-1"}))
            """)
        outcome = adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome["outcome"], "registered")
        self.assertEqual(outcome["state"], "ready")

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
        with self.assertRaises(sa.AdapterError) as caught:
            adapter.create(FakeJob(self.folder))
        self.assertEqual(caught.exception.reason, "outcome_job_mismatch")

    def test_success_without_private_creation_evidence_stops(self):
        adapter = self.adapter('import json;print(json.dumps({"ok":True,"accountExists":True,"state":"ready"}))')
        with self.assertRaises(sa.AdapterError) as caught:
            adapter.create(FakeJob(self.folder))
        self.assertEqual(caught.exception.reason, "creation_evidence_unconfirmed")

    def test_cli_contract_and_password_transport(self):
        self.waiting_state()
        adapter = self.adapter('''
            import json,sys,os
            args=sys.argv
            assert args[args.index('-email')+1]=='fixture@example.test'
            assert args[args.index('-country')+1]=='USA'
            assert args[args.index('-phonecc')+1]=='GB'
            assert args[args.index('-phone')+1]=='7700900123'
            assert os.environ['APPLESERVICES_SIGNUP_PASSWORD']=='Fixture-only123!'
            assert 'Fixture-only123!' not in args
            assert not os.environ.get('YESCAPTCHA_KEY')
            assert '-device-dir' in args and '-email-code-file' in args
            print(json.dumps({'state':'waiting_input','stage':'email-sent'}))
            raise SystemExit(4)
        ''')
        outcome=adapter.create(FakeJob(self.folder))
        self.assertEqual(outcome.get('state'),'waiting_input')
        self.assertEqual(outcome.get('stage'),'email-sent')

    def test_executor_uses_job_proxy_in_create_and_resume(self):
        job = FakeJob(self.folder)
        job.cfg['egress_mode'] = 'roxy'
        job.cfg['roxy']['proxyInfo'] = {'host': '127.0.0.1', 'port': 1080,
            'proxyUserName': 'fixture-user', 'proxyPassword': 'fixture:p@ss'}
        adapter = self.adapter('''
            import json, os
            expected = 'socks5://fixture-user:fixture%3Ap%40ss@127.0.0.1:1080'
            assert os.environ.get('HTTPS_PROXY') == expected
            assert os.environ.get('HTTP_PROXY') == expected
            assert os.environ.get('NO_PROXY') == 'localhost,127.0.0.1'
            assert 'ALL_PROXY' not in os.environ
            print(json.dumps({'ok': True, 'accountExists': False}))
        ''')
        with patch.dict(os.environ, {'HTTPS_PROXY': 'http://parent.invalid:9999',
                                    'ALL_PROXY': 'http://parent.invalid:9999'}):
            for action in (adapter.create, adapter.resume):
                self.assertEqual(action(job)['outcome'], 'not_applied')

    def test_direct_executor_does_not_inherit_parent_proxy(self):
        adapter = self.adapter('''
            import json, os
            assert not any(k.upper() in {'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY'} for k in os.environ)
            print(json.dumps({'ok': True, 'accountExists': False}))
        ''')
        with patch.dict(os.environ, {'HTTPS_PROXY': 'http://parent.invalid:9999'}):
            for action in (adapter.create, adapter.resume):
                self.assertEqual(action(FakeJob(self.folder))['outcome'], 'not_applied')

    def test_explicit_manual_captcha_file_reaches_executor_with_bounded_wait(self):
        job=FakeJob(self.folder)
        target=self.folder/'protocol'/'captcha-answer.txt'
        job.cfg['protocol']['captchaAnswer']=str(target)
        adapter=sa.SignupAdapter(binary='unused')
        context=sa.ExecutorContext.from_job(job)
        args=adapter._arguments(job,context)
        self.assertEqual(args[args.index('-captcha-answer-file')+1],str(target.resolve()))
        self.assertEqual(args[args.index('-captcha-wait')+1],'60s')
        self.assertNotIn('-captcha-answer',args)

    def test_frozen_identity_reaches_protocol_executor(self):
        job=FakeJob(self.folder)
        args=sa.SignupAdapter(binary='unused')._arguments(job,sa.ExecutorContext.from_job(job))
        for flag, value in (("-first","Audrey"),("-last","Bennett"),("-birthday","1994-03-09")):
            self.assertEqual(args[args.index(flag)+1],value)

    def test_manual_captcha_file_must_not_alias_state_or_leave_task(self):
        job=FakeJob(self.folder)
        for target in (self.folder.parent/'outside.txt',self.folder/'protocol'/'state.json',self.folder/'email-code.txt'):
            job.cfg['protocol']['captchaAnswer']=str(target)
            with self.subTest(target=target),self.assertRaisesRegex(sa.AdapterError,'protocol_captcha_input_invalid'):
                sa.SignupAdapter(binary='unused')._arguments(job,sa.ExecutorContext.from_job(job))

    def test_actual_adapter_waiting_reaches_pipeline_without_new_resource_requests(self):
        saved=self.waiting_state()
        adapter=self.adapter('''
            import json
            print(json.dumps({'state':'waiting_input','stage':'email-sent'}))
            raise SystemExit(4)
        ''')
        cfg=FakeJob(self.folder).cfg
        cfg['registration_method']='protocol'
        job=pipeline.Job(self.folder,cfg)
        job.status['number_owned']=True
        with patch.object(pipeline.adapter_registry,'resolve',return_value=adapter), patch.object(job,'number') as number:
            self.assertTrue(job.protocol_step(action='create'))
        number.assert_not_called()
        self.assertEqual(job.status['status'],'waiting_input')
        self.assertEqual(job.status['challenge']['kind'],'email')
        self.assertFalse(job.status['account_created'])
        self.assertEqual(job.status['challenge']['sent_at'],saved['emailRequestedAt'])

    def test_account_or_finalize_mismatch_cannot_be_registered(self):
        adapter=self.adapter('import json;print(json.dumps({"ok":True,"accountExists":True,"state":"ready"}))')
        job=FakeJob(self.folder)
        state=Path(sa.ExecutorContext.from_job(job).state_path)
        state.parent.mkdir(parents=True,exist_ok=True)
        for change in ({'email':'foreign@example.test'},{'created':False},{'operations':{}},{'status':'unknown'}):
            saved={'email':job.cfg['email'],'registrationMethod':'protocol','created':True,'status':'ready',
                   'operations':{'finalize':{'Status':'succeeded'}},**change}
            state.write_text(json.dumps(saved),encoding='utf8')
            with self.assertRaises(sa.AdapterError):adapter.create(job)

    def test_waiting_cannot_use_foreign_unsent_or_invalid_time_evidence(self):
        adapter=self.adapter('import json;print(json.dumps({"stage":"email-sent"}));raise SystemExit(4)')
        for change in ({'email':'foreign@example.test'},{'operations':{}},{'stage':'phone-sent'},
                       {'emailRequestedAt':pipeline.iso(pipeline.time.time()+60)},
                       {'emailRequestedAt':'2026-11-26T01:00:00'},{'verificationId':None}):
            self.waiting_state(**change)
            with self.assertRaises(sa.AdapterError):adapter.create(FakeJob(self.folder))

    def test_missing_password_cannot_generate_a_different_account_password(self):
        job=FakeJob(self.folder);job.cfg.pop('password')
        with self.assertRaises(sa.AdapterError) as caught:
            self.adapter('raise SystemExit(99)').create(job)
        self.assertEqual(caught.exception.reason,'protocol_password_required')

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
