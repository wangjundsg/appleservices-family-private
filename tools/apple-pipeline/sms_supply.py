"""Account-scoped purchase and settlement ledger. No implicit paid requests."""
import datetime as dt
from decimal import Decimal, InvalidOperation
import hashlib
import json
import os
import re
from pathlib import Path
import time

PROVIDERS = {
    "grizzly": ("https://api.grizzlysms.com/stubs/handler_api.php", "SMS_API_KEY"),
    "hero": ("https://hero-sms.com/stubs/handler_api.php", "SMS_API_KEY_HERO"),
}
COUNTRIES = {"GB": ("16", "44"), "CA": ("36", "1"), "ID": ("6", "62"), "US": ("12", "1"), "VN": ("10", "84")}

class SupplyError(Exception):
    pass

def price(value):
    try:
        result = Decimal(str(value))
        if not result.is_finite() or result <= 0: raise ValueError()
        return result
    except (InvalidOperation, ValueError): raise SupplyError("purchase_price_required") from None

class Supply:
    def __init__(self, root, email, atomic, lock, call):
        self.account = hashlib.sha256(email.strip().lower().encode()).hexdigest()
        self.path = Path(root) / (self.account + ".json")
        self.atomic, self.lock, self.call = atomic, lock, call

    def read(self):
        return json.loads(self.path.read_text(encoding="utf-8")) if self.path.exists() else {"schema_version":1,"account_id":self.account,"spend":[]}

    def provider(self, name):
        url, env = PROVIDERS[name]
        key = os.environ.get(env)
        if not key: raise SupplyError("sms_provider_key_missing")
        return {"api":url,"key":key}

    def purchase(self, job_id, country, policy, allow=False):
        if not allow: raise SupplyError("purchase_disabled")
        if country not in COUNTRIES: raise SupplyError("purchase_country_not_allowed")
        maximum, budget = price(policy.get("max_price")), price(policy.get("max_total"))
        if policy.get("currency") != "USD" or maximum > budget: raise SupplyError("purchase_budget_invalid")
        country_id, dial = COUNTRIES[country]
        with self.lock(self.path.with_suffix(".lock")):
            ledger = self.read()
            rows = ledger["spend"]
            # Retain all purchase intents across jobs. Unknown purchase never changes provider.
            if any(row["result"] in {"PURCHASE_PENDING","PURCHASE_UNKNOWN","RECEIPT_INVALID"} for row in rows):
                raise SupplyError("purchase_requires_readback")
            bought = [row for row in rows if row["result"] != "NO_PURCHASE"]
            if len(bought) >= 2: raise SupplyError("account_number_limit")
            exposure = sum((Decimal(row.get("price") or row["reserved"]) for row in bought), Decimal(0))
            if exposure + maximum > budget: raise SupplyError("purchase_budget_exhausted")
            # At most one successful acquisition per call; only NO_NUMBERS permits fallback.
            for provider in ("grizzly", "hero"):
                cfg = self.provider(provider)
                now = time.time()
                row = {"operation_id":f"sms:{self.account}:{job_id}:{len(rows)+1}", "job_id":job_id,
                       "provider":provider,"country":country,"currency":"USD","price":None,
                       "reserved":str(maximum),"result":"PURCHASE_PENDING","requested_at":now,
                       "activation_id":None,"refund_attempts":0}
                rows.append(row); self.atomic(self.path, ledger)
                try:
                    answer = self.call(cfg, action="getNumberV2",service="wx",country=country_id,maxPrice=str(maximum))
                except Exception:
                    row["result"]="PURCHASE_UNKNOWN"; self.atomic(self.path,ledger)
                    raise SupplyError("purchase_requires_readback") from None
                if answer == "NO_NUMBERS":
                    row["result"]="NO_PURCHASE"; row["reason"]="NO_NUMBERS"; self.atomic(self.path,ledger)
                    continue
                if answer in {"NO_BALANCE","BAD_KEY","BAD_SERVICE","SERVICE_UNAVAILABLE_REGION"}:
                    row["result"]="NO_PURCHASE"; row["reason"]=answer; self.atomic(self.path,ledger)
                    raise SupplyError("number_purchase_rejected")
                try:
                    result=json.loads(answer)
                    if not isinstance(result,dict): raise ValueError()
                    activation=str(result["activationId"]); phone=str(result["phoneNumber"])
                    if not activation.isdigit() or not phone.isdigit() or not phone.startswith(dial): raise ValueError()
                    row["activation_id"]=activation
                    actual=price(result["activationCost"])
                    row["price"]=str(actual); row["currency"]=str(result["currency"])
                    # Never label an unverified currency/price as USD spending.
                    if str(result["currency"]) not in {"840","USD"} or actual>maximum: raise ValueError()
                    if str(result.get("countryCode",country_id)) != country_id: raise ValueError()
                    start=dt.datetime.fromisoformat(result["activationTime"])
                    end=dt.datetime.fromisoformat(result["activationEnd"])
                    duration=(end-start).total_seconds()
                    if not 0 < duration <= 1800: raise ValueError()
                    expires=now+duration  # Same provider clock for both times; conservative request start.
                    row.update(result="PURCHASED",currency="USD",expires_at=expires,
                               phone=phone,can_get_another_sms=str(result.get("canGetAnotherSms"))=="1")
                    self.atomic(self.path,ledger)
                    return {**cfg,"activation":activation,"phone":phone[len(dial):],"phonecc":country,
                            "expires_at":dt.datetime.fromtimestamp(expires,dt.timezone.utc).isoformat(),
                            "supply_operation":row["operation_id"],"provider":provider,
                            "can_get_another_sms":row["can_get_another_sms"]}
                except (KeyError, TypeError, ValueError, SupplyError):
                    row["result"]="RECEIPT_INVALID"; self.atomic(self.path,ledger)
                    raise SupplyError("purchase_receipt_requires_review") from None
            raise SupplyError("number_unavailable")

    def otp_received(self, operation):
        with self.lock(self.path.with_suffix(".lock")):
            ledger=self.read()
            for row in ledger["spend"]:
                if row["operation_id"]==operation:
                    row["result"]="OTP_RECEIVED"; row["otp_received"]=True
            self.atomic(self.path,ledger)

    def settle(self, job_id):
        with self.lock(self.path.with_suffix(".lock")):
            ledger=self.read()
            for row in ledger["spend"]:
                if row["job_id"]!=job_id or not row.get("activation_id") or row["result"] in {"REFUNDED","OTP_RECEIVED"}: continue
                if time.time()<row.get("refund_after",0) or row.get("refund_attempts",0)>=5: continue
                try:
                    cfg=self.provider(row["provider"])
                    status=self.call(cfg,action="getStatus",id=row["activation_id"])
                    if re.fullmatch(r"STATUS_(?:OK|WAIT_RETRY):\d{4,8}",status):
                        row.update(result="OTP_RECEIVED",otp_received=True); self.atomic(self.path,ledger); continue
                    if status == "STATUS_CANCEL":
                        row["result"]="REFUNDED"; self.atomic(self.path,ledger); continue
                    if status not in {"STATUS_WAIT_CODE","STATUS_WAIT_RETRY","STATUS_WAIT_RESEND"}:
                        row["refund_after"]=time.time()+130; row["refund_pending"]=True; self.atomic(self.path,ledger); continue
                    row.update(refund_attempts=row.get("refund_attempts",0)+1,refund_pending=True,refund_after=time.time()+130)
                    self.atomic(self.path,ledger)  # Intent before sending the cancellation.
                    reply=self.call(cfg,action="setStatus",status="8",id=row["activation_id"])
                    if reply in {"ACCESS_CANCEL","ACCESS_CANCEL_ALREADY"}: row.update(result="REFUNDED",refund_pending=False)
                    else: row["result"]="REFUND_PENDING"
                except Exception:
                    row.update(refund_pending=True,refund_after=time.time()+130)
                self.atomic(self.path,ledger)
            rows=[row for row in ledger["spend"] if row["job_id"]==job_id]
            valid=[row for row in rows if row.get("activation_id")]
            return {"purchases":len(valid),"currency":"USD",
                    "gross":str(sum((Decimal(r["price"]) for r in rows if r.get("price") and r["currency"]=="USD"),Decimal(0))),
                    "refunded":str(sum((Decimal(r["price"]) for r in rows if r["result"]=="REFUNDED" and r.get("price") and r["currency"]=="USD"),Decimal(0))),
                    "otp_received":any(r.get("otp_received") for r in rows),
                    "refund_pending":any(r.get("refund_pending") and r["result"] not in {"REFUNDED","OTP_RECEIVED"} for r in rows)}
