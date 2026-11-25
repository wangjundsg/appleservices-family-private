"""Independent Apple pipeline. Only private input files contain credentials.

run --input FILE creates one idempotent job; resume never replays an unknown registration.
status --job ID prints a sanitized receipt; keepalive --job ID validates its session.
No background job starts merely by importing this module.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import secrets
import subprocess
import threading
import time
import urllib.parse
import urllib.request
from sms_supply import Supply, SupplyError

ROOT = Path(__file__).resolve().parents[2]
PROBE_STATES = {"account_exists", "requires_2fa", "authenticated"}
STATE = Path(os.environ.get("APPLESERVICES_PIPELINE_STATE", ROOT / "state" / "pipeline")).resolve()
STAGES = ("create", "login", "terms", "gsa", "device", "keepalive", "subscription", "family")
COUNTRIES = {"GB": "GBR", "US": "USA", "CA": "CAN", "AU": "AUS", "CN": "CHN", "DE": "DEU",
             "FR": "FRA", "SG": "SGP", "HK": "HKG", "JP": "JPN", "ID": "IDN", "VN": "VNM", "PL":"POL", "CO":"COL"}


import job_policy
import protocol_adapter as adapter_registry


class PipelineError(Exception):
    def __init__(self, reason, state="unknown"):
        self.reason, self.state = reason, state
        super().__init__(reason)


def atomic(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(path.name + "." + secrets.token_hex(8) + ".tmp")
    try:
        with open(temp, "x", encoding="utf-8") as f:
            os.chmod(temp, 0o600)
            json.dump(value, f, ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)


@contextlib.contextmanager
def lock(path):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a+b") as f:
        if f.tell() == 0:
            f.write(b"0")
            f.flush()
        f.seek(0)
        try:
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(f.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise PipelineError("job_busy", "busy") from None
        try:
            yield
        finally:
            f.seek(0)
            if os.name == "nt":
                msvcrt.locking(f.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(f, fcntl.LOCK_UN)


def read(path):
    return json.loads(Path(path).read_text(encoding="utf-8-sig"))


def timestamp(value):
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None: raise PipelineError("expiry_requires_timezone", "usage")
    return parsed.timestamp()


def iso(value=None):
    return dt.datetime.fromtimestamp(time.time() if value is None else value, dt.timezone.utc).isoformat()


def job_path(job):
    if not isinstance(job, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", job):
        raise PipelineError("invalid_job_id", "usage")
    return STATE / job


def account_country(cfg):
    country = cfg.get("account_country", "US")
    if not isinstance(country, str) or country not in COUNTRIES:
        raise PipelineError("unsupported_account_country", "usage")
    return COUNTRIES[country]


def validate(cfg):
    job_path(cfg.get("job_id"))
    account_country(cfg)
    if not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", cfg.get("email", "")):
        raise PipelineError("email_required", "usage")
    if cfg["email"].rsplit("@", 1)[1].lower() in {"outlook.com", "icloud.com"}:
        raise PipelineError("unsupported_registration_mailbox", "usage")
    pw = cfg.get("password", "")
    if len(pw) < 8 or not all(re.search(p, pw) for p in [r"[A-Z]", r"[a-z]", r"\d", r"[^A-Za-z0-9]"]):
        raise PipelineError("password_requires_four_character_classes", "usage")
    if sum(bool(cfg.get(k)) for k in ("graph_token_file", "email_code_file", "code_pool_config")) != 1:
        raise PipelineError("choose_one_email_code_source", "usage")
    if not Path(cfg.get("graph_token_file") or cfg.get("email_code_file") or cfg.get("code_pool_config")).is_absolute():
        raise PipelineError("mailbox_code_source_requires_absolute_path", "usage")
    if cfg.get("graph_token_file") and not Path(cfg["graph_token_file"]).is_file():
        raise PipelineError("mailbox_token_file_missing", "usage")
    sms = cfg.get("sms", {})
    if (not sms.get("auto_purchase") and not sms.get("key")) or not re.fullmatch(r"[A-Z]{2}", sms.get("phonecc", "")):
        raise PipelineError("sms_credentials_and_country_required", "usage")
    if sms["phonecc"] not in COUNTRIES:
        raise PipelineError("unsupported_form_country", "usage")
    if sms.get("activation"):
        if not str(sms.get("phone", "")).isdigit() or not sms.get("expires_at"):
            raise PipelineError("activation_phone_and_expiry_required", "usage")
        if timestamp(sms["expires_at"]) <= time.time():
            raise PipelineError("activation_expired", "usage")
    elif not sms.get("auto_purchase") and not all(sms.get(k) for k in ("country", "dial_code", "max_price")):
        raise PipelineError("number_purchase_country_dial_code_and_price_required", "usage")
    roxy = cfg.get("roxy", {})
    proxy = roxy.get("proxyInfo", {})
    if not all(roxy.get(k) for k in ("token", "workspaceId", "projectId", "locale", "timeZone")):
        raise PipelineError("roxy_settings_required", "usage")
    if cfg.get("egress_mode", "roxy") not in {"roxy", "direct"}:
        raise PipelineError("invalid_egress_mode", "usage")
    if cfg.get("egress_mode") != "direct" and (proxy.get("protocol", "").upper() != "SOCKS5" or not proxy.get("host") or not 0 < int(proxy.get("port", 0)) < 65536):
        raise PipelineError("residential_socks5_required", "usage")
    if cfg.get("accept_terms") is not True:
        raise PipelineError("terms_consent_required", "usage")
    if cfg.get("captcha_solver") not in (None, "manual", "yes"):
        raise PipelineError("unsupported_captcha_solver", "usage")
    if cfg.get("captcha_solver") == "yes" and not cfg.get("yescaptcha_key"):
        raise PipelineError("captcha_key_required", "usage")
    snake, camel = cfg.get("registration_method"), cfg.get("registrationMethod")
    if snake and camel and snake != camel:
        raise PipelineError("registration_method_conflict", "usage")
    method = snake or camel or "browser"
    if method not in {"browser", "protocol"}:
        raise PipelineError("invalid_registration_method", "usage")


def request(url, body=None, headers=None, timeout=25):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            return response.read(2 << 20).decode()
    except Exception:
        # URLs may contain provider credentials. Do not propagate urllib errors.
        raise PipelineError("transport_result_unknown") from None


def sms_call(cfg, **params):
    endpoint = cfg.get("api", "https://api.grizzlysms.com/stubs/handler_api.php")
    return request(endpoint + "?" + urllib.parse.urlencode({"api_key": cfg["key"], **params})).strip()


def reserve_code(sms, state_root=None):
    state_root = STATE if state_root is None else state_root
    key = hashlib.sha256((sms.get("api", "grizzly") + ":" + str(sms["activation"])).encode()).hexdigest()
    path = state_root / "budgets" / (key + ".json")
    with lock(path.with_suffix(".lock")):
        budget = read(path) if path.exists() else {"requests": 0}
        if budget["requests"] >= 2:
            raise PipelineError("code_request_budget_exhausted", "waiting_input")
        budget["requests"] += 1
        atomic(path, budget)  # Persist before the process can request a code; failures stop the run.


class Job:
    SUPPORTED_SCHEMA = 2

    @classmethod
    def guard_schema(cls, status):
        """Refuse a status written by a newer build instead of guessing."""
        if not status:
            return status
        version = status.get("schema_version")
        if isinstance(version, int) and version > cls.SUPPORTED_SCHEMA:
            raise PipelineError("schema_unsupported", "needs_review")
        return status

    @staticmethod
    def upgrade_status(status):
        """Convert an older schema explicitly, once, at load time.

        A job written before challenges had identities stored only a timestamp and
        a stage. Converting it here (and marking it legacy) keeps the decision
        table honest: it never invents an open challenge on the fly.
        """
        if not status or status.get("challenge"):
            return status
        stage, stamp = status.get("challenge_stage"), status.get("challenge_at")
        if not stage or not stamp:
            return status
        status["challenge"] = {
            "id": "legacy-" + str(stamp),
            "kind": { "email-sent": "email", "phone-sent": "phone" }.get(stage, stage),
            "stage": stage,
            "status": "open",
            "sent_at": stamp,
            "job_id": status.get("job_id"),
            "legacy": True,
        }
        return status

    def __init__(self, folder, cfg, status=None, allow_purchase=False):
        self.folder, self.cfg = Path(folder), cfg
        status = self.guard_schema(status)
        status = self.upgrade_status(status) if status else status
        self.method = cfg.get("registration_method") or cfg.get("registrationMethod") or "browser"
        self.allow_purchase = allow_purchase
        self.status = status or {"job_id": cfg["job_id"], "stage": "create", "status": "queued",
                                 "attempts": {}, "account_created": False, "trust_verified": False}

    def update(self, **values):
        self.status.update(values, updated_at=iso(), schema_version=2,
                           account_id=hashlib.sha256(self.cfg["email"].strip().lower().encode()).hexdigest(),
                           revision=self.status.get("revision", 0)+1)
        atomic(self.folder / "result.json", self.status)
        with open(self.folder / "events.jsonl", "a", encoding="utf-8") as f:
            f.write(json.dumps({k: self.status.get(k) for k in ("stage", "status", "reason", "updated_at")}) + "\n")

    def env(self):
        env = dict(os.environ)
        # Debug switches in another terminal must not turn the pipeline into a credential logger.
        for key in ("IPM_DEBUG_APPLE_AUTH", "APPLESERVICES_DELEGATE_DUMP", "APPLESERVICES_FRESH_GSA"):
            env.pop(key, None)
        env.update(APPLESERVICES_APPLEID=self.cfg["email"], APPLESERVICES_PASSWORD=self.cfg["password"],
                   APPLESERVICES_CREATE_INPUT=str(self.folder / "create-input.json"))
        if self.cfg.get("yescaptcha_key"): env["YESCAPTCHA_KEY"] = self.cfg["yescaptcha_key"]
        for key in list(env):
            if key.upper() in {"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}: env.pop(key)
        proxy = self.cfg["roxy"].get("proxyInfo", {})
        user = urllib.parse.quote(str(proxy.get("proxyUserName", "")), safe="")
        password = urllib.parse.quote(str(proxy.get("proxyPassword", "")), safe="")
        auth = f"{user}:{password}@" if user else ""
        if self.cfg.get("egress_mode") != "direct":
            address = f"socks5://{auth}{proxy['host']}:{proxy['port']}"
            env.update(HTTP_PROXY=address, HTTPS_PROXY=address, NO_PROXY="localhost,127.0.0.1")
        profile = self.folder / "profile.json"
        if profile.exists():
            seen = read(profile)
            env.update(APPLESERVICES_UA=seen["ua"], APPLESERVICES_LOCALE=seen["locale"])
            offset = -int(seen.get("offset", 0))
            env["APPLESERVICES_GMT"] = f"GMT{'+' if offset >= 0 else '-'}{abs(offset) // 60:02d}:{abs(offset) % 60:02d}"
        return env

    def forge(self, action, **body):
        cfg = self.cfg["roxy"]
        url = cfg.get("api", "http://127.0.0.1:50100") + "/browser/" + action
        result = json.loads(request(url, {"workspaceId": cfg["workspaceId"], **body},
                                    {"token": cfg["token"], "Content-Type": "application/json"}))
        if result.get("code") != 0 or result.get("success") is not True:
            if action == "delete" and "EPERM" in json.dumps(result):
                raise PipelineError("browser_directory_busy")
            raise PipelineError("browser_" + action + "_rejected")
        return result.get("data", {})

    def cleanup_browser(self):
        resource = self.folder / "browser.json"
        if not resource.exists(): return
        identity = read(resource)["dirId"]
        try: self.forge("close", dirId=identity)
        except PipelineError: pass
        for attempt in range(5):
            try:
                result = self.forge("delete", dirIds=[identity])
                if identity not in result.get("deleted", []):
                    raise PipelineError("browser_delete_unconfirmed")
                resource.unlink()
                self.update(cleanup_pending=False)
                return
            except PipelineError as e:
                if e.reason != "browser_directory_busy": break
                time.sleep(1.5 * (attempt + 1))
        self.update(cleanup_pending=True)

    def number(self):
        sms = self.cfg["sms"]
        if sms.get("activation"):
            self.claim_number()
            return
        if sms.get("auto_purchase"):
            # Persist the acquisition intent before spending: the same intent must
            # resolve to the same order after a crash.
            acquisition = self.status.get("acquisition_id")
            if not acquisition:
                acquisition = hashlib.sha256(f"{self.cfg['job_id']}:{time.time_ns()}".encode()).hexdigest()[:32]
                self.update(acquisition_id=acquisition, reason="number_purchase_pending")
            policy = {**sms, "acquisition_id": acquisition}
            try: purchased = self.supply().purchase(self.cfg["job_id"], sms["phonecc"], policy, self.allow_purchase)
            except SupplyError as e: raise PipelineError(str(e), "needs_review") from None
            self.cfg["sms"] = {**sms, **purchased}
            atomic(self.folder / "input.json", self.cfg)
            self.claim_number()
            return
        # Legacy receipts cannot prove paid price/currency/expiry. Require the bounded adapter.
        raise PipelineError("purchase_policy_required", "usage")

    def protocol_step(self, waiting=None, action=None):
        """One delegation to the adapter: create the first time, resume afterwards.

        Returns True when the job must stay waiting — the caller then leaves the
        run loop without releasing anything.
        """
        sms = self.cfg["sms"]
        if sms.get("expires_at") and timestamp(sms["expires_at"]) <= time.time():
            # Expiry stops the job; it never buys another number on its own.
            self.update(status="needs_review", stage=self.status["stage"], reason="number_expired",
                        refund_deferred=bool(self.status.get("number_owned") and not self.status.get("account_created")))
            raise PipelineError("number_expired", "needs_review")
        # Decide what this job may do before touching anything. A job whose
        # outcome is unknown is not a job that may start over.
        if action is None:
            action = self.protocol_action()
        if (self.folder / "cancel.request").exists():
            # An explicit stop outranks the review gate here too: protocol_step is
            # also called directly, not only from run().
            return self.protocol_cancel(adapter_registry.resolve(self.cfg.get("protocol")))
        if action == "review":
            # Keep the reason that made this job unresolved; do not rewrite it as
            # a waiting state and do not call the adapter at all.
            raise PipelineError(self.status.get("reason") or "registration_outcome_unconfirmed", "needs_review")
        # Resolve first: an absent adapter must stop the job before the number is
        # bought, on a resume exactly as on a fresh run.
        adapter = adapter_registry.resolve(self.cfg.get("protocol"))
        if not self.status.get("number_owned"):
            # Same lifecycle as the browser path: buy when allowed, claim the
            # activation, and record it before the adapter sees anything.
            self.number()
        if waiting is None:
            waiting = self.status.get("status") == "waiting_input"
        if (self.folder / "cancel.request").exists():
            return self.protocol_cancel(adapter)
        inputs = self.collect_inputs() if action == "resume" else {}
        if action == "create":
            self.update(registration_started=True, create_attempted=True)
            outcome = adapter.create(self) or {}
        else:
            outcome = adapter.resume(self, inputs) or {}
        state = outcome.get("state")
        if outcome.get("outcome") == "inconclusive" and outcome.get("failure") == "server_rejected_34607001":
            status = outcome.get("http_status")
            self.update(status="needs_review", reason="server_rejected_34607001",
                        http_status=status if type(status) is int and 100 <= status <= 599 else None)
            raise PipelineError("server_rejected_34607001", "needs_review")
        if state == "waiting_input":
            # A new stage means a new challenge: rebind the timestamp so the code
            # from the previous stage cannot answer this one.
            stage_now = outcome.get("stage") or self.status["stage"]
            kind = self.challenge_kind_for(stage_now)
            current = self.status.get("challenge") or {}
            rotated = bool(outcome.get("verification_id")) and outcome.get("verification_id") != current.get("source_id")
            if (current.get("stage") != stage_now or current.get("status") not in {"open", "sent"}
                    or rotated):
                # Sending a code for a new stage is a new challenge: it gets its own
                # identity and is bound to this job and stage.
                challenge = self.open_challenge(kind or stage_now, source_id=outcome.get("verification_id", ""),sent_at=outcome.get("sent_at"))
                challenge["stage"] = stage_now
                challenge["job_id"] = self.cfg["job_id"]
                self.update(challenge=challenge, challenge_at=challenge["sent_at"], challenge_stage=stage_now)
            self.update(status="waiting_input", stage=outcome.get("stage") or self.status["stage"],
                        reason=outcome.get("reason") or "waiting_input", active=False,
                        create_confirmed=True, operations={**(self.status.get("operations") or {}),
                                                           "create": "succeeded"})
            return True
        if state == "ready":
            # A new challenge gets a new timestamp; the old one must not admit an
            # already-delivered code.
            self.update(stage="login", status="queued", account_created=True, challenge_at=None,
                        challenge_stage=None, challenge=None,
                        reason=outcome.get("reason") or "protocol_registration_complete")
            return False
        raise PipelineError(outcome.get("reason") or "protocol_adapter_failed",
                            outcome.get("status") or "needs_review")

    def claim_number(self):
        sms = self.cfg["sms"]
        key = hashlib.sha256((sms.get("api", "grizzly") + ":" + str(sms["activation"])).encode()).hexdigest()
        if key == self.status.get("number_ref") and (self.status.get("refund_confirmed") or self.status.get("refund_pending")):
            raise PipelineError("replace_settled_activation_before_retry", "waiting_input")
        reservation = STATE / "numbers" / (key + ".json")
        with lock(reservation.with_suffix(".lock")):
            if reservation.exists() and read(reservation) != self.cfg["job_id"]:
                raise PipelineError("activation_assigned_to_another_job", "needs_review")
            atomic(reservation, self.cfg["job_id"])
        if key != self.status.get("number_ref"):
            self.update(number_owned=True, number_ref=key, refund_attempts=0, refund_pending=False, refund_confirmed=False, refund_after=None)

    def next_code(self):
        previous = self.folder / "used-codes.json"
        used = read(previous) if previous.exists() else []
        created = self.folder / "create-sms.txt"
        if created.exists(): used.append(created.read_text().strip())
        code = sms_call(self.cfg["sms"], action="getStatus", id=self.cfg["sms"]["activation"])
        if not re.fullmatch(r"STATUS_OK:\d{6}", code): return ""
        if self.cfg["sms"].get("supply_operation"):
            self.supply().otp_received(self.cfg["sms"]["supply_operation"])
        code = code.split(":")[1]
        if code in used: return ""
        atomic(previous, used + [code])
        return code

    def process(self, args, timeout, code_mode=None, ignore_cancel=False):
        if not ignore_cancel: self.check_cancel()
        messages = queue.Queue()
        end = time.monotonic() + timeout
        output = ""
        pending_lines = ""
        final = None
        waiting = False
        code_delivered = False
        with subprocess.Popen(args, cwd=ROOT, env=self.env(), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0)) as child:
            def consume():
                while True:
                    chunk = child.stdout.read1(4096)
                    if not chunk: break
                    messages.put(chunk.decode("utf-8", errors="replace"))
            thread = threading.Thread(target=consume, daemon=True)
            thread.start()
            next_poll = 0
            heartbeat = 0
            try:
                while child.poll() is None or thread.is_alive() or not messages.empty():
                    if not ignore_cancel: self.check_cancel()
                    if time.monotonic() >= heartbeat:
                        self.status["heartbeat_at"] = iso()
                        atomic(self.folder / "result.json", self.status)
                        heartbeat = time.monotonic() + 5
                    if time.monotonic() >= end:
                        raise PipelineError("stage_timeout")
                    try: chunk = messages.get(timeout=.1)
                    except queue.Empty: chunk = ""
                    output = (output + chunk)[-65536:]
                    pending_lines += chunk
                    lines = pending_lines.split("\n")
                    pending_lines = lines.pop()[-65536:]
                    for line in lines:
                        if line.startswith("step "): line = line[5:]
                        if line.startswith("{"):
                            try: event = json.loads(line)
                            except ValueError: continue
                            state = event.get("state")
                            if state == "terms": self.update(stage="terms", status="running")
                            if state == "waiting_input":
                                waiting = True
                                self.update(status="waiting_input", reason="awaiting_sms" if code_mode else "awaiting_registration_code")
                            if state in PROBE_STATES or state in {"success", "unknown", "rate_limited", "waiting_input", "failed", "needs_review"}: final = event
                    if "verification code requested" in output or "Enter six-digit code" in output:
                        waiting = True
                    if re.search(r'''tooManyCodesSent["'\s:=]+true|ec=-22981|(?:HTTP|status)["'\s:=]+423''', output):
                        raise PipelineError("code_rate_limited", "rate_limited")
                    if waiting and code_mode and not code_delivered and time.monotonic() >= next_poll:
                        self.update(status="waiting_input", reason="awaiting_sms")
                        next_poll = time.monotonic() + 4
                        try: code = self.next_code()
                        except PipelineError: code = ""
                        if code:
                            if code_mode == "stdin":
                                child.stdin.write((code + "\n").encode()); child.stdin.flush()
                            else:
                                file = self.folder / "web-code.txt"
                                with open(file, "x", encoding="utf-8") as f:
                                    os.chmod(file, 0o600); f.write(code)
                            code_delivered = True
                            self.update(status="running", reason="code_delivered")
                return child.returncode, final
            finally:
                if child.poll() is None:
                    if os.name == "nt":
                        subprocess.run(["taskkill", "/PID", str(child.pid), "/T", "/F"], stdout=subprocess.DEVNULL,
                                       stderr=subprocess.DEVNULL, creationflags=subprocess.CREATE_NO_WINDOW, timeout=20)
                    else: child.kill()
                    child.wait(timeout=20)
                thread.join(timeout=2)

    def create(self):
        country = account_country(self.cfg)
        self.number()
        sms, roxy = self.cfg["sms"], self.cfg["roxy"]
        if sms.get("auto_purchase") and not sms.get("can_get_another_sms"):
            raise PipelineError("number_cannot_receive_followup", "failed")
        remaining = timestamp(sms["expires_at"]) - time.time()
        if remaining <= 0: raise PipelineError("activation_expired", "waiting_input")
        self.update(reason="browser_create_pending")
        proxy = {} if self.cfg.get("egress_mode") == "direct" else {"proxyInfo": roxy["proxyInfo"]}
        resource = self.forge("create", projectId=roxy["projectId"], windowName="apple-" + self.cfg["job_id"],
                              os="Windows", locale=roxy["locale"], timeZone=roxy["timeZone"], **proxy)
        if not resource.get("dirId"): raise PipelineError("browser_identity_missing")
        atomic(self.folder / "browser.json", {"dirId": resource["dirId"]})
        try:
            opened = self.forge("open", dirId=resource["dirId"], args=[], headless=False)
            port = opened.get("debuggingPort") or str(opened.get("debuggerAddress", "")).rsplit(":", 1)[-1]
            if not str(port).isdigit() or not 0 < int(port) < 65536:
                raise PipelineError("browser_port_missing")
            inp = {"email": self.cfg["email"], "password": self.cfg["password"], "phone": str(sms["phone"]),
                   "country": country,
                   "phonecc": sms["phonecc"], "activation": str(sms["activation"]), "hero-key": sms["key"],
                   "sms-api": sms.get("api", "https://api.grizzlysms.com/stubs/handler_api.php"),
                   "outdir": str(self.folder), "cdp-port": str(port), "activation-expires-at": sms["expires_at"],
                   "expected-locale": roxy["locale"], "expected-timezone": roxy["timeZone"],
                   "sms-code-out": str(self.folder / "create-sms.txt"), "await-captcha": str(self.folder / "captcha.txt")}
            if self.cfg.get("code_pool_config"): inp["code-pool-config"] = self.cfg["code_pool_config"]
            elif self.cfg.get("graph_token_file"): inp["graph-token-file"] = self.cfg["graph_token_file"]
            else: inp["await-email-code"] = self.cfg["email_code_file"]
            if self.cfg.get("captcha_solver") == "yes":
                inp["captcha-cmd"] = json.dumps(["python", "tools/captcha/solver.py", "--provider", "yes", "--skip-local"])
            atomic(self.folder / "create-input.json", inp)
            code, _ = self.process(["node", "tools/capture/pipeline-browser.cjs"], 45)
            if code != 0: raise PipelineError("browser_preflight_failed", "failed")
            self.update(reason="form_running")
            self.update(registration_started=True)
            code, _ = self.process(["node", "tools/capture/create-account.cjs"], max(1, timestamp(sms["expires_at"]) - time.time()) + 15)
            self.update(create_exit_code=code, creation_rejected=code in (1, 2))
            if self.cfg.get("handoff") == "dashboard" and code not in (0, 1, 2):
                raise PipelineError("registration_requires_readback", "needs_review")
            if code not in (1, 2):
                self.update(stage="login", status="queued", account_created=code == 0,
                            reason="created" if code == 0 else "registration_needs_confirmation")
            else:
                raise PipelineError("registration_rejected", "usage" if code == 2 else "failed")
        finally:
            self.cleanup_browser()

    def create_with_replacement(self):
        for attempt in range(2):
            try:
                return self.create()
            except PipelineError as error:
                safe = error.reason == "number_cannot_receive_followup" or (error.reason == "registration_rejected" and self.status.get("creation_rejected"))
                if not self.cfg["sms"].get("auto_purchase") or not safe or self.status.get("cleanup_pending"):
                    raise
                self.settle_number()
                if self.status.get("spend", {}).get("otp_received"):
                    raise PipelineError("registration_requires_readback", "needs_review") from None
                if attempt == 1:
                    raise PipelineError("account_number_limit", "needs_review") from None
                sms = self.cfg["sms"]
                self.cfg["sms"] = {k:sms[k] for k in ("auto_purchase","phonecc","max_price","max_total","currency")}
                atomic(self.folder / "input.json", self.cfg)
                self.update(registration_started=False, creation_rejected=False, reason="replacement_number_pending")

    def begin(self, stage):
        self.check_cancel()
        attempts = self.status["attempts"]
        attempts[stage] = attempts.get(stage, 0) + 1
        if attempts[stage] > 2: raise PipelineError("stage_attempt_budget_exhausted", "waiting_input")
        self.update(stage=stage, status="running", reason="stage_started", attempts=attempts)

    def authenticate(self):
        if self.cfg.get("handoff") == "dashboard":
            return self.dashboard_handoff()
        sms = self.cfg["sms"]
        remaining = timestamp(sms["expires_at"]) - time.time()
        if remaining <= 0:
            code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-mode", "probe", "-session",
                                       str(self.folder / "icloud-web-session.json"), "-timeout", "90s"], 95)
            if code == 0 and event and event.get("state") in PROBE_STATES:
                self.update(account_created=True, probe_state=event["state"])
                raise PipelineError("account_exists_activation_expired", "waiting_input")
            raise PipelineError("expired_activation_account_unconfirmed", "needs_review")
        reserve_code(sms)
        (self.folder / "web-code.txt").unlink(missing_ok=True)
        code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-session", str(self.folder / "icloud-web-session.json"),
                                   "-code-file", str(self.folder / "web-code.txt"), "-accept-terms", "-timeout", f"{int(remaining)}s"],
                                  remaining + 5, "file")
        if code != 0 or not event or event.get("state") != "success":
            if event and event.get("reason") == "apple_sms_verify_precondition_failed":
                raise PipelineError("apple_sms_verify_precondition_failed", "needs_review")
            raise PipelineError("web_login_unconfirmed", (event or {}).get("state", "unknown"))
        self.update(stage="gsa", status="queued", account_created=True, reason="web_session_confirmed")

    def gsa(self):
        remaining = timestamp(self.cfg["sms"]["expires_at"]) - time.time()
        if remaining <= 0: raise PipelineError("activation_expired", "waiting_input")
        reserve_code(self.cfg["sms"])
        code, event = self.process([str(ROOT / "bin/login.exe"), "-dir", str(self.folder), "-twofactor", "sms", "-checkpoint"],
                               remaining, "stdin")
        if event and event.get("reason") == "gsa_account_repair_required":
            raise PipelineError("gsa_account_repair_required", "needs_review")
        if code != 0 or not (self.folder / "icloud-accepted.json").exists():
            raise PipelineError("gsa_login_unconfirmed")
        self.update(stage="device", status="queued", reason="gsa_checkpoint_saved")

    def device(self):
        code, event = self.process([str(ROOT / "bin/trusted-device.exe"), "-dir", str(self.folder), "-name", "Apple-" + self.cfg["job_id"],
                                   "-native-push", "-listen-only", "-resume-session", "-register-only"], 120)
        if code != 0 or not event or event.get("registration_accepted") is not True:
            raise PipelineError("device_registration_unconfirmed")
        self.update(stage="keepalive", status="queued", registration_accepted=True, reason="device_registered")

    def keepalive(self):
        code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-mode", "keepalive", "-session",
                                   str(self.folder / "icloud-web-session.json"), "-timeout", "90s"], 100)
        if code != 0 or not event or event.get("state") != "success":
            raise PipelineError("session_validation_failed", "needs_login")
        self.update(status="ready", reason="session_validated", next_run_at=iso(time.time() + 600))

    def run(self, resume=False):
        try:
            if self.status["status"] == "ready": return
            if (resume and self.cfg.get("handoff") == "dashboard" and self.status.get("stage") == "subscription"
                    and self.status.get("reason") == "subscription_payment_required" and self.status.get("account_created")):
                self.check_cancel()
                self.complete_family()
                self.update(status="ready", handoff_complete=True, reason="panel_handoff_complete", next_run_at=None)
                return
            # Protocol admission comes first, before the legacy recovery branch can
            # clean a browser or rewrite the stage on its way past.
            protocol_action = self.protocol_action() if self.method == "protocol" else None
            if self.method == "protocol" and self.status.get("stop_requested"):
                # A persisted stop is an instruction, not just a blocked run.
                self.protocol_cancel(adapter_registry.resolve(self.cfg.get("protocol")))
                return
            if self.method == "protocol" and (self.folder / "cancel.request").exists():
                # An explicit stop outranks the review gate: the operator already
                # decided, and the adapter is the only one who can confirm what the
                # request actually did.
                self.protocol_cancel(adapter_registry.resolve(self.cfg.get("protocol")))
                return
            if protocol_action == "review":
                # Keep the fact that made us stop: a cooldown keeps its deadline and
                # its rate_limited state instead of being rewritten as needs_review.
                hold = self.policy_decision()
                if hold.reason == "server_requires_cooldown":
                    raise PipelineError("server_requires_cooldown", "rate_limited")
                raise PipelineError(hold.reason or self.status.get("reason") or "registration_outcome_unverified",
                                    "needs_review")
            # Browser jobs claim the activation here; protocol jobs do it inside
            # protocol_step so the purchase and the claim stay together.
            if (self.method != "protocol" and self.status["stage"] == "create"
                    and self.cfg["sms"].get("activation") and not self.status.get("number_owned")):
                self.claim_number()
            stage = self.status["stage"]
            if (self.method != "protocol" and resume and stage == "create"
                    and self.status["attempts"].get("create")):
                self.cleanup_browser()
                if self.status["status"] not in {"failed", "usage"}:
                    if not self.cfg["sms"].get("activation"):
                        raise PipelineError("number_purchase_requires_readback", "needs_review")
                    # A crash/timeout after a submit is never permission to repeat registration.
                    self.update(stage="login", status="queued", reason="reconcile_interrupted_creation")
            elif resume and stage in {"login", "terms", "gsa", "device", "subscription", "family"} and self.status["status"] != "queued":
                raise PipelineError("ambiguous_stage_requires_readback", "needs_review")
            while self.status["status"] != "ready":
                stage = self.status["stage"]
                # begin() flips the status to running, so the waiting flag has to be
                # read first; otherwise a resumed job looks like a fresh create and
                # falls through to the login handoff.
                was_waiting = self.status.get("status") == "waiting_input"
                # Decide before anything rewrites the status: the decision must see
                # the persisted business state, not the stage bookkeeping.
                action = self.protocol_action() if self.method == "protocol" else None
                if action == "review":
                    raise PipelineError(self.status.get("reason") or "registration_outcome_unconfirmed", "needs_review")
                if self.method == "protocol" and (self.folder / "cancel.request").exists():
                    adapter = adapter_registry.resolve(self.cfg.get("protocol"))
                    if self.protocol_cancel(adapter):
                        return
                # Waiting polls must not spend the stage budget: begin() counts one
                # attempt per call, and a job can wait many times for one code.
                if was_waiting or (self.method == "protocol" and stage == "create" and self.status.get("challenge_kind")):
                    self.update(stage=stage, status="running", reason="polling")
                else:
                    self.begin(stage)
                if self.method == "protocol" and not self.status.get("account_created"):
                    # Until an account is proven, the adapter owns the job whatever
                    # the persisted status says: running, unknown and needs_review
                    # all mean "registration outcome not confirmed", never "go to
                    # login". Only account_created hands the job to the next stage.
                    if self.protocol_step(waiting=was_waiting, action=action):
                        return
                elif stage == "create": self.create_with_replacement()
                elif self.cfg.get("handoff") == "dashboard": self.dashboard_handoff()
                elif stage in {"login", "terms"}: self.authenticate()
                elif stage == "gsa": self.gsa()
                elif stage == "device": self.device()
                elif stage == "keepalive": self.keepalive()
                else: raise PipelineError("invalid_stage", "usage")
        except PipelineError as error:
            previous = self.status.get("cooldown_until")
            cooldown = previous if error.state == "rate_limited" and previous else (
                iso(time.time() + 3600) if error.state == "rate_limited" else previous)
            prior = self.status.get("reason")
            # The hold code explains this refusal; the job keeps the reason that
            # made it unresolved.
            extra = {}
            if (error.state in {"needs_review", "rate_limited"} and prior and prior != error.reason
                    and error.reason not in {"gsa_account_repair_required", "apple_sms_verify_precondition_failed"}):
                extra["hold_reason"] = error.reason
                self.update(status=error.state, reason=prior, cooldown_until=cooldown, **extra)
            else:
                self.update(status=error.state, reason=error.reason, cooldown_until=cooldown)
            if False: pass
            if (self.method != "protocol" and self.cfg.get("handoff") != "dashboard" and self.status["stage"] == "create"
                    and error.state == "unknown" and self.cfg["sms"].get("activation")):
                self.update(stage="login", status="queued", reason="registration_needs_confirmation")
                try:
                    self.begin("login")
                    self.authenticate()
                except PipelineError as e:
                    self.update(status=e.state, reason=e.reason)
                if self.status["status"] == "queued": self.run()
            if error.state not in {"needs_review", "waiting_input", "unknown"}:
                self.settle_number()
            else:
                self.update(refund_deferred=bool(self.status.get("number_owned") and not self.status.get("account_created")))
        except (Exception, KeyboardInterrupt):
            # Retain the stage so a restart cannot mistake an interrupted write for a fresh job.
            self.update(status="unknown", reason="interrupted_or_local_error",
                        refund_deferred=bool(self.status.get("number_owned") and not self.status.get("account_created")))
        finally:
            # A waiting job keeps its number: releasing it here would invalidate the
            # code the operator is about to receive. Unknown outcomes keep it too.
            withholding = self.status.get("status") in {"waiting_input", "unknown", "needs_review"}
            if self.cfg["sms"].get("auto_purchase") and not withholding:
                try: self.settle_number()
                except Exception: self.update(refund_pending=True)
            self.update(active=False)

    def supply(self):
        return Supply(ROOT / "state" / "sms-supply", self.cfg["email"], atomic, lock, sms_call)

    def panel_call(self, action, **body):
        bridge = self.cfg["panel_bridge"]
        url = bridge["url"]
        parsed = urllib.parse.urlparse(url)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1": raise PipelineError("invalid_panel_bridge", "usage")
        self.update(heartbeat_at=iso(), heartbeat_deadline=iso(time.time()+250))
        reply = json.loads(request(url, {"action":action, **body},
                                  {"Content-Type":"application/json", "X-App-Token":bridge["token"]}, timeout=240))
        if reply.get("state") == "rate_limited": raise PipelineError("code_rate_limited", "rate_limited")
        if reply.get("state") == "unknown":
            reason=reply.get("reason")
            known={"family_binding_invalid","family_role_mismatch","family_action_invalid","family_readback_required","family_full","family_organizer_not_ready","subscription_disabled","subscription_auth_required","subscription_readback_required","subscription_offer_unsupported","subscription_renewal_unconfirmed"}
            raise PipelineError(reason if reason in known else "panel_handoff_requires_readback", "needs_review")
        return reply

    def panel_once(self, action, **body):
        self.check_cancel()
        operations = self.status.setdefault("operations", {})
        key = action
        digest=hashlib.sha256(json.dumps(body,sort_keys=True).encode()).hexdigest()
        if key in operations:
            if operations[key].get("state") == "succeeded" and operations[key].get("intent_hash")==digest: return operations[key]["result"]
            raise PipelineError("panel_handoff_requires_readback", "needs_review")
        operations[key] = {"state":"intent", "started_at":iso(), "intent_hash":digest}
        self.update(operations=operations)
        reply = self.panel_call(action, **body)
        if reply.get("ok") is not True: raise PipelineError("panel_handoff_requires_readback", "needs_review")
        operations[key].update(state="succeeded", result=reply)
        self.update(operations=operations)
        return reply

    def prepare_followup_sms(self):
        if not (self.folder / "create-sms.txt").exists(): return
        if self.status.get("followup_sms") == "ready": return
        if self.status.get("followup_sms"):
            raise PipelineError("followup_sms_requires_readback", "needs_review")
        self.update(followup_sms="intent")
        reply=sms_call(self.cfg["sms"], action="setStatus", status="3", id=self.cfg["sms"]["activation"])
        if reply != "ACCESS_RETRY_GET":
            raise PipelineError("followup_sms_requires_readback", "needs_review")
        self.update(followup_sms="ready")

    def dashboard_handoff(self):
        # The existing dashboard owns Mac identity/login/NAC; do not run the parallel CLI chain.
        try:
            self.panel_once("attach")
            observed = self.panel_call("observe")
            if observed.get("activated") and observed.get("online"):
                self.complete_family()
                self.update(status="ready", account_created=True, handoff_complete=True, reason="panel_handoff_complete", next_run_at=None)
                return
            sms = self.cfg["sms"]
            if timestamp(sms["expires_at"]) <= time.time(): raise PipelineError("activation_expired", "needs_review")
            self.prepare_followup_sms()
            reserve_code(sms)
            self.panel_once("start")
            self.update(stage="gsa", reason="panel_login_started")
            deadline=timestamp(sms["expires_at"])
            delivered=False
            while time.time()<deadline:
                self.check_cancel()
                observed=self.panel_call("observe")
                if observed.get("activated"):
                    self.update(account_created=True, stage="keepalive")
                    break
                if observed.get("gsa_verified"):
                    self.update(account_created=True, stage="terms", reason="panel_activation_started")
                    self.panel_once("stop")
                    plan=self.panel_call("plan")
                    if not plan.get("ok") or not plan.get("terms"): raise PipelineError("panel_activation_plan_unconfirmed", "needs_review")
                    self.panel_once("activate", terms=plan["terms"])
                    break
                if observed.get("waiting_sms") and not delivered:
                    self.update(status="waiting_input", reason="awaiting_sms")
                    code=self.next_code()
                    if code:
                        self.panel_once("sms", code=code)
                        delivered=True
                        self.update(status="running")
                elif observed.get("failed"):
                    raise PipelineError("panel_login_unconfirmed", "needs_review")
                time.sleep(4)
            else: raise PipelineError("activation_expired", "needs_review")
            self.check_cancel()
            self.panel_once("resume")
            self.update(stage="keepalive", reason="panel_keepalive_started")
            for _ in range(45):
                self.check_cancel()
                state=self.panel_call("observe")
                if state.get("activated") and state.get("online"):
                    self.complete_family()
                    self.update(status="ready", account_created=True, handoff_complete=True,
                                reason="panel_handoff_complete", next_run_at=None)
                    return
                if state.get("failed"): break
                time.sleep(2)
            raise PipelineError("panel_keepalive_unconfirmed", "needs_review")
        except PipelineError as error:
            if error.reason != "subscription_payment_required":
                try: self.panel_call("stop")
                except Exception: pass
            raise
        except (Exception, KeyboardInterrupt):
            # Stop only this task's owned worker, including a GUI worker waiting for an expired OTP.
            try: self.panel_call("stop")
            except Exception: pass
            raise

    def complete_family(self):
        family=self.cfg.get("family", {})
        role=family.get("role")
        if role in (None,"","none"): return
        if role not in ("organizer","member"): raise PipelineError("family_role_mismatch","usage")
        if role=="organizer":
            if family.get("acceptSubscription") is not True: raise PipelineError("subscription_disabled","usage")
            self.update(stage="subscription",reason="subscription_planning")
            plan=self.panel_call("subscription-plan")
            if plan.get("ok") is not True: raise PipelineError("subscription_offer_unsupported","needs_review")
            if not plan.get("existing"):
                action=plan.get("purchase_action")
                if action == "storage-skill-purchase-cn":
                    # Keep the account online for attended payment. Resume only
                    # reads the current paid quota; this path never buys blindly.
                    raise PipelineError("subscription_payment_required", "waiting_input")
                if action not in {"storage-skill-purchase-us"}: raise PipelineError("subscription_offer_unsupported","needs_review")
                if action=="storage-skill-purchase-us": self.panel_once("subscription-auth",plan=action)
                self.panel_once("subscription-buy",plan=action)
            if self.panel_call("subscription-verify").get("ok") is not True:
                raise PipelineError("subscription_readback_required","needs_review")
            if family.get("region") == "CN":
                self.panel_once("subscription-cancel")
            self.update(subscription_verified=True,stage="family",reason="family_creating")
            self.panel_once("family-create")
            self.panel_once("family-share")
        else:
            if not family.get("organizer"): raise PipelineError("family_binding_invalid","usage")
            self.update(stage="family",reason="family_joining")
            self.panel_once("family-join")
            self.panel_once("family-join-storage")
        if self.panel_call("family-verify").get("ok") is not True:
            raise PipelineError("family_readback_required","needs_review")
        self.update(family_complete=True,family_role=role,reason="family_completed")

    CODE_PATTERN = re.compile(r"^\d{6}$")

    def challenge_kind_for(self, stage):
        """Which code the current stage is waiting for, if any."""
        if stage == "email-sent":
            return "email"
        if stage == "phone-sent":
            return "phone"
        return ""

    def collect_inputs(self):
        """Read only the code the current stage is waiting for.

        Reading both files would consume the SMS code during the email challenge,
        and re-reading the email file during the phone challenge would look like a
        replay. One challenge, one input.
        """
        kind = self.challenge_kind_for(self.status.get("stage", ""))
        if not kind:
            return {}
        try:
            code, _ = self._parse_input(kind)
            return {kind: code}
        except PipelineError as error:
            if error.reason in {"input_not_ready", "input_path_missing", "input_empty",
                                "input_predates_challenge", "input_malformed"}:
                return {}  # keep waiting; nothing was consumed
            raise

    def policy_view(self):
        """The facts the decision table needs, and nothing else."""
        # Older rows carry only a timestamp; convert them once, explicitly.
        self.status = self.upgrade_status(self.status) or self.status
        # Only a challenge that was actually opened counts. A bare timestamp is not
        # evidence, so it is not promoted into an open challenge here.
        challenge = self.status.get("challenge") or {}
        return {
            "run": {"state": self.status.get("status"), "cooldown_until": self.status.get("cooldown_until")},
            "status": self.status.get("status"),
            "stage": self.status.get("stage"),
            "job_id": self.cfg.get("job_id") or self.status.get("job_id"),
            "reason": self.status.get("reason"),
            "registration_started": self.status.get("registration_started"),
            "create_attempted": self.status.get("create_attempted"),
            "attempts": self.status.get("attempts") or {},
            "create_confirmed": self.status.get("create_confirmed"),
            "operations": self.status.get("operations"),
            "challenge": challenge,
            "account_created": self.status.get("account_created"),
        }

    def policy_decision(self):
        facts = {
            "needs_purchase": bool(self.cfg["sms"].get("auto_purchase") and not self.status.get("number_owned")),
            "stop_requested": bool(self.status.get("stop_requested") or (self.folder / "cancel.request").exists()),
        }
        return job_policy.decide(self.policy_view(), "run", time.time(), facts)

    def protocol_action(self):
        """Delegates to the shared decision table (job_policy.decide).

        create — nothing was submitted
        resume — a submission is confirmed and a live challenge is bound
        review — anything unresolved, expired, cooling down or stopped
        done   — registration is proven and the job belongs to activation
        """
        decision = self.policy_decision()
        return {"create": "create", "acquire_then_create": "create",
                "resume": "resume", "handoff": "done"}.get(decision.action, "review")

    def protocol_action_legacy(self):
        """What this job is allowed to do, decided from evidence only.

        create  — nothing has been submitted yet
        resume  — a submission exists and the outcome is still open
        review  — the outcome is unknown or already flagged; a human decides
        """
        if self.status.get("account_created"):
            return "done"  # registration is proven; the job belongs to the next stage
        status = self.status.get("status")
        attempts = self.status.get("attempts") or {}
        intent = bool(self.status.get("registration_started") or self.status.get("create_attempted")
                      or attempts.get("create") or status == "waiting_input")
        if status in {"unknown", "needs_review"}:
            return "review"
        pending = [key for key, value in (self.status.get("operations") or {}).items()
                   if value not in {"succeeded", ""}]
        if pending:
            return "review"
        if not intent:
            return "create"
        # Intent proves a request was *started*, not that it landed. Resuming needs
        # completion evidence plus a live challenge; without both, a human checks.
        # A persisted waiting state was written by the adapter, so it is itself
        # evidence that a submission happened; an interrupted "running" needs the
        # explicit create_confirmed marker.
        confirmed = bool(self.status.get("create_confirmed") or status == "waiting_input")
        challenge = self.status.get("challenge_at") and self.status.get("challenge_stage") == self.status.get("stage")
        if confirmed and challenge:
            return "resume"
        return "review"

    def protocol_cancel(self, adapter):
        """Cancel is a request, not a refund.

        The adapter may only report that execution stopped. Unless the outcome is
        confirmed, the number stays with the job and a human decides.
        """
        result = adapter.cancel(self) or {}
        confirmed = result.get("state") == "cancelled" and result.get("outcome_confirmed") is True
        if confirmed:
            self.update(status="cancelled", stage=self.status["stage"], reason="cancelled_confirmed",
                        refund_deferred=False)
        else:
            self.update(status="needs_review", stage=self.status["stage"], reason="cancel_outcome_unconfirmed",
                        refund_deferred=bool(self.status.get("number_owned") and not self.status.get("account_created")))
        return True

    def open_challenge(self, kind, source_id="", sent_at=None):
        """Start a challenge with an identity of its own.

        Re-sending a code for the same stage must still be a new challenge, so the
        identity cannot be the stage name plus a timestamp.
        """
        if sent_at is not None:
            try:
                age=time.time()-timestamp(sent_at)
                if not 0<=age<=86400: raise ValueError()
            except (ValueError,TypeError,AttributeError):
                raise PipelineError("challenge_send_time_invalid","needs_review") from None
        challenge = {
            "id": hashlib.sha256(f"{self.cfg['job_id']}:{kind}:{time.time_ns()}".encode()).hexdigest()[:32],
            "kind": kind,
            "sent_at": sent_at if sent_at is not None else iso(),
            "status": "open",
            "source_id": source_id,
        }
        self.update(challenge=challenge, challenge_at=challenge["sent_at"], challenge_stage=self.status.get("stage"))
        return challenge

    def active_challenge(self, kind):
        challenge = self.status.get("challenge") or {}
        if challenge.get("kind") != kind or challenge.get("status") not in {"open", "sent"}:
            return None
        return challenge

    def input_state(self, input_id):
        return ((self.status.get("inputs") or {}).get(input_id) or {}).get("state")

    def reserve_input(self, input_id, kind):
        """available -> reserved -> dispatch_intent -> accepted.

        reserved without a dispatch intent is reclaimable after a crash; anything
        past dispatch_intent must be verified instead of being re-sent.
        """
        inputs = dict(self.status.get("inputs") or {})
        record = inputs.get(input_id) or {"kind": kind}
        if record.get("state") in {"dispatch_intent", "accepted"}:
            return record
        record.update(state="reserved", run_id=self.status.get("active_run_id"), reserved_at=iso())
        inputs[input_id] = record
        self.update(inputs=inputs)
        return record

    def dispatch_input(self, input_id):
        inputs = dict(self.status.get("inputs") or {})
        record = inputs.get(input_id) or {}
        record.update(state="dispatch_intent", dispatch_at=iso())
        inputs[input_id] = record
        self.update(inputs=inputs)

    def accept_input(self, input_id, accepted):
        inputs = dict(self.status.get("inputs") or {})
        record = inputs.get(input_id) or {}
        record.update(state="accepted" if accepted else "rejected", settled_at=iso())
        inputs[input_id] = record
        self.update(inputs=inputs)

    def peek_input(self, kind):
        """Validate a code without consuming it.

        Returns (code, input_id). The caller decides whether it is dispatched, so a
        crash before dispatch leaves nothing consumed.
        """
        return self._parse_input(kind)

    def _parse_input(self, kind):
        """Shared validation: path, freshness, shape, binding. No side effects."""
        key = {"email": "emailCode", "phone": "phoneCode"}.get(kind)
        if key is None:
            raise PipelineError("unknown_input_kind", "usage")
        raw = (self.cfg.get("protocol") or {}).get(key) or self.cfg.get(key)
        if not raw:
            raise PipelineError("input_path_missing", "usage")
        path = Path(raw).resolve()
        if self.folder.resolve() not in path.parents:
            raise PipelineError("input_outside_task", "usage")
        if not path.is_file():
            raise PipelineError("input_not_ready", "waiting_input")
        challenge = self.active_challenge(kind) or {}
        sent_at = challenge.get("sent_at") or self.status.get("challenge_at")
        if sent_at and path.stat().st_mtime < timestamp(sent_at):
            raise PipelineError("input_predates_challenge", "waiting_input")
        code = path.read_text(encoding="utf-8").strip()
        if not code:
            raise PipelineError("input_empty", "waiting_input")
        if not self.CODE_PATTERN.match(code):
            # Shape is checked before any reservation: "not-a-code" must stay usable.
            raise PipelineError("input_malformed", "waiting_input")
        digest = hashlib.sha256(f"{kind}:{code}:{challenge.get('id', '')}".encode()).hexdigest()
        state = self.input_state(digest)
        if state in {"dispatch_intent", "accepted"}:
            raise PipelineError("input_already_consumed", "needs_review")
        if state in {"rejected", "inconclusive"}:
            raise PipelineError("input_dispatch_unconfirmed", "needs_review")
        return code, digest

    def read_input(self, kind):
        """Validate and reserve one code. A reservation is reclaimable; a
        dispatch intent is not."""
        code, digest = self._parse_input(kind)
        self.reserve_input(digest, kind)
        self.update(consumed_inputs={**(self.status.get("consumed_inputs") or {}), digest: iso()})
        return code

    def reclaim_reservations(self):
        """Give back reservations that provably never reached dispatch."""
        inputs = dict(self.status.get("inputs") or {})
        changed = False
        for key, record in list(inputs.items()):
            if record.get("state") == "reserved":
                inputs.pop(key, None)
                changed = True
        if changed:
            self.update(inputs=inputs)

    def check_cancel(self):
        if (self.folder / "cancel.request").exists():
            raise PipelineError("cancelled_requires_readback" if self.status.get("registration_started") else "cancelled_before_registration", "cancelled")

    def settle_number(self, force=False):
        sms = self.cfg["sms"]
        # At expiry the only honest next step is a read-only probe; the policy is
        # applied afterwards, with the probe result in hand.
        needs_probe = (not sms.get("auto_purchase") and self.status.get("number_owned")
                       and not self.status.get("account_created") and not self.status.get("expiry_checked")
                       and timestamp(sms.get("expires_at") or 0) <= time.time())
        # One policy decides whether this order may be released. force is kept only
        # for an explicit reviewer resolution and is recorded, never inferred.
        verdict = job_policy.settlement_decision(
            {"registration": {"outcome": "registered" if self.status.get("account_created") else "unknown"},
             "registration_started": bool(self.status.get("registration_started")),
             "attempts": self.status.get("attempts") or {},
             "run": {"state": self.status.get("status")}},
            {"id": sms.get("activation"), "state": "refund_confirmed" if self.status.get("refund_confirmed") else "owned",
             "cost": (self.status.get("spend") or {}).get("cost")},
            {"sms_state": self.status.get("sms_state"),
             "otp_received": bool(self.status.get("code_received")),
             "business_release_allowed": bool(
                 self.status.get("registration_not_applied")
                 or not (self.status.get("registration_started") or self.status.get("create_attempted")
                         or (self.status.get("attempts") or {}).get("create"))),
             "order_matches": self.status.get("order_matches", True),
             "evidence": self.status.get("failure")})
        if verdict.action not in {"refund"} and not needs_probe:
            if self.status.get("number_owned") and not self.status.get("account_created"):
                self.update(refund_deferred=True, settlement_reason=verdict.reason)
            return
        if force:
            self.update(settlement_forced=True, settlement_reason="reviewer_resolution")
        # One guard for every caller (run's finally, the exception path and the
        # maintenance sweep): an unresolved job keeps its number. Only an explicit
        # confirmed cancellation may pass force=True.
        if not force and self.status.get("status") in {"needs_review", "waiting_input", "unknown"}:
            if self.status.get("number_owned") and not self.status.get("account_created"):
                self.update(refund_deferred=True)
            return
        if sms.get("auto_purchase"):
            if (self.folder / "create-sms.txt").exists() and sms.get("supply_operation"):
                self.supply().otp_received(sms["supply_operation"])
            summary=self.supply().settle(self.cfg["job_id"])
            self.update(spend=summary, refund_pending=summary["refund_pending"],
                        refund_confirmed=summary["refunded"]!="0", refund_after=iso(time.time()+130))
            return
        if not sms.get("activation") or not self.status.get("number_owned") or self.status.get("account_created") or self.status.get("refund_confirmed"): return
        # Only proven creation failure, or lease expiry, releases the rented number.
        expired = timestamp(sms["expires_at"]) <= time.time()
        if expired and self.status.get("registration_started") and not self.status.get("expiry_checked"):
            # Password readback sends no SMS and is never a replay of registration.
            try:
                code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-mode", "probe", "-session",
                                           str(self.folder / "icloud-web-session.json"), "-timeout", "90s"], 95, ignore_cancel=True)
                exists = code == 0 and event and event.get("state") in PROBE_STATES
            except PipelineError:
                exists = False
            self.update(expiry_checked=True, account_created=bool(exists), refund_deferred=False,
                        reason="account_exists_activation_expired" if exists else "expired_activation_account_unconfirmed")
            if exists: return
        if self.status["stage"] != "create" and not expired:
            self.update(refund_deferred=True)
            return
        # A cancelled job only means "stop". It is not evidence that no account was
        # created, so it is never on its own a refund basis: the registration result
        # and the SMS receive state have to be confirmed first.
        safe_cancel = (self.status["status"] == "cancelled"
                       and not self.status.get("registration_started")
                       and not self.status.get("code_received")
                       and not self.status.get("activation_used"))
        if self.status["status"] not in {"failed", "usage"} and not expired and not safe_cancel:
            self.update(refund_deferred=True)
            return
        if self.status["status"] == "cancelled" and not safe_cancel:
            self.update(refund_deferred=True, reason="cancelled_requires_outcome_check")
            return
        # Re-evaluate with the probe result: the probe gathers evidence, it does not
        # grant permission.
        after_probe = job_policy.settlement_decision(
            {"registration": {"outcome": "registered" if self.status.get("account_created") else "unknown"},
             "registration_started": bool(self.status.get("registration_started")),
             "attempts": self.status.get("attempts") or {}},
            {"id": sms.get("activation"),
             "state": "refund_confirmed" if self.status.get("refund_confirmed") else "owned"},
            {"sms_state": self.status.get("sms_state"),
             "otp_received": bool(self.status.get("code_received")),
             "business_release_allowed": bool(
                 self.status.get("registration_not_applied")
                 or not (self.status.get("registration_started") or self.status.get("create_attempted")
                         or (self.status.get("attempts") or {}).get("create"))),
             "order_matches": self.status.get("order_matches", True)})
        if after_probe.action != "refund":
            self.update(refund_deferred=True, settlement_reason=after_probe.reason)
            return
        if self.status.get("refund_attempts", 0) >= 5: return
        if self.status.get("refund_after") and timestamp(self.status["refund_after"]) > time.time(): return
        self.update(refund_attempts=self.status.get("refund_attempts", 0) + 1)
        try:
            reply = sms_call(sms, action="setStatus", status="8", id=sms["activation"])
            refunded = reply in {"ACCESS_CANCEL", "ACCESS_CANCEL_ALREADY"}
        except PipelineError:
            refunded = False
        self.update(refund_deferred=False, refund_pending=not refunded, refund_confirmed=refunded,
                    refund_after=iso(time.time() + 130) if not refunded else None)


def execute(action, job_id=None, input_file=None, allow_purchase=False):
    cfg = read(input_file) if input_file else None
    folder = job_path(cfg["job_id"] if cfg else job_id)
    if action == "status": return read(folder / "result.json")
    with lock(folder / ".lock"):
        if (folder / "input.json").exists():
            existing = read(folder / "input.json")
            digest = lambda value: hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()
            if cfg and (folder / "request-hash.json").exists() and digest(cfg) != read(folder / "request-hash.json"):
                raise PipelineError("job_id_conflict", "usage")
            cfg = existing
            status = read(folder / "result.json")
            if action == "run":
                # A resume has to pass the same gate as a new job: without an
                # adapter there is nothing to run, and buying a number first would
                # spend money on a job that cannot proceed.
                method = cfg.get("registration_method") or cfg.get("registrationMethod") or "browser"
                if method == "protocol":
                    try:
                        adapter_registry.resolve(cfg.get("protocol"))
                    except adapter_registry.AdapterMissing as missing:
                        raise PipelineError(missing.reason, "needs_review")
                return status  # identical invocation is a read, not a retry
        else:
            if action != "run": raise PipelineError("job_not_found", "usage")
            validate(cfg)
            method = cfg.get("registration_method") or cfg.get("registrationMethod") or "browser"
            if method == "protocol":
                # Fail before the account reservation, the number purchase and any
                # browser start: without an adapter there is nothing to run.
                try:
                    adapter_registry.resolve(cfg.get("protocol"))
                except adapter_registry.AdapterMissing as missing:
                    raise PipelineError(missing.reason, "needs_review")
            for binary in ("login.exe", "appleweb.exe", "trusted-device.exe"):
                if not (ROOT / "bin" / binary).exists(): raise PipelineError("build_binaries_first", "usage")
            account_key = hashlib.sha256(cfg["email"].lower().encode()).hexdigest()
            reservation = STATE / "accounts" / (account_key + ".json")
            with lock(reservation.with_suffix(".lock")):
                if reservation.exists() and read(reservation) != cfg["job_id"]:
                    previous_id = read(reservation)
                    previous = read(job_path(previous_id) / "result.json")
                    safe_retry = previous.get("creation_rejected") or (not previous.get("registration_started") and previous.get("status") in {"failed", "usage", "cancelled"})
                    if cfg.get("retry_of") != previous_id or previous.get("account_created") or previous.get("active") or not safe_retry:
                        raise PipelineError("account_already_has_a_job", "usage")
                atomic(reservation, cfg["job_id"])
            atomic(folder / "request-hash.json", hashlib.sha256(json.dumps(cfg, sort_keys=True).encode()).hexdigest())
            atomic(folder / "result.json", Job(folder, cfg).status)
            atomic(folder / "input.json", cfg)
            status = None
        job = Job(folder, cfg, status, allow_purchase=allow_purchase)
        if action == "cleanup":
            job.cleanup_browser()
        elif action == "refund":
            job.settle_number()
        elif action == "keepalive":
            if not job.status.get("registration_accepted"): raise PipelineError("device_stage_incomplete", "usage")
            try: job.keepalive()
            except PipelineError as e: job.update(status=e.state, reason=e.reason)
        else:
            job.update(active=True)
            job.run(resume=action == "resume")
        return job.status


def maintain_once():
    # Read-only recovery of interrupted writes: never restart registration/login here.
    for receipt in STATE.glob("*/result.json"):
        try:
            with lock(receipt.parent / ".lock"):
                job = Job(receipt.parent, read(receipt.parent / "input.json"), read(receipt))
                if job.status.get("active"):
                    job.update(active=False, status="needs_review", reason="interrupted_requires_readback",
                               refund_deferred=bool(job.status.get("number_owned") and not job.status.get("account_created")))
                if (receipt.parent / "cancel.request").exists() and job.status.get("status") != "ready":
                    job.update(status="cancelled", reason="cancelled_requires_readback" if job.status.get("registration_started") else "cancelled_before_registration")
                if job.status.get("cleanup_pending") or (receipt.parent / "browser.json").exists(): job.cleanup_browser()
                # The sweep may retry a *pending* refund, but never an unresolved one:
                # settle_number itself refuses those, and the explicit call stays
                # reserved for a confirmed cancellation.
                if job.status.get("refund_pending"):
                    job.settle_number()
                if job.cfg.get("handoff") != "dashboard" and job.status.get("status") == "ready" and timestamp(job.status["next_run_at"]) <= time.time():
                    try: job.keepalive()
                    except PipelineError as e: job.update(status=e.state, reason=e.reason)
        except (PipelineError, OSError, ValueError, KeyError):
            continue


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["run", "resume", "status", "keepalive", "refund", "cleanup", "maintain", "maintain-once"])
    parser.add_argument("--input", help="private JSON manifest (run only)")
    parser.add_argument("--job")
    parser.add_argument("--allow-purchase", action="store_true", help="explicitly permit a bounded paid SMS purchase")
    args = parser.parse_args()
    try:
        if args.action == "maintain-once":
            maintain_once()
            return 0
        if args.action == "maintain":
            # Explicit foreground maintenance service; no automatic registration or login retries.
            while True:
                maintain_once()
                time.sleep(15)
        if args.action == "run" and not args.input: raise PipelineError("input_required", "usage")
        result = execute(args.action, args.job, args.input, allow_purchase=args.allow_purchase)
    except PipelineError as e: result = {"status": e.state, "reason": e.reason}
    except Exception: result = {"status": "unknown", "reason": "local_state_unavailable"}
    print(json.dumps(result, ensure_ascii=False))
    return 0 if result.get("status") == "ready" else 3


if __name__ == "__main__":
    raise SystemExit(main())
