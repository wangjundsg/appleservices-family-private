"""Offline W1/W2 mid-batch audit; baseline ad19776 has known failures."""
import datetime as dt
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(os.environ.get('APPLESERVICES_REVIEW_ROOT', str(Path(__file__).resolve().parents[3])))
sys.path.insert(0, str(ROOT / 'tools/apple-pipeline'))
import job_policy as policy
import pipeline as p
from test_protocol_adapter import make_config


class BatchPolicyReview(unittest.TestCase):
    def test_rate_limit_without_valid_deadline_cannot_create(self):
        for deadline in (None, 'invalid'):
            with self.subTest(deadline=deadline):
                result = policy.decide({'status': 'rate_limited', 'cooldown_until': deadline},
                                       'run', dt.datetime(2026, 11, 16, tzinfo=dt.timezone.utc).timestamp())
                self.assertNotIn(result.action, {'create', 'resume', 'acquire_then_create'})

    def test_cooldown_respects_timezone(self):
        now = dt.datetime(2026, 11, 16, tzinfo=dt.timezone.utc).timestamp()
        result = policy.decide({'status': 'rate_limited',
                                'cooldown_until': '2026-11-15T23:30:00-02:00'}, 'run', now)
        self.assertEqual(result.action, 'hold', 'deadline is still 90 minutes away')

    def test_unrecognized_operation_is_not_permission(self):
        for operation in ('unknown', {'state': 'unknown'}, {'status': 'intent'}):
            with self.subTest(operation=operation):
                result = policy.decide({'status': 'queued', 'operations': {'create': operation}}, 'run', 1)
                self.assertNotIn(result.action, {'create', 'resume', 'acquire_then_create'})

    def test_refund_needs_business_and_sms_evidence(self):
        cases = [
            ({'registration': {'outcome': 'unknown'}, 'registration_started': True},
             {'confirmed_absent': True}),
            ({'registration': {'outcome': 'not_applied'}, 'registration_started': False,
              'run': {'state': 'queued'}}, {}),
        ]
        for job, observation in cases:
            with self.subTest(job=job, observation=observation):
                result = policy.settlement_decision(job, {'id': 'fixture-order', 'state': 'owned'}, observation)
                self.assertNotEqual(result.action, 'refund')

    def test_current_phase_binding_is_required_in_real_run(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            folder = root / 'jobs/reg-test'
            folder.mkdir(parents=True)
            cfg = make_config(folder, protocol={'phoneCode': str(folder / 'phone-code.txt')})
            job = p.Job(folder, cfg)
            job.update(stage='phone-sent', status='waiting_input', number_owned=True,
                       registration_started=True, create_confirmed=True,
                       operations={'create': 'succeeded'}, challenge_stage='email-sent',
                       challenge_at=p.iso(p.time.time()-30))
            adapter = Mock()
            adapter.resume.return_value = {'state': 'waiting_input', 'stage': 'phone-sent'}
            with patch.object(p, 'ROOT', root), patch.object(p, 'STATE', root/'jobs'), \
                    patch.object(p.adapter_registry, 'resolve', return_value=adapter):
                job.run(resume=True)
            self.assertEqual(adapter.resume.call_count, 0, 'email challenge admitted phone resume')

    def test_persisted_stop_reaches_actual_admission(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            folder = root / 'jobs/reg-test'
            folder.mkdir(parents=True)
            job = p.Job(folder, make_config(folder))
            job.update(stage='create', status='queued', number_owned=True, stop_requested=True)
            adapter = Mock()
            adapter.create.return_value = {'state': 'waiting_input', 'stage': 'email-sent'}
            with patch.object(p, 'ROOT', root), patch.object(p, 'STATE', root/'jobs'), \
                    patch.object(p.adapter_registry, 'resolve', return_value=adapter):
                job.run()
            self.assertEqual(adapter.create.call_count, 0, 'persisted stop omitted from policy_view')


if __name__ == '__main__':
    unittest.main()
