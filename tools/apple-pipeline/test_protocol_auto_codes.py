"""Automatic reads are offline here; no Apple, mailbox or SMS requests."""
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import pipeline as p
from test_protocol_adapter import make_config

class AutomaticCodeTests(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory();self.addCleanup(temporary.cleanup)
        self.folder=Path(temporary.name)
        cfg=make_config(self.folder,protocol={'auto_codes':True},code_pool_config=str(self.folder/'client.json'))
        cfg.pop('email_code_file')
        self.job=p.Job(self.folder,cfg)
        self.challenge('email')

    def challenge(self,kind):
        self.job.update(stage='email-sent' if kind=='email' else 'phone-sent',status='waiting_input')
        challenge=self.job.open_challenge(kind,sent_at=p.iso(p.time.time()-5))
        challenge.update(job_id=self.job.cfg['job_id'],stage=self.job.status['stage'])
        self.job.update(challenge=challenge)

    def mail(self,args,**kwargs):
        request=json.loads(kwargs['input'])
        self.assertEqual(request['reader'],'outlook')
        self.assertEqual(request['mailbox'],self.job.cfg['email'])
        self.assertEqual(request['since'],p.timestamp(self.job.status['challenge']['sent_at'])*1000)
        p.atomic(request['output'],{'challenge_id':request['challenge_id'],'code':'001234','message_id':'message-1','received_at':p.iso()})
        return SimpleNamespace(returncode=0,stdout='{"found":true}')

    def test_email_read_is_bound_and_restart_uses_saved_result_without_another_read(self):
        with patch.object(p.subprocess,'run',side_effect=self.mail) as run:
            self.assertEqual(self.job.collect_inputs(),{'email':'001234'})
        self.assertEqual(run.call_count,1)
        self.assertEqual(p.read(self.folder/'seen-protocol-mail.json'),['message-1'])
        self.job._input_path('email').unlink()
        restarted=p.Job(self.folder,self.job.cfg,status=p.read(self.folder/'result.json'))
        with patch.object(p.subprocess,'run') as run:
            self.assertEqual(restarted.collect_inputs(),{'email':'001234'})
        run.assert_not_called()
        self.assertNotIn('001234',(self.folder/'events.jsonl').read_text())

    def test_manual_input_arriving_during_read_wins(self):
        def receive(args,**kwargs):
            result=self.mail(args,**kwargs)
            target=self.job._input_path('email');target.parent.mkdir(parents=True,exist_ok=True)
            target.write_text('009999')
            return result
        with patch.object(p.subprocess,'run',side_effect=receive):
            self.assertEqual(self.job.collect_inputs(),{'email':'009999'})

    def test_otpgmail_order_reaches_shared_reader_without_a_purchase_or_resend(self):
        self.job.cfg.pop('code_pool_config')
        self.job.cfg['otpgmail_order_file']=str(self.folder/'otpgmail-order.json')
        p.validate(self.job.cfg)
        def receive(args,**kwargs):
            request=json.loads(kwargs['input'])
            self.assertEqual(request['reader'],'otpgmail')
            self.assertEqual(request['file'],self.job.cfg['otpgmail_order_file'])
            self.assertEqual(request['mailbox'],self.job.cfg['email'])
            p.atomic(request['output'],{'challenge_id':request['challenge_id'],'code':'001234',
                'message_id':'otpgmail:fixture','received_at':p.iso()})
            return SimpleNamespace(returncode=0,stdout='{"found":true}')
        with patch.object(p.subprocess,'run',side_effect=receive) as run:
            self.assertEqual(self.job.collect_inputs(),{'email':'001234'})
        self.assertEqual(run.call_count,1)

    def test_absent_mail_remains_waiting_without_a_code_file(self):
        with patch.object(p.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout='{"found":false}')):
            self.assertEqual(self.job.collect_inputs(),{})
        self.assertFalse(self.job._input_path('email').exists())

    def test_malformed_reader_reply_and_saved_record_fail_closed(self):
        for reply in ('[]','{}','{"found":"false"}'):
            with self.subTest(reply=reply),patch.object(p.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout=reply)):
                with self.assertRaisesRegex(p.PipelineError,'protocol_mail_read_failed'):self.job.collect_inputs()
        record=self.folder/'auto-codes'/(self.job.status['challenge']['id']+'.json')
        for saved in ([],{'challenge_id':self.job.status['challenge']['id'],'code':'001234','received_at':'invalid'}):
            p.atomic(record,saved)
            with self.subTest(saved=saved),patch.object(p.subprocess,'run') as run:
                with self.assertRaisesRegex(p.PipelineError,'automatic_input_binding_invalid'):self.job.collect_inputs()
            run.assert_not_called()
        self.assertFalse(self.job._input_path('email').exists())

    def test_manual_mode_and_foreign_challenge_do_not_read_resources(self):
        with patch.object(p.subprocess,'run') as run,patch.object(p,'sms_call') as sms:
            self.job.cfg['protocol']['auto_codes']=False
            self.assertEqual(self.job.collect_inputs(),{})
            self.job.cfg['protocol']['auto_codes']=True
            self.job.status['challenge']['job_id']='foreign'
            with self.assertRaisesRegex(p.PipelineError,'binding_invalid'):self.job.collect_inputs()
        run.assert_not_called();sms.assert_not_called()

    def test_dispatched_input_is_never_refetched_or_replayed(self):
        target=self.job._input_path('email');target.parent.mkdir(parents=True,exist_ok=True);target.write_text('001234')
        _,digest=self.job._parse_input('email');self.job.dispatch_input(digest)
        with patch.object(p.subprocess,'run') as run:
            with self.assertRaisesRegex(p.PipelineError,'already_consumed'):self.job.collect_inputs()
        run.assert_not_called()

    def test_sms_is_saved_before_marking_consumed_and_recovery_does_not_poll(self):
        self.challenge('phone')
        with patch.object(p,'sms_call',return_value='STATUS_OK:000987') as sms:
            self.assertEqual(self.job.collect_inputs(),{'phone':'000987'})
        self.assertEqual(sms.call_args.kwargs,{'action':'getStatus','id':'12345'})
        self.assertEqual(p.read(self.folder/'used-codes.json'),['000987'])
        self.job._input_path('phone').unlink()
        with patch.object(p,'sms_call') as sms:
            self.assertEqual(self.job.collect_inputs(),{'phone':'000987'})
        sms.assert_not_called()

    def test_sms_persistence_failure_does_not_consume_the_code(self):
        self.challenge('phone')
        with patch.object(p,'sms_call',return_value='STATUS_OK:000987'),patch.object(p,'atomic',side_effect=OSError('fixture disk full')):
            with self.assertRaises(OSError):self.job.collect_inputs()
        self.assertFalse((self.folder/'used-codes.json').exists())

if __name__=='__main__':unittest.main()
