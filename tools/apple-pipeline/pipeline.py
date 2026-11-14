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

ROOT = Path(__file__).resolve().parents[2]
STATE = Path(os.environ.get("APPLESERVICES_PIPELINE_STATE", ROOT / "state" / "pipeline")).resolve()
STAGES = ("create", "login", "terms", "gsa", "device", "keepalive")
COUNTRIES = {"GB": "GBR", "US": "USA", "CA": "CAN", "AU": "AUS", "CN": "CHN", "DE": "DEU",
             "FR": "FRA", "SG": "SGP", "HK": "HKG", "JP": "JPN"}


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


def validate(cfg):
    job_path(cfg.get("job_id"))
    if not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", cfg.get("email", "")):
        raise PipelineError("email_required", "usage")
    if cfg["email"].rsplit("@", 1)[1].lower() in {"outlook.com", "icloud.com"}:
        raise PipelineError("unsupported_registration_mailbox", "usage")
    pw = cfg.get("password", "")
    if len(pw) < 8 or not all(re.search(p, pw) for p in [r"[A-Z]", r"[a-z]", r"\d", r"[^A-Za-z0-9]"]):
        raise PipelineError("password_requires_four_character_classes", "usage")
    if bool(cfg.get("graph_token_file")) == bool(cfg.get("email_code_file")):
        raise PipelineError("choose_one_email_code_source", "usage")
    if not Path(cfg.get("graph_token_file") or cfg.get("email_code_file")).is_absolute():
        raise PipelineError("mailbox_code_source_requires_absolute_path", "usage")
    if cfg.get("graph_token_file") and not Path(cfg["graph_token_file"]).is_file():
        raise PipelineError("mailbox_token_file_missing", "usage")
    sms = cfg.get("sms", {})
    if not sms.get("key") or not re.fullmatch(r"[A-Z]{2}", sms.get("phonecc", "")):
        raise PipelineError("sms_credentials_and_country_required", "usage")
    if sms["phonecc"] not in COUNTRIES:
        raise PipelineError("unsupported_form_country", "usage")
    if sms.get("activation"):
        if not str(sms.get("phone", "")).isdigit() or not sms.get("expires_at"):
            raise PipelineError("activation_phone_and_expiry_required", "usage")
        if timestamp(sms["expires_at"]) <= time.time():
            raise PipelineError("activation_expired", "usage")
    elif not all(sms.get(k) for k in ("country", "dial_code", "max_price")):
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


def request(url, body=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=25) as response:
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
    def __init__(self, folder, cfg, status=None):
        self.folder, self.cfg = Path(folder), cfg
        self.status = status or {"job_id": cfg["job_id"], "stage": "create", "status": "queued",
                                 "attempts": {}, "account_created": False, "trust_verified": False}

    def update(self, **values):
        self.status.update(values, updated_at=iso())
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
        # A timeout here must never buy a second number on resume.
        self.update(status="running", reason="number_purchase_pending")
        answer = sms_call(sms, action="getNumber", service="wx", country=sms["country"], maxPrice=sms["max_price"])
        if answer in {"NO_NUMBERS", "NO_BALANCE", "BAD_KEY", "BAD_SERVICE", "SERVICE_UNAVAILABLE_REGION"}:
            raise PipelineError("number_unavailable", "failed")
        match = re.fullmatch(r"ACCESS_NUMBER:(\d+):(\d+)", answer)
        if not match:
            raise PipelineError("number_purchase_unconfirmed")
        phone = match[2]
        prefix = str(sms["dial_code"])
        sms.update(activation=match[1], phone=phone.removeprefix(prefix), expires_at=iso(time.time() + 20 * 60))
        atomic(self.folder / "input.json", self.cfg)
        self.claim_number()

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
                            if state in {"success", "account_exists", "unknown", "rate_limited", "waiting_input", "failed"}: final = event
                    if "verification code requested" in output or "Enter six-digit code" in output:
                        waiting = True
                    if "tooManyCodesSent" in output or "ec=-22981" in output or re.search(r"(?:HTTP|status)[:= ]+423", output):
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
        self.number()
        sms, roxy = self.cfg["sms"], self.cfg["roxy"]
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
                   "country": COUNTRIES[sms["phonecc"]],
                   "phonecc": sms["phonecc"], "activation": str(sms["activation"]), "hero-key": sms["key"],
                   "sms-api": sms.get("api", "https://api.grizzlysms.com/stubs/handler_api.php"),
                   "outdir": str(self.folder), "cdp-port": str(port), "activation-expires-at": sms["expires_at"],
                   "expected-locale": roxy["locale"], "expected-timezone": roxy["timeZone"],
                   "sms-code-out": str(self.folder / "create-sms.txt"), "await-captcha": str(self.folder / "captcha.txt")}
            if self.cfg.get("graph_token_file"): inp["graph-token-file"] = self.cfg["graph_token_file"]
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
            if code not in (1, 2):
                self.update(stage="login", status="queued", account_created=code == 0,
                            reason="created" if code == 0 else "registration_needs_confirmation")
            else:
                raise PipelineError("registration_rejected", "usage" if code == 2 else "failed")
        finally:
            self.cleanup_browser()

    def begin(self, stage):
        self.check_cancel()
        attempts = self.status["attempts"]
        attempts[stage] = attempts.get(stage, 0) + 1
        if attempts[stage] > 2: raise PipelineError("stage_attempt_budget_exhausted", "waiting_input")
        self.update(stage=stage, status="running", reason="stage_started", attempts=attempts)

    def authenticate(self):
        sms = self.cfg["sms"]
        remaining = timestamp(sms["expires_at"]) - time.time()
        if remaining <= 0:
            code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-mode", "probe", "-session",
                                       str(self.folder / "icloud-web-session.json"), "-timeout", "90s"], 95)
            if code == 0 and event and event.get("state") == "account_exists":
                self.update(account_created=True)
                raise PipelineError("account_exists_activation_expired", "waiting_input")
            raise PipelineError("expired_activation_account_unconfirmed", "needs_review")
        reserve_code(sms)
        (self.folder / "web-code.txt").unlink(missing_ok=True)
        code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-session", str(self.folder / "icloud-web-session.json"),
                                   "-code-file", str(self.folder / "web-code.txt"), "-accept-terms", "-timeout", f"{int(remaining)}s"],
                                  remaining + 5, "file")
        if code != 0 or not event or event.get("state") != "success":
            raise PipelineError("web_login_unconfirmed", (event or {}).get("state", "unknown"))
        self.update(stage="gsa", status="queued", account_created=True, reason="web_session_confirmed")

    def gsa(self):
        remaining = timestamp(self.cfg["sms"]["expires_at"]) - time.time()
        if remaining <= 0: raise PipelineError("activation_expired", "waiting_input")
        reserve_code(self.cfg["sms"])
        code, _ = self.process([str(ROOT / "bin/login.exe"), "-dir", str(self.folder), "-twofactor", "sms", "-checkpoint"],
                               remaining, "stdin")
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
            if self.status["stage"] == "create" and self.cfg["sms"].get("activation") and not self.status.get("number_owned"):
                self.claim_number()
            stage = self.status["stage"]
            if resume and stage == "create" and self.status["attempts"].get("create"):
                self.cleanup_browser()
                if self.status["status"] not in {"failed", "usage"}:
                    if not self.cfg["sms"].get("activation"):
                        raise PipelineError("number_purchase_requires_readback", "needs_review")
                    # A crash/timeout after a submit is never permission to repeat registration.
                    self.update(stage="login", status="queued", reason="reconcile_interrupted_creation")
            elif resume and stage in {"login", "terms", "gsa", "device"} and self.status["status"] != "queued":
                raise PipelineError("ambiguous_stage_requires_readback", "needs_review")
            while self.status["status"] != "ready":
                stage = self.status["stage"]
                self.begin(stage)
                if stage == "create": self.create()
                elif stage in {"login", "terms"}: self.authenticate()
                elif stage == "gsa": self.gsa()
                elif stage == "device": self.device()
                elif stage == "keepalive": self.keepalive()
                else: raise PipelineError("invalid_stage", "usage")
        except PipelineError as error:
            self.update(status=error.state, reason=error.reason,
                        cooldown_until=iso(time.time() + 3600) if error.state == "rate_limited" else None)
            if self.status["stage"] == "create" and error.state == "unknown" and self.cfg["sms"].get("activation"):
                self.update(stage="login", status="queued", reason="registration_needs_confirmation")
                try:
                    self.begin("login")
                    self.authenticate()
                except PipelineError as e:
                    self.update(status=e.state, reason=e.reason)
                if self.status["status"] == "queued": self.run()
            self.settle_number()
        except (Exception, KeyboardInterrupt):
            # Retain the stage so a restart cannot mistake an interrupted write for a fresh job.
            self.update(status="unknown", reason="interrupted_or_local_error",
                        refund_deferred=bool(self.status.get("number_owned") and not self.status.get("account_created")))
        finally:
            self.update(active=False)

    def check_cancel(self):
        if (self.folder / "cancel.request").exists():
            raise PipelineError("cancelled_requires_readback" if self.status.get("registration_started") else "cancelled_before_registration", "cancelled")

    def settle_number(self):
        sms = self.cfg["sms"]
        if not sms.get("activation") or not self.status.get("number_owned") or self.status.get("account_created") or self.status.get("refund_confirmed"): return
        # Only proven creation failure, or lease expiry, releases the rented number.
        expired = timestamp(sms["expires_at"]) <= time.time()
        if expired and self.status.get("registration_started") and not self.status.get("expiry_checked"):
            # Password readback sends no SMS and is never a replay of registration.
            try:
                code, event = self.process([str(ROOT / "bin/appleweb.exe"), "-mode", "probe", "-session",
                                           str(self.folder / "icloud-web-session.json"), "-timeout", "90s"], 95, ignore_cancel=True)
                exists = code == 0 and event and event.get("state") == "account_exists"
            except PipelineError:
                exists = False
            self.update(expiry_checked=True, account_created=bool(exists), refund_deferred=False,
                        reason="account_exists_activation_expired" if exists else "expired_activation_account_unconfirmed")
            if exists: return
        if self.status["stage"] != "create" and not expired:
            self.update(refund_deferred=True)
            return
        safe_cancel = self.status["status"] == "cancelled" and not self.status.get("registration_started")
        if self.status["status"] not in {"failed", "usage"} and not expired and not safe_cancel:
            self.update(refund_deferred=True)
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


def execute(action, job_id=None, input_file=None):
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
            if action == "run": return status  # identical invocation is a read, not a retry
        else:
            if action != "run": raise PipelineError("job_not_found", "usage")
            validate(cfg)
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
        job = Job(folder, cfg, status)
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
                if job.status.get("refund_pending") or job.status.get("refund_deferred") or job.status.get("status") == "cancelled": job.settle_number()
                if job.status.get("status") == "ready" and timestamp(job.status["next_run_at"]) <= time.time():
                    try: job.keepalive()
                    except PipelineError as e: job.update(status=e.state, reason=e.reason)
        except (PipelineError, OSError, ValueError, KeyError):
            continue


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["run", "resume", "status", "keepalive", "refund", "cleanup", "maintain", "maintain-once"])
    parser.add_argument("--input", help="private JSON manifest (run only)")
    parser.add_argument("--job")
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
        result = execute(args.action, args.job, args.input)
    except PipelineError as e: result = {"status": e.state, "reason": e.reason}
    except Exception: result = {"status": "unknown", "reason": "local_state_unavailable"}
    print(json.dumps(result, ensure_ascii=False))
    return 0 if result.get("status") == "ready" else 3


if __name__ == "__main__":
    raise SystemExit(main())
