import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import pipeline as p
from sms_supply import Supply, SupplyError

# Fixture prices only; production config deliberately has no price defaults.
POLICY = {"max_price":"1.10", "max_total":"2.20", "currency":"USD"}

def receipt(**changes):
    value = {"activationId":12345,"phoneNumber":"447700900123","activationCost":"1.01",
             "currency":840,"countryCode":"16","activationTime":"2026-11-15 01:00:00",
             "activationEnd":"2026-11-15 01:20:00","canGetAnotherSms":"1"}
    value.update(changes)
    return json.dumps(value)

class SupplyTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.call=Mock(return_value=receipt())
        self.supply=Supply(Path(self.temp.name),"owner@example.test",p.atomic,p.lock,self.call)
        env=patch.dict(os.environ,{"SMS_API_KEY":"fixture","SMS_API_KEY_HERO":"fixture-hero"})
        env.start();self.addCleanup(env.stop)

    def buy(self, job="job", policy=POLICY):
        return self.supply.purchase(job,"GB",policy,allow=True)

    def test_switch_and_explicit_prices_gate_all_network(self):
        with self.assertRaisesRegex(SupplyError,"purchase_disabled"):
            self.supply.purchase("job","GB",POLICY)
        for policy in ({},dict(POLICY,max_price=""),dict(POLICY,max_price="NaN"),dict(POLICY,currency="RUB")):
            with self.assertRaises(SupplyError): self.buy(policy=policy)
        self.call.assert_not_called()

    def test_only_explicit_no_numbers_allows_fallback(self):
        self.call.side_effect=["NO_NUMBERS",receipt()]
        self.assertEqual(self.buy()["provider"],"hero")
        self.assertEqual(self.call.call_count,2)
        self.assertEqual(self.call.call_args.kwargs["maxPrice"],"1.10")
        self.assertEqual(self.supply.read()["spend"][0]["result"],"NO_PURCHASE")

    def test_timeout_persists_unknown_and_blocks_new_job(self):
        self.call.side_effect=TimeoutError()
        for job in ("job","other-job"):
            with self.assertRaisesRegex(SupplyError,"readback"): self.buy(job)
        self.assertEqual(self.call.call_count,1)

    def test_rejection_does_not_switch_provider(self):
        self.call.return_value="NO_BALANCE"
        with self.assertRaisesRegex(SupplyError,"rejected"):self.buy()
        self.assertEqual(self.call.call_count,1)

    def test_disk_failure_prevents_paid_request(self):
        self.supply.atomic=Mock(side_effect=OSError("full"))
        with self.assertRaises(OSError):self.buy()
        self.call.assert_not_called()

    def test_two_numbers_and_gross_budget_apply_across_jobs(self):
        self.buy("first");self.buy("second")
        with self.assertRaisesRegex(SupplyError,"account_number_limit"):self.buy("third")
        self.assertEqual(self.call.call_count,2)

    def test_budget_checks_decimal_without_recycling_refunds(self):
        self.buy()
        ledger=self.supply.read();ledger["spend"][0]["result"]="REFUNDED";p.atomic(self.supply.path,ledger)
        with self.assertRaisesRegex(SupplyError,"budget_exhausted"):
            self.buy("retry",dict(POLICY,max_total="2.10"))
        self.assertEqual(self.call.call_count,1)

    def test_currency_and_expiry_fail_closed_but_retain_order(self):
        self.call.return_value=receipt(currency=643)
        with self.assertRaisesRegex(SupplyError,"receipt_requires_review"):self.buy()
        self.assertEqual(self.supply.read()["spend"][0]["activation_id"],"12345")
        with self.assertRaisesRegex(SupplyError,"readback"):self.buy("new")
        self.assertEqual(self.call.call_count,1)

    def test_early_cancel_is_swept_later(self):
        self.buy()
        self.call.side_effect=["STATUS_WAIT_CODE","EARLY_CANCEL_DENIED"]
        self.assertTrue(self.supply.settle("job")["refund_pending"])
        before=self.call.call_count
        self.supply.settle("job")
        self.assertEqual(before,self.call.call_count)
        ledger=self.supply.read();ledger["spend"][0]["refund_after"]=0;p.atomic(self.supply.path,ledger)
        self.call.side_effect=["STATUS_WAIT_CODE","ACCESS_CANCEL"]
        summary=self.supply.settle("job")
        self.assertFalse(summary["refund_pending"])
        self.assertEqual(summary["refunded"],"1.01")

    def test_received_sms_never_refunds(self):
        result=self.buy();self.supply.otp_received(result["supply_operation"])
        self.call.reset_mock()
        summary=self.supply.settle("job")
        self.call.assert_not_called()
        self.assertTrue(summary["otp_received"])
        self.assertEqual(summary["gross"],"1.01")

    def test_refund_readback_detects_sms_before_cancel(self):
        self.buy();self.call.reset_mock();self.call.return_value="STATUS_OK:012345"
        self.assertTrue(self.supply.settle("job")["otp_received"])
        self.assertEqual(self.call.call_count,1)

if __name__=="__main__":unittest.main()
