import sys, tempfile, unittest, os
from pathlib import Path
from unittest.mock import Mock, patch
sys.path.insert(0, str(Path(os.environ.get('APPLESERVICES_REVIEW_ROOT', str(Path(__file__).resolve().parents[3]))) / 'tools/apple-pipeline'))
import pipeline as p
from test_protocol_adapter import make_config
from test_sms_supply import receipt, POLICY

class SystemReview(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(); self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name); self.folder = self.root / 'jobs/reg-test'; self.folder.mkdir(parents=True)
        self.cfg = make_config(self.folder, protocol={'emailCode': str(self.folder/'email-code.txt')})
        for item in (patch.object(p,'ROOT',self.root), patch.object(p,'STATE',self.root/'jobs'),
                     patch.dict(os.environ,{'SMS_API_KEY':'fixture-only','SMS_API_KEY_HERO':'fixture-only'})):
            item.start(); self.addCleanup(item.stop)

    def test_purchase_is_recovered_after_manifest_write_crash(self):
        self.cfg['sms'] = dict(POLICY, auto_purchase=True, phonecc='GB')
        p.atomic(self.folder/'input.json',self.cfg)
        job=p.Job(self.folder,self.cfg,allow_purchase=True)
        actual_atomic=p.atomic
        def fail_input(path,value):
            if Path(path)==self.folder/'input.json': raise OSError('fixture interrupted write')
            actual_atomic(path,value)
        call=Mock(side_effect=[receipt(),receipt(activationId=12346)])
        with patch.object(p,'sms_call',call):
            with patch.object(p,'atomic',side_effect=fail_input):
                with self.assertRaises(OSError): job.number()
            # Restart with the unchanged manifest after the successful ledger write.
            restored=p.Job(self.folder,p.read(self.folder/'input.json'),
                           p.read(self.folder/'result.json'),allow_purchase=True)
            restored.number()
        self.assertEqual(call.call_count,1,'same acquisition bought a second order after restart')

    def test_manual_refund_checks_received_sms(self):
        job=p.Job(self.folder,self.cfg)
        job.update(status='failed',stage='create',number_owned=True,registration_started=True,creation_rejected=True)
        calls=[]
        def provider(cfg,**query):
            calls.append(query)
            return 'STATUS_OK:012345' if query['action']=='getStatus' else 'ACCESS_CANCEL'
        with patch.object(p,'sms_call',side_effect=provider): job.settle_number()
        self.assertFalse(any(q['action']=='setStatus' for q in calls),repr(calls))

    def test_stopped_task_cannot_resume_after_cancel_command_archived(self):
        job=p.Job(self.folder,self.cfg)
        job.update(status='cancelled',stage='email-sent',number_owned=True,registration_started=True,
                   create_confirmed=True,operations={'create':'succeeded'},challenge_stage='email-sent',
                   challenge_at=p.iso(p.time.time()-20),reason='cancelled_confirmed')
        adapter=Mock();adapter.resume.return_value={'state':'waiting_input','stage':'email-sent'}
        with patch.object(p.adapter_registry,'resolve',return_value=adapter):job.run(resume=True)
        self.assertEqual(adapter.resume.call_count+adapter.create.call_count,0)

    def test_input_read_before_dispatch_is_not_consumption(self):
        job=p.Job(self.folder,self.cfg)
        job.update(status='waiting_input',stage='email-sent',challenge_stage='email-sent',challenge_at=p.iso(p.time.time()-20))
        (self.folder/'email-code.txt').write_text('012345')
        job.collect_inputs() # crash here, before any adapter call
        restored=p.Job(self.folder,self.cfg,p.read(self.folder/'result.json'))
        try: restored.collect_inputs()
        except p.PipelineError as error: self.fail('undelivered input is permanently consumed: '+error.reason)

if __name__=='__main__':unittest.main()
