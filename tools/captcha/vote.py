"""Consensus OCR for Apple's account-creation image challenge (local only).

Apple serves a 126x56 bitmap carrying 4-5 distorted alphanumerics. One answer
belongs to one image (the driver re-reads only after Apple swaps the picture),
so a single read is the whole budget: this script spends it on variety instead
of a second guess.

Method
  * render the image through a handful of deterministic variants (raw,
    autocontrast, upscales, sharpened, threshold, median-denoised);
  * read every variant with both ddddocr model variants (beta and plain);
  * keep the reads whose length is the most common one (a lone hallucinated
    length must not decide the shape of the answer);
  * vote character by character; the decoded text is the vote, ddddocr's own
    confidence only weighs it.

The probabilities ddddocr returns are CTC timesteps, not one row per character,
so they are deliberately NOT used for per-position voting: an earlier version
did and turned VPMZS into VPMZV (the trailing timestep is blank).

Nothing leaves the machine and no paid provider is contacted.

Usage:
    python tools/captcha/vote.py <image.png>

Prints the consensus string (upper case) on stdout; exits non-zero when no read
clears the minimum length.
"""

from __future__ import annotations

import io
import sys
from collections import Counter, defaultdict

try:
    from PIL import Image, ImageEnhance, ImageFilter, ImageOps
except Exception:  # pragma: no cover - Pillow ships with this workspace
    Image = None

MIN_LENGTH = 4
MAX_LENGTH = 6
_MODELS = {}


def model(beta: bool):
    """ddddocr model, loaded once per process (a load costs ~1s)."""
    if beta not in _MODELS:
        import ddddocr

        _MODELS[beta] = ddddocr.DdddOcr(show_ad=False, beta=beta)
    return _MODELS[beta]


def variants(image):
    grey = image.convert("L")
    ac = ImageOps.autocontrast(grey)
    out = {
        "raw": grey,
        "ac": ac,
        "sharp": ImageEnhance.Sharpness(ac).enhance(2.0),
        "thr140": ac.point(lambda p: 255 if p > 140 else 0),
        "med": ac.filter(ImageFilter.MedianFilter(3)),
    }
    for scale in (2, 4):
        out[f"x{scale}"] = grey.resize((grey.width * scale, grey.height * scale), Image.LANCZOS)
    return out


def to_bytes(image) -> bytes:
    buf = io.BytesIO()
    image.save(buf, format="PNG")
    return buf.getvalue()


def read_all(image) -> list:
    """Every model x variant read as (text, confidence)."""
    out = []
    for name, variant in variants(image).items():
        raw = to_bytes(variant)
        for beta in (True, False):
            try:
                got = model(beta).classification(raw, probability=True)
            except Exception:
                continue
            if not isinstance(got, dict):
                continue
            text = "".join(ch for ch in str(got.get("text") or "") if ch.isalnum())
            if text:
                out.append((text, float(got.get("confidence") or 0.0), f"{'beta' if beta else 'plain'}:{name}"))
    return out


def consensus(reads: list):
    if not reads:
        return "", {}
    lengths = Counter(len(text) for text, _, _ in reads)
    # Apple's alphabet is 4-6 characters, so a read outside that window is a
    # truncation or a hallucination and never wins the length election: on
    # .cache/os-ab/M/captcha-2.png the majority read two characters ("a4") while
    # the sharpened bitmap read the true four ("AI4F").
    plausible = {L: n for L, n in lengths.items() if MIN_LENGTH <= L <= MAX_LENGTH}
    pool = plausible or lengths
    # The most common plausible length wins; ties go to the longer read.
    length = max(pool.items(), key=lambda kv: (kv[1], kv[0]))[0]
    rows = [row for row in reads if len(row[0]) == length]
    votes = [defaultdict(float) for _ in range(length)]
    for text, confidence, _ in rows:
        weight = 0.5 + 0.5 * min(1.0, max(0.0, confidence))
        for index, ch in enumerate(text):
            votes[index][ch.upper()] += weight
    text = "".join(max(v.items(), key=lambda kv: kv[1])[0] for v in votes)
    detail = {
        "length": length,
        "reads": len(rows),
        "lengths_seen": dict(lengths),
        # A small margin is the honest signal that this read is a coin flip.
        "margin": [round(sorted(v.values(), reverse=True)[0] - (sorted(v.values(), reverse=True)[1] if len(v) > 1 else 0.0), 3) for v in votes],
        "spread": [len(v) for v in votes],
        "sources": sorted({src for _, _, src in rows}),
    }
    return text, detail


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: vote.py <image>", file=sys.stderr)
        return 2
    if Image is None:
        print("vote.py: Pillow missing", file=sys.stderr)
        return 2
    try:
        image = Image.open(sys.argv[1])
    except Exception as exc:
        print(f"vote.py: unreadable image ({type(exc).__name__})", file=sys.stderr)
        return 2
    tries = read_all(image)
    text, detail = consensus(tries)
    import json

    if len(text) < MIN_LENGTH:
        # Fall back to a typed-but-likely-wrong answer rather than returning
        # nothing: the driver refuses an answer shorter than four characters and
        # would re-read the same image, trip its duplicate breaker and end the
        # run with a rented number wasted. A wrong answer costs one attempt and
        # makes Apple rotate the picture, which is the better trade.
        best = max(tries, key=lambda row: (len(row[0]), row[1])) if tries else ("", 0.0, "")
        guess = (best[0] + "XXXX")[:4].upper()
        print("vote " + json.dumps({**detail, "pad": True, "best_read": best[0], "best_src": best[2]}, separators=(",", ":")), file=sys.stderr)
        if len(guess) >= MIN_LENGTH:
            print(guess, end="")
            return 0
        print("", end="")
        return 1
    print("vote " + json.dumps(detail, separators=(",", ":")), file=sys.stderr)
    print(text, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
