"""Offline application-level checks for 76454cb; no live providers/executors."""
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(os.environ.get('APPLESERVICES_REVIEW_ROOT', str(Path(__file__).resolve().parents[3])))
sys.path.insert(0, str(ROOT / 'tools/apple-pipeline'))
import pipeline as p
import job_policy
from test_protocol_adapter import make_config


class DecisionApplicationReview(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.folder = self.root / 'jobs/reg-test'
        self.folder.mkdir(parents=True)
        self.cfg = make_config(self.folder, protocol={'emailCode': str(self.folder/'email-code.txt')})
        self.job = p.Job(self.folder, self.cfg)
        self.adapter = Mock()
        self.adapter.resume.return_value = {'state': 'waiting_input', 'stage': 'email-sent'}
        self.adapter.cancel.return_value = {'state': 'cancelled', 'outcome_confirmed': False}
        for handle in (patch.object(p, 'ROOT', self.root), patch.object(p, 'STATE', self.root/'jobs'),
                       patch.object(p.adapter_registry, 'resolve', return_value=self.adapter)):
            handle.start()
            self.addCleanup(handle.stop)

    def waiting(self, owner='reg-test'):
        self.job.update(stage='email-sent', status='waiting_input', number_owned=True,
                        registration_started=True, create_confirmed=True,
                        operations={'create':'succeeded'}, challenge_at=p.iso(p.time.time()-30),
                        challenge_stage='email-sent',
                        challenge={'id':'old-challenge','job_id':owner,'kind':'email',
                                   'stage':'email-sent','status':'open','source_id':'old-verification'})

    def test_probe_cannot_override_otp_refund_hold(self):
        self.cfg['sms']['expires_at'] = p.iso(p.time.time()-60)
        self.job.update(stage='create',status='failed',number_owned=True,registration_started=True,
                        code_received=True,sms_state='otp_received')
        provider = Mock(return_value='ACCESS_CANCEL')
        with patch.object(self.job,'process',return_value=(1,None)), patch.object(p,'sms_call',provider):
            self.job.settle_number()
        self.assertFalse(any(call.kwargs.get('action')=='setStatus' for call in provider.call_args_list),
                         'probe failure bypassed hold and sent setStatus=8 despite OTP evidence')

    def test_other_job_challenge_cannot_resume(self):
        self.waiting(owner='different-job')
        self.job.run(resume=True)
        self.assertEqual(self.adapter.resume.call_count,0,'policy_view lost own job_id')

    def test_missing_challenge_binding_is_not_valid(self):
        decision=job_policy.decide({'job_id':'reg-test','stage':'email-sent','status':'waiting_input',
                                   'create_confirmed':True,'challenge':{'id':'unbound'}},'run',p.time.time())
        self.assertNotEqual(decision.action,'resume')

    def test_same_stage_new_verification_opens_new_challenge(self):
        self.waiting()
        self.adapter.resume.return_value={'state':'waiting_input','stage':'email-sent',
                                          'verification_id':'new-verification'}
        self.job.run(resume=True)
        current=self.job.status['challenge']
        self.assertNotEqual(current['id'],'old-challenge','same-stage resend kept old challenge')
        self.assertEqual(current['source_id'],'new-verification')

    def test_persisted_stop_is_applied_as_stop(self):
        self.waiting()
        self.job.update(stop_requested=True)
        self.job.run(resume=True)
        self.assertEqual(self.adapter.create.call_count+self.adapter.resume.call_count,0)
        self.assertEqual(self.adapter.cancel.call_count,1,'stop Decision was collapsed into review')


if __name__ == '__main__':
    unittest.main()
