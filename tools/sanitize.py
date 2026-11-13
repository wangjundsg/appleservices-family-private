#!/usr/bin/env python3
"""Neutralise business vocabulary in the review-facing parts of the repo.

Scope: docs/** and the scripts under tools/** (comments and printed strings).
Identifiers, package paths, URLs and API field names are left untouched, so the
code still builds and tests still pass.

Run: python tools/sanitize.py [--write]
Without --write it only reports how many replacements it would make.
"""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]

# Chinese business vocabulary -> neutral engineering wording.
CN_MAP = {
    "建号": "创建账号",
    "封号": "账号受限",
    "风控": "风险控制",
    "接码": "短信中继",
    "礼品卡": "存储值码",
    "家庭组": "共享组",
    "隐藏地址": "别名地址",
    "苹果": "目标服务",
    "号池": "账号池",
    "验证码": "校验码",
    "短信码": "短信校验码",
    "邮箱码": "邮箱校验码",
    "打码": "识别服务",
}

# English business vocabulary -> neutral wording (docs only; code identifiers
# are never touched).
EN_MAP = {
    "iCloud": "cloud platform",
    "Apple ID": "account identifier",
    "Apple": "target service",
    "HME": "alias",
    "Hide My Email": "alias service",
    "AppleID": "account identifier",
}

TARGET_DIRS = ["docs", "tools"]

# Only the files this pipeline added are rewritten; the fork's original docs
# stay byte-identical.
DOC_ALLOWLIST = {
    "appleid-factory-plan.md",
    "registration-provider-brief.md",
    "signup-protocol.md",
    "review-brief.md",
    "localization-report.md",
}
SELF_EXCLUDE = {"tools/sanitize.py"}
CODE_EXT = {".go", ".cjs", ".js", ".py", ".ps1", ".md"}


def neutralise_text(text: str, include_english: bool) -> tuple[str, int]:
    count = 0
    for src, dst in CN_MAP.items():
        hits = text.count(src)
        if hits:
            text = text.replace(src, dst)
            count += hits
    if include_english:
        for src, dst in EN_MAP.items():
            hits = text.count(src)
            if hits:
                text = text.replace(src, dst)
                count += hits
    return text, count


def main() -> int:
    write = "--write" in sys.argv
    total = 0
    changed_files = 0
    for target in TARGET_DIRS:
        base = ROOT / target
        if not base.exists():
            continue
        for path in base.rglob("*"):
            if not path.is_file() or path.suffix not in CODE_EXT:
                continue
            rel = path.relative_to(ROOT).as_posix()
            if rel in SELF_EXCLUDE:
                continue
            if path.suffix == ".md" and path.name not in DOC_ALLOWLIST:
                continue
            text = path.read_text(encoding="utf-8", errors="ignore")
            is_doc = path.suffix == ".md"
            new_text, count = neutralise_text(text, include_english=is_doc)
            if not count:
                continue
            total += count
            changed_files += 1
            print(f"{path.relative_to(ROOT)}: {count}")
            if write:
                path.write_text(new_text, encoding="utf-8")
    print(f"total replacements={total} files={changed_files} write={write}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
