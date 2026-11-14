# Client B -- Selenium (Python) attached to the SAME Roxy/Forge debugger port.
#
# Selenium has no "raw" CDP transport: every command goes
#   python -> chromedriver (spawned here) -> DevTools (127.0.0.1:debuggerPort)
# The attach is expressed as chromeOptions.debuggerAddress, which is the only
# WebDriver-level way to say "do not launch, use that one".
#
#   python tools/detect/client_selenium.py --port 51234 --out .cache/x/B.json \
#          --driver "D:\\Workbench\\data\\roxy-userdata\\chrome-bin\\155\\chromedriver.exe"

import argparse
import base64
import json
import os
import re
import sys
import time
import traceback

from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service

HERE = os.path.dirname(os.path.abspath(__file__))
PROBE = open(os.path.join(HERE, "probe.js"), encoding="utf-8").read()

# ChromeDriver does not await promises in Execute Script: an async probe comes
# back as None for every field. Execute Async Script does await, so the same
# probe source is wrapped in the callback dance instead of being rewritten.
PROBE_ASYNC_WRAPPER = (
    "const cb = arguments[arguments.length - 1];"
    "Promise.resolve((" + PROBE + "))"
    ".then(function (r) { cb(r); })"
    ".catch(function (e) { cb({ __error: String(e).slice(0, 200) }); });"
)

PAGES = [
    ("sannysoft", "https://bot.sannysoft.com/", 6),
    ("creepjs", "https://abrahamjuliot.github.io/creepjs/", 14),
    ("browserleaks-js", "https://browserleaks.com/javascript", 6),
]

SANNY_ROWS = """
const rows = [];
for (const tr of document.querySelectorAll('tr')) {
  const tds = [...tr.querySelectorAll('td')];
  if (tds.length < 2) continue;
  const label = (tds[0].innerText || '').trim().replace(/\\s+/g, ' ');
  const cells = tds.slice(1).map(td => ({
    text: (td.innerText || '').trim().replace(/\\s+/g, ' '),
    cls: String(td.className || ''), id: td.id || ''
  }));
  rows.push({ label, cells });
}
const failed = rows.filter(r => r.cells.some(c => /failed/.test(c.cls) && !/passed/.test(c.cls)));
return { rows: rows, failed: failed.map(r => r.label + ' :: ' + r.cells.map(c => c.text).join(' / ')) };
"""

LEAKS_ROWS = """
const text = document.body.innerText || '';
const lines = text.split('\\n').map(s => s.trim()).filter(Boolean);
const pick = re => lines.filter(l => re.test(l)).slice(0, 12);
return { webdriverLines: pick(/webdriver/i), userAgentLines: pick(/user.?agent/i), lines: lines.slice(0, 160) };
"""


def extract(driver, page_id):
    if page_id == "sannysoft":
        return driver.execute_script(SANNY_ROWS)
    if page_id == "browserleaks-js":
        return driver.execute_script(LEAKS_ROWS)
    return driver.execute_script(
        "window.scrollTo(0, 0);"
        "const text = document.body.innerText || '';"
        "return { text: text.slice(0, 16000),"
        " hasTrustNode: Boolean(document.querySelector('#fingerprint-data')),"
        " trustMention: /trust/i.test(document.body.innerHTML || ''),"
        " headlessLine: (text.match(/like headless[^\\n]*/i) || [''])[0],"
        " stealthLine: (text.match(/stealth[^\\n]*/i) || [''])[0],"
        " htmlHead: (document.body.innerHTML || '').slice(0, 2500) };"
    )


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--driver", default="")
    args = ap.parse_args()

    out_path = os.path.abspath(args.out)
    shot_dir = os.path.join(os.path.dirname(out_path), "shots")
    os.makedirs(shot_dir, exist_ok=True)

    result = {
        "client": "selenium-attach",
        "port": args.port,
        "selenium": __import__("selenium").__version__,
        "driver_path": args.driver,
        "pages": [],
        "errors": [],
    }
    started = time.time()

    opts = Options()
    # The only supported "use that browser, do not launch one" switch.
    # Launch args (--disable-blink-features, excludeSwitches, ...) are pointless
    # here: the browser already runs, chromedriver only attaches. chromedriver
    # 155 also rejects the old excludeSwitches capability outright
    # ("unrecognized chrome option: excludeSwitches").
    opts.debugger_address = f"127.0.0.1:{args.port}"
    service = Service(executable_path=args.driver) if args.driver else Service()
    driver = None
    try:
        driver = webdriver.Chrome(service=service, options=opts)
        caps = driver.capabilities or {}
        result["browser_version"] = caps.get("browserVersion", "")
        result["chrome_caps"] = {k: v for k, v in caps.items() if k in ("browserName", "browserVersion", "platformName")}
        for page_id, url, settle in PAGES:
            rec = {"id": page_id, "url": url, "ok": False}
            try:
                t0 = time.time()
                driver.get(url)
                time.sleep(settle)
                rec["load_ms"] = int((time.time() - t0) * 1000)
                rec["title"] = driver.title
                rec["probe"] = driver.execute_async_script(PROBE_ASYNC_WRAPPER)
                rec["extract"] = extract(driver, page_id)
                shot = os.path.join(shot_dir, f"B-{page_id}.png")
                with open(shot, "wb") as fh:
                    fh.write(base64.b64decode(driver.get_screenshot_as_base64()))
                rec["screenshot"] = os.path.relpath(shot, os.path.dirname(out_path))
                rec["ok"] = True
            except Exception as exc:  # noqa: BLE001 - one bad page must not kill the run
                rec["error"] = f"{type(exc).__name__}: {str(exc)[:350]}"
                result["errors"].append(f"{page_id}: {rec['error']}")
            result["pages"].append(rec)
    except Exception as exc:  # noqa: BLE001
        result["attach_failed"] = True
        result["errors"].append("attach: " + f"{type(exc).__name__}: {str(exc)[:400]}")
        result["trace"] = traceback.format_exc()[-1500:]
    finally:
        if driver is not None:
            try:
                driver.quit()
            except Exception:  # noqa: BLE001
                pass
        result["elapsed_ms"] = int((time.time() - started) * 1000)
        with open(out_path, "w", encoding="utf-8") as fh:
            json.dump(result, fh, ensure_ascii=False, indent=2)
        print(json.dumps({
            "client": result["client"],
            "ok": sum(1 for p in result["pages"] if p["ok"]),
            "errors": result["errors"],
            "out": out_path,
            "elapsed_ms": result["elapsed_ms"],
        }))
    return 3 if result["errors"] else 0


if __name__ == "__main__":
    sys.exit(main())
