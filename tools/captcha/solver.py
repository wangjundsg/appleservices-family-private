"""Image-challenge solver with a local-first fallback chain.

Order:
  1. local OCR (accepted only when confidence and model agreement pass)
  2. remote solver (paid, only when the local read is unusable)
  3. caller refreshes the image and retries

Providers implemented: yescaptcha (default), 2captcha, capsolver.
Keys come from --key or the provider's env var; never from the command line
of a parent process when possible.

Usage:
  python tools/captcha/solver.py --provider yes --key XXX image.png
  python tools/captcha/solver.py --provider yes image.png      # key from env
  python tools/captcha/solver.py --dry-run image.png           # no network
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

PROVIDERS = {
    "yes": {
        "env": "YESCAPTCHA_KEY",
        "create": "https://api.yescaptcha.com/createTask",
        "result": "https://api.yescaptcha.com/getTaskResult",
    },
    "2captcha": {
        "env": "TWOCAPTCHA_KEY",
        "create": "https://2captcha.com/in.php",
        "result": "https://2captcha.com/res.php",
    },
    "capsolver": {
        "env": "CAPSOLVER_KEY",
        "create": "https://api.capsolver.com/createTask",
        "result": "https://api.capsolver.com/getTaskResult",
    },
}


def post_json(url: str, payload: dict, timeout: int = 30) -> dict:
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def local_ocr(path: str) -> str:
    from ocr import read_local
    return read_local(path)


def solve_yes(key: str, image_b64: str, timeout: int) -> str:
    # Both official nodes share the API; a task must stay on its creation node.
    endpoint = os.environ.get("YESCAPTCHA_API_BASE", "https://api.yescaptcha.com").rstrip("/")
    if endpoint not in {"https://api.yescaptcha.com", "https://cn.yescaptcha.com"}:
        raise ValueError("unsupported_yescaptcha_endpoint")
    created = post_json(
        endpoint + "/createTask",
        {"clientKey": key, "task": {"type": "ImageToTextTask", "body": image_b64}},
    )
    task_id = created.get("taskId")
    if not task_id:
        return ""
    deadline = time.time() + timeout
    while time.time() < deadline:
        time.sleep(2)
        got = post_json(
            endpoint + "/getTaskResult", {"clientKey": key, "taskId": task_id}
        )
        if got.get("status") == "ready":
            return (got.get("solution") or {}).get("text", "").strip()
        if got.get("errorId"):
            return ""
    return ""


def solve_2captcha(key: str, image_b64: str, timeout: int) -> str:
    body = urllib.parse.urlencode(
        {"key": key, "method": "base64", "body": image_b64, "json": 1}
    ).encode()
    req = urllib.request.Request(
        PROVIDERS["2captcha"]["create"],
        data=body,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        created = json.loads(resp.read().decode())
    task_id = created.get("request")
    if not task_id:
        return ""
    deadline = time.time() + timeout
    while time.time() < deadline:
        time.sleep(3)
        q = urllib.parse.urlencode(
            {"key": key, "action": "get", "id": task_id, "json": 1}
        )
        with urllib.request.urlopen(
            f"{PROVIDERS['2captcha']['result']}?{q}", timeout=30
        ) as resp:
            got = json.loads(resp.read().decode())
        if got.get("status") == 1:
            return str(got.get("request", "")).strip()
        if got.get("request") not in ("CAPCHA_NOT_READY", "CAPTCHA_NOT_READY"):
            return ""
    return ""


def solve_capsolver(key: str, image_b64: str, timeout: int) -> str:
    created = post_json(
        PROVIDERS["capsolver"]["create"],
        {
            "clientKey": key,
            "task": {"type": "ImageToTextTask", "body": image_b64},
        },
    )
    task_id = created.get("taskId")
    if not task_id:
        return ""
    deadline = time.time() + timeout
    while time.time() < deadline:
        time.sleep(2)
        got = post_json(
            PROVIDERS["capsolver"]["result"],
            {"clientKey": key, "taskId": task_id},
        )
        if got.get("status") == "ready":
            return (got.get("solution") or {}).get("text", "").strip()
        if got.get("errorId"):
            return ""
    return ""


SOLVERS = {
    "yes": solve_yes,
    "2captcha": solve_2captcha,
    "capsolver": solve_capsolver,
}


def preprocess(path: str, scale: int = 4) -> bytes:
    """Upscale and harden contrast before the paid read.

    Apple's image challenge is 160x70 with heavy noise; at native size the online
    readers confuse B/R and 8/3 often enough that a five-attempt run can burn out
    on near misses (2026-11-14: OCR answered DHCBR for DhCBB). A 4x LANCZOS
    upscale with a contrast stretch is retained for compatibility; its accuracy
    needs a labelled evaluation set. Falls back to raw bytes without Pillow.
    """
    try:
        from PIL import Image, ImageEnhance, ImageOps
    except Exception:
        with open(path, "rb") as handle:
            return handle.read()
    try:
        img = Image.open(path).convert("RGB")
        img = ImageOps.autocontrast(img)
        img = ImageEnhance.Sharpness(img).enhance(2.0)
        img = img.resize((img.width * scale, img.height * scale), Image.LANCZOS)
        import io

        # The online readers reject payloads above a few hundred KB
        # (ERROR_ZERO_CAPTCHA_FILESIZE), and a 4x LANCZOS PNG of a noisy
        # challenge blows past that. Re-encode as JPEG and shrink until it fits.
        limit = 90_000
        for quality in (90, 80, 70, 60, 50):
            buf = io.BytesIO()
            img.save(buf, format="JPEG", quality=quality, optimize=True)
            data = buf.getvalue()
            import base64 as _b64

            if len(_b64.b64encode(data)) <= limit:
                return data
        while img.width > 80:
            img = img.resize((img.width // 2, img.height // 2), Image.LANCZOS)
            buf = io.BytesIO()
            img.save(buf, format="JPEG", quality=60, optimize=True)
            data = buf.getvalue()
            import base64 as _b64

            if len(_b64.b64encode(data)) <= limit:
                return data
        return data
    except Exception:
        with open(path, "rb") as handle:
            return handle.read()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("image")
    ap.add_argument("--provider", default="yes", choices=sorted(PROVIDERS))
    ap.add_argument("--key", default="")
    ap.add_argument("--timeout", type=int, default=60)
    ap.add_argument("--skip-local", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--upscale", type=int, default=4, help="pre-upscale factor before the paid read (0 disables)")
    args = ap.parse_args()

    if not os.path.exists(args.image):
        print("", end="")
        return 2

    if not args.skip_local:
        local = local_ocr(args.image)
        if local:
            print(local, end="")
            return 0

    key = args.key or os.environ.get(PROVIDERS[args.provider]["env"], "")
    raw = preprocess(args.image, args.upscale) if args.upscale and args.upscale > 0 else open(args.image, "rb").read()
    image_b64 = base64.b64encode(raw).decode()

    if args.dry_run:
        print(
            f"dry-run provider={args.provider} key={'set' if key else 'missing'} "
            f"bytes={len(image_b64)}",
            file=sys.stderr,
        )
        print("", end="")
        return 1
    if not key:
        print(json.dumps({"solver_error":"missing_key"}), file=sys.stderr)
        print("", end="")
        return 1

    try:
        text = SOLVERS[args.provider](key, image_b64, args.timeout)
    except urllib.error.HTTPError as exc:
        print(json.dumps({"solver_error":f"http_{exc.code}"}), file=sys.stderr)
        text = ""
    except (urllib.error.URLError, TimeoutError) as exc:
        print(json.dumps({"solver_error":"network_error"}), file=sys.stderr)
        text = ""
    except ValueError as exc:
        print(f"solver: {type(exc).__name__}", file=sys.stderr)
        text = ""

    from ocr import valid_text
    if not valid_text(text):
        print("", end="")
        return 1
    print(text, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
