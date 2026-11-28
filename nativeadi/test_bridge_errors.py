import unittest
from bridge import Bridge, NativeFailure, local_failure, record_failure, provision_with_retry
import queue
import tempfile
import pathlib
import json
import base64


class BridgeErrors(unittest.TestCase):
    def test_private_failure_evidence_survives_a_second_attempt(self):
        bridge = Bridge.__new__(Bridge)
        bridge.function = 103
        with tempfile.TemporaryDirectory() as folder:
            for raw in [b'\x01private-first-error', b'\x01private-second-error']:
                try:
                    bridge.unwrap(raw)
                except NativeFailure as failure:
                    self.assertTrue(record_failure(folder, failure))
                    self.assertNotIn('private-', str(failure))
            records = [json.loads(p.read_text()) for p in pathlib.Path(folder).glob('failure-*.json')]
            self.assertEqual(len(records), 2)
            self.assertEqual({base64.b64decode(r['native_error_base64']) for r in records},
                             {b'\x01private-first-error', b'\x01private-second-error'})
            self.assertTrue(all(r['function'] == 103 for r in records))
            self.assertFalse(record_failure(None, ValueError('private-value')))

    def test_localized_tcp_timeout_is_retried_before_authentication(self):
        from unittest.mock import Mock
        bridge = Bridge.__new__(Bridge)
        bridge.function = 103
        raw = b'\x01Request Error: error trying to connect: tcp connect error: ' + "连接尝试失败".encode() + b' (os error 10060)'
        with self.assertRaises(NativeFailure) as raised:
            bridge.unwrap(raw)
        failure = raised.exception
        self.assertEqual(failure.code, 'native_connect_timeout')
        with tempfile.TemporaryDirectory() as folder:
            generate = Mock(side_effect=[failure, failure, '123456'])
            wait = Mock()
            self.assertEqual(provision_with_retry(generate, folder, sleep=wait), '123456')
            self.assertEqual(generate.call_count, 3)
            self.assertEqual([c.args[0] for c in wait.call_args_list], [2, 4])
            self.assertEqual(len(list(pathlib.Path(folder).glob('failure-*.json'))), 2)

    def test_initialization_retry_is_bounded_and_never_replays_ambiguous_requests(self):
        from unittest.mock import Mock
        with tempfile.TemporaryDirectory() as folder:
            for code, function, expected in [('native_connect_timeout',103,3),
                    ('native_network_timeout',103,1), ('native_tls_handshake_failed',103,1),
                    ('native_operation_failed',103,1), ('native_connect_timeout',106,1)]:
                action = Mock(side_effect=NativeFailure(code,function))
                with self.assertRaises(NativeFailure):
                    provision_with_retry(action, folder, sleep=lambda _:None)
                self.assertEqual(action.call_count,expected)

    def test_local_failure_is_redacted_and_distinct(self):
        self.assertEqual(local_failure(ValueError('private-token')), 'native_local_value_error')
        self.assertEqual(local_failure(OSError('private-path')), 'native_local_io_error')
        self.assertEqual(local_failure(RuntimeError('private-identity')), 'native_local_operation_failed')
        self.assertEqual(local_failure(queue.Empty()), 'native_bridge_timeout')

    def test_failure_keeps_operation_without_native_payload(self):
        bridge = Bridge.__new__(Bridge)
        bridge.function = 103
        with self.assertRaises(NativeFailure) as raised:
            bridge.unwrap(b'\x01private-token unknown provisioning response')
        self.assertEqual(raised.exception.function, 103)
        self.assertNotIn('private-token', str(raised.exception))

    def test_native_failure_is_classified_without_metadata(self):
        bridge = Bridge.__new__(Bridge)
        cases = [
            (b'unexpected EOF during handshake', 'native_tls_handshake_failed'),
            (b'request timed out', 'native_network_timeout'),
            (b'connection refused', 'native_connection_failed'),
            (b'unknown response', 'native_operation_failed'),
        ]
        for raw, code in cases:
            with self.subTest(code=code), self.assertRaises(NativeFailure) as raised:
                bridge.unwrap(b'\x01' + raw + b' https://private-user:private-password@example.test/private-token')
            self.assertEqual(str(raised.exception), code)
            self.assertEqual(raised.exception.code, code)
        self.assertEqual(bridge.unwrap(b'\x00private-success'), b'private-success')


if __name__ == '__main__':
    unittest.main()
