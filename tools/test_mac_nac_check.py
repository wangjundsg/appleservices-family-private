import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("mac_nac_check", Path(__file__).with_name("mac-nac-check.py"))
nac = importlib.util.module_from_spec(spec)
spec.loader.exec_module(nac)


class CertificateFallbackTests(unittest.TestCase):
    def test_success_keeps_original_route(self):
        with patch.object(nac.subprocess, "run") as run:
            self.assertEqual(nac.fetch_certificate("exchange", "cert.plist"), [])
            self.assertEqual(run.call_count, 1)
            self.assertNotIn("-ipv4", run.call_args.args[0])

    def test_failed_public_get_selects_ipv4_for_following_exchange(self):
        for failure in (subprocess.CalledProcessError(1, "exchange"), subprocess.TimeoutExpired("exchange", 40)):
            with self.subTest(failure=type(failure).__name__), patch.object(nac.subprocess, "run", side_effect=[failure, None]) as run:
                self.assertEqual(nac.fetch_certificate("exchange", "cert.plist"), ["-ipv4"])
                self.assertEqual(run.call_count, 2)
                self.assertEqual(run.call_args.args[0], ["exchange", "-op", "cert", "-out", "cert.plist", "-ipv4"])

    def test_failed_fallback_stops(self):
        failure = subprocess.CalledProcessError(1, "exchange")
        with patch.object(nac.subprocess, "run", side_effect=failure) as run:
            with self.assertRaises(subprocess.CalledProcessError):
                nac.fetch_certificate("exchange", "cert.plist")
            self.assertEqual(run.call_count, 2)


if __name__ == "__main__":
    unittest.main()
