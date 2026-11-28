import unittest
from unittest.mock import patch
import test_pipeline as fixture
p=fixture.p


class ActivationRecoveryTests(unittest.TestCase):
    setUp=fixture.PipelineTests.setUp
    def test_unauthorized_reauthenticates_once_and_ignores_old_worker(self):
        self.job.cfg['handoff']='dashboard'
        sequence=iter([{}, {'worker_pid':9,'failed':True},
            {'worker_pid':10,'failed':True,'failure_reason':'automatic_activation_unauthorized'},
            {'worker_pid':10,'failed':True,'failure_reason':'automatic_activation_unauthorized'},
            {'worker_pid':11,'waiting_sms':True},
            {'worker_pid':11,'activated':True,'online':True},
            {'worker_pid':11,'activated':True,'online':True}])
        actions=[]
        def call(action,**body):
            actions.append(action)
            if action=='observe': return next(sequence)
            return {'ok':True,'worker_pid':11 if action=='restart-activation' else 10}
        with patch.object(self.job,'panel_call',side_effect=call), patch.object(self.job,'next_code',return_value='123456'), patch.object(p,'sms_call',return_value='ACCESS_RETRY_GET'), patch.object(p.time,'sleep'):
            self.job.dashboard_handoff()
        self.assertEqual(actions.count('restart-activation'),1)
        self.assertIn('sms-restart',actions)
        self.assertNotIn('activate-saved',actions)
        self.assertEqual(self.job.status['status'],'ready')

    def test_second_unauthorized_does_not_loop(self):
        sequence=iter([{}, {'failed':True,'failure_reason':'automatic_activation_unauthorized'}, {'failed':True,'failure_reason':'automatic_activation_unauthorized'}])
        actions=[]
        def call(action,**body):
            actions.append(action)
            return next(sequence) if action=='observe' else {'ok':True}
        with patch.object(self.job,'panel_call',side_effect=call), patch.object(p,'sms_call',return_value='ACCESS_RETRY_GET'), patch.object(p.time,'sleep'):
            with self.assertRaisesRegex(p.PipelineError,'panel_activation_unauthorized'):
                self.job.dashboard_handoff()
        self.assertEqual(actions.count('restart-activation'),1)

    def test_ambiguous_terms_write_never_starts_fresh_login(self):
        sequence=iter([{}, {'failed':True,'failure_reason':'automatic_activation_terms_unconfirmed'}])
        actions=[]
        def call(action,**body):
            actions.append(action)
            return next(sequence) if action=='observe' else {'ok':True}
        with patch.object(self.job,'panel_call',side_effect=call):
            with self.assertRaisesRegex(p.PipelineError,'panel_activation_terms_failed'):
                self.job.dashboard_handoff()
        self.assertNotIn('restart-activation',actions)

    def test_confirmed_middle_failures_restart_complete_activation(self):
        for failure in ('automatic_activation_nac_failed','automatic_activation_delegate_refused','automatic_activation_terms_unavailable'):
            with self.subTest(failure=failure):
                self.job.status['operations']={}
                self.job.status.pop('activation_recovery_started',None)
                sequence=iter([{}, {'failed':True,'failure_reason':failure}, {'activated':True,'online':True}, {'activated':True,'online':True}])
                actions=[]
                def call(action,**body):
                    actions.append(action)
                    return next(sequence) if action=='observe' else {'ok':True}
                with patch.object(self.job,'panel_call',side_effect=call),patch.object(p,'sms_call',return_value='ACCESS_RETRY_GET'),patch.object(p,'reserve_code'),patch.object(p.time,'sleep'):
                    self.job.dashboard_handoff()
                self.assertEqual(actions.count('restart-activation'),1)
                self.assertNotIn('activate-saved',actions)

    def test_unknown_restart_result_cannot_trigger_another_login(self):
        with patch.object(p,'sms_call',return_value='ACCESS_RETRY_GET'),patch.object(self.job,'panel_call',side_effect=p.PipelineError('transport_result_unknown')) as call:
            with self.assertRaisesRegex(p.PipelineError,'transport_result_unknown'): self.job.restart_activation()
            with self.assertRaisesRegex(p.PipelineError,'recovery_exhausted'): self.job.restart_activation()
        call.assert_called_once()


if __name__=='__main__': unittest.main()
