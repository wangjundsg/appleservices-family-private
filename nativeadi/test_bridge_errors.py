import unittest
from bridge import Bridge, NativeFailure


class BridgeErrors(unittest.TestCase):
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
