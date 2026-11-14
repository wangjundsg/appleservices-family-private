# Compare the two detection clients against one Roxy/Forge profile.
#
#   python tools/detect/summarize.py .cache/detect-baseline/baseline-2
#
# Reads A-playwright.json / B-selenium.json and emits a markdown comparison
# table plus the raw numbers behind it, so the choice of client is auditable.

import json
import os
import re
import sys


def load(run_dir, name):
    path = os.path.join(run_dir, name)
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def page(doc, page_id):
    if not doc:
        return None
    for p in doc.get("pages", []):
        if p.get("id") == page_id:
            return p
    return None


def sanny_failed(doc):
    p = page(doc, "sannysoft")
    if not p or not p.get("ok"):
        return None, []
    ex = p.get("extract") or {}
    rows = ex.get("rows") or []
    failed = [r["label"] for r in rows
              if any("failed" in (c.get("cls") or "") and "passed" not in (c.get("cls") or "")
                     for c in r.get("cells", []))]
    return len(failed), failed


def row_value(doc, page_id, label_regex):
    p = page(doc, page_id)
    if not p or not p.get("ok"):
        return None
    for r in (p.get("extract") or {}).get("rows") or []:
        if re.search(label_regex, r.get("label", ""), re.I):
            cells = r.get("cells") or []
            return cells[0].get("text") if cells else None
    return None


def creep_numbers(doc):
    p = page(doc, "creepjs")
    if not p or not p.get("ok"):
        return {}
    text = (p.get("extract") or {}).get("text") or ""
    out = {}
    m = re.search(r"chromium:\s*(\w+)", text)
    out["chromium"] = m.group(1) if m else "?"
    m = re.search(r"(\d+)%\s*like headless", text)
    out["like_headless"] = f"{m.group(1)}%" if m else "?"
    m = re.search(r"(\d+)%\s*headless", text)
    out["headless"] = f"{m.group(1)}%" if m else "?"
    m = re.search(r"(\d+)%\s*stealth", text)
    out["stealth"] = f"{m.group(1)}%" if m else "?"
    # Resistance block: shows what creepjs thinks it detected.
    m = re.search(r"Resistance[^\n]*\n(.*?)(?:\n\S+\.\d+ ?ms|\Z)", text, re.S)
    if m:
        out["resistance"] = " ".join(m.group(1).split())[:160]
    out["trust_mention"] = bool((p.get("extract") or {}).get("trustMention"))
    out["has_trust_node"] = bool((p.get("extract") or {}).get("hasTrustNode"))
    return out


def main():
    run_dir = sys.argv[1] if len(sys.argv) > 1 else ".cache/detect-baseline/baseline-2"
    a = load(run_dir, "A-playwright.json")
    b = load(run_dir, "B-selenium.json")

    rows = []

    def add(metric, av, bv):
        rows.append((metric, av, bv))

    add("客户端版本", (a or {}).get("client_version", "-"), (b or {}).get("selenium", "-"))
    for pid in ("sannysoft", "creepjs", "browserleaks-js"):
        pa, pb = page(a, pid), page(b, pid)
        add(f"{pid} 加载是否成功",
            "ok" if pa and pa.get("ok") else f"FAIL {pa.get('error') if pa else 'missing'}",
            "ok" if pb and pb.get("ok") else f"FAIL {pb.get('error') if pb else 'missing'}")

    # navigator.webdriver, read from the probe on all three pages
    def wd(doc):
        vals = []
        for pid in ("sannysoft", "creepjs", "browserleaks-js"):
            p = page(doc, pid)
            if p and p.get("ok"):
                vals.append(str((p.get("probe") or {}).get("webdriver")))
        return " / ".join(vals) if vals else "n/a"

    add("navigator.webdriver", wd(a), wd(b))
    add("sannysoft: WebDriver 行", str(row_value(a, "sannysoft", r"^WebDriver \(New\)")),
        str(row_value(b, "sannysoft", r"^WebDriver \(New\)")))
    add("sannysoft: WebDriver Advanced", str(row_value(a, "sannysoft", r"WebDriver Advanced")),
        str(row_value(b, "sannysoft", r"WebDriver Advanced")))
    add("sannysoft: Chrome 对象", str(row_value(a, "sannysoft", r"^Chrome \(New\)")),
        str(row_value(b, "sannysoft", r"^Chrome \(New\)")))
    add("sannysoft: Permissions", str(row_value(a, "sannysoft", r"^Permissions")),
        str(row_value(b, "sannysoft", r"^Permissions")))
    fa, la = sanny_failed(a)
    fb, lb = sanny_failed(b)
    add("sannysoft: 红项数量", f"{fa} {la[:4]}", f"{fb} {lb[:4]}")

    ca, cb = creep_numbers(a), creep_numbers(b)
    for key in ("chromium", "like_headless", "headless", "stealth", "trust_mention", "has_trust_node"):
        add(f"creepjs: {key}", str(ca.get(key)), str(cb.get(key)))
    add("creepjs: Resistance", ca.get("resistance", "-"), cb.get("resistance", "-"))

    def leaks_wd(doc):
        p = page(doc, "browserleaks-js")
        if not p or not p.get("ok"):
            return "n/a"
        lines = (p.get("extract") or {}).get("webdriverLines") or []
        return " | ".join(lines[:2]) if lines else "missing"

    add("browserleaks: webdriver 行", leaks_wd(a), leaks_wd(b))
    add("browserleaks: UA 行", "同 probe",
        "同 probe")

    # Injection traces: driver globals + DOM attributes + toString patching
    def inj(doc):
        p = page(doc, "sannysoft")
        if not p or not p.get("ok"):
            return "n/a"
        pr = p.get("probe") or {}
        g = pr.get("suspectGlobals") or []
        dom = pr.get("domTrace") or []
        native = pr.get("toStrNative")
        return f"globals={len(g)}{g[:5]} dom={len(dom)} toStringNative={native}"

    add("注入痕迹（主世界全局/DOM/toString）", inj(a), inj(b))

    def chrome_keys(doc):
        p = page(doc, "sannysoft")
        if not p or not p.get("ok"):
            return "n/a"
        pr = p.get("probe") or {}
        return f"{pr.get('chromeType')} {pr.get('chromeKeys')} runtime={pr.get('hasChromeRuntime')}"

    add("window.chrome", chrome_keys(a), chrome_keys(b))

    out = ["| 指标 | A: Playwright connectOverCDP | B: Selenium attach |", "|---|---|---|"]
    for metric, av, bv in rows:
        out.append(f"| {metric} | {av} | {bv} |")
    md = "\n".join(out)
    print(md)
    with open(os.path.join(run_dir, "comparison.md"), "w", encoding="utf-8") as fh:
        fh.write(md + "\n")
    print(f"\nwritten: {os.path.join(run_dir, 'comparison.md')}")


if __name__ == "__main__":
    main()
