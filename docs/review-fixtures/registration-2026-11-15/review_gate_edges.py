import os
import sys, tempfile, unittest
from pathlib import Path
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(os.environ.get('APPLESERVICES_REVIEW_ROOT', str(Path(__file__).resolve().parents[3]))) / 'tools/apple-pipeline'))
import pipeline as p
from test_protocol_adapter import make_config


class GateEdges(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.folder = self.root / 'jobs/reg-test'
        self.folder.mkdir(parents=True)
        self.cfg = make_config(self.folder, protocol={
            'emailCode': str(self.folder / 'email-code.txt'),
            'phoneCode': str(self.folder / 'phone-code.txt')})
        self.adapter = Mock()
        self.adapter.resume.return_value = {'state': 'waiting_input', 'stage': 'email-sent'}
        for item in (patch.object(p, 'ROOT', self.root), patch.object(p, 'STATE', self.root / 'jobs'),
                     patch.object(p.adapter_registry, 'resolve', return_value=self.adapter)):
            item.start()
            self.addCleanup(item.stop)
        self.job = p.Job(self.folder, self.cfg)
        self.job.update(stage='email-sent', status='waiting_input', number_owned=True,
                        registration_started=True, attempts={'create': 1}, create_confirmed=True,
                        operations={'create': 'succeeded'}, challenge_stage='email-sent',
                        challenge_at=p.iso(p.time.time() - 30))
        p.atomic(self.folder / 'input.json', self.cfg)

    def test_future_cooldown_stops_real_resume(self):
        until = p.iso(p.time.time() + 1800)
        self.job.update(status='rate_limited', reason='code_rate_limited', cooldown_until=until)
        result = p.execute('resume', job_id=self.cfg['job_id'])
        self.assertEqual(self.adapter.resume.call_count + self.adapter.create.call_count, 0,
                         f'cooling task reached adapter; status={result["status"]}, reason={result["reason"]}')
        self.assertEqual(result['status'], 'rate_limited')
        self.assertEqual(result['reason'], 'code_rate_limited')
        self.assertEqual(result['cooldown_until'], until)

    def test_structured_pending_operation_preserves_review_reason(self):
        # This is the operation record shape used by Job.panel_once in this module.
        self.job.update(status='running', reason='phone_outcome_pending',
                        operations={'create': 'succeeded', 'phone': {'state': 'intent', 'started_at': p.iso()}})
        self.job.run(resume=True)
        self.assertEqual(self.adapter.resume.call_count + self.adapter.create.call_count, 0)
        self.assertEqual(self.job.status['status'], 'needs_review', repr(self.job.status))
        self.assertEqual(self.job.status['reason'], 'phone_outcome_pending')


if __name__ == '__main__':
    unittest.main()
