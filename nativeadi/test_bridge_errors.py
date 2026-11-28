import unittest
from bridge import Bridge, NativeFailure, local_failure
import queue


class BridgeErrors(unittest.TestCase):
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
