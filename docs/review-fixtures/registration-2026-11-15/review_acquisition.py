"""R01 offline recovery and stable-intent contract checks for 2837794."""
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(os.environ.get('APPLESERVICES_REVIEW_ROOT', str(Path(__file__).resolve().parents[3])))
sys.path.insert(0, str(ROOT/'tools/apple-pipeline'))
import pipeline as p
from sms_supply import Supply, SupplyError
from test_sms_supply import receipt, POLICY
from test_protocol_adapter import make_config


class AcquisitionReview(unittest.TestCase):
    def setUp(self):
        temporary=tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root=Path(temporary.name)
        self.call=Mock(side_effect=[receipt(),receipt(activationId=12346)])
        self.supply=Supply(self.root/'supply','owner@example.test',p.atomic,p.lock,self.call)
        self.policy={**POLICY,'acquisition_id':'fixed-acquisition'}
        env=patch.dict(os.environ,{'SMS_API_KEY':'fixture-only','SMS_API_KEY_HERO':'fixture-only'})
        env.start();self.addCleanup(env.stop)

    def test_binding_write_crash_recovers_order_from_saved_job(self):
        folder=self.root/'jobs/reg-test';folder.mkdir(parents=True)
        cfg=make_config(folder,sms={**POLICY,'auto_purchase':True,'phonecc':'GB'})
        p.atomic(folder/'input.json',cfg)
        actual_atomic=p.atomic
        def fail_binding(path,value):
            if Path(path)==folder/'input.json':raise OSError('fixture binding write failure')
            actual_atomic(path,value)
        with patch.object(p,'ROOT',self.root),patch.object(p,'STATE',self.root/'jobs'),patch.object(p,'sms_call',self.call):
            job=p.Job(folder,cfg,allow_purchase=True)
            with patch.object(p,'atomic',side_effect=fail_binding):
                with self.assertRaises(OSError):job.number()
            saved=p.read(folder/'result.json')
            restored=p.Job(folder,p.read(folder/'input.json'),saved,allow_purchase=True)
            restored.number()
        self.assertEqual(self.call.call_count,1)
        self.assertEqual(restored.cfg['sms']['activation'],'12345')
        self.assertTrue(restored.cfg['sms']['recovered'])
        self.assertEqual(restored.status['acquisition_id'],saved['acquisition_id'])

    def test_same_intent_never_buys_again_after_order_state_changes(self):
        first=self.supply.purchase('job','GB',self.policy,True)
        for result in ('OTP_RECEIVED','REFUND_PENDING','REFUNDED'):
            with self.subTest(result=result):
                ledger=self.supply.read();ledger['spend']=ledger['spend'][:1]
                ledger['spend'][0]['result']=result
                p.atomic(self.supply.path,ledger)
                self.call.reset_mock();self.call.side_effect=None;self.call.return_value=receipt(activationId=12346)
                recovered=None
                try:
                    recovered=self.supply.purchase('job','GB',self.policy,True)
                except SupplyError:
                    pass  # Blocking a settled/uncertain original order is permitted.
                self.assertEqual(self.call.call_count,0,'same acquisition spent again after status changed')
                if recovered:
                    self.assertEqual(recovered['activation'],first['activation'],'same intent returned a new order')

    def test_same_intent_cannot_change_country(self):
        self.supply.purchase('job','GB',self.policy,True)
        with self.assertRaises(SupplyError):
            self.supply.purchase('job','CA',self.policy,True)
        self.assertEqual(self.call.call_count,1)


if __name__=='__main__':unittest.main()
