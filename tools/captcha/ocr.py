"""Local OCR with conservative acceptance; confidence is not measured accuracy."""
from __future__ import annotations
import argparse
from functools import lru_cache
import io
import re
import sys
from contextlib import redirect_stdout

def valid_text(text):
    return isinstance(text, str) and re.fullmatch(r"[A-Za-z0-9]{4,6}", text) is not None

def image_usable(raw):
    from PIL import Image, ImageStat
    try:
        image = Image.open(io.BytesIO(raw)).convert("L")
        return image.width >= 30 and image.height >= 12 and ImageStat.Stat(image).stddev[0] >= 3
    except Exception:
        return False

def result_value(result):
    if not isinstance(result, dict): return "", 0.0
    if "text" in result:
        text, score = result.get("text", ""), result.get("confidence", 0)
    else:
        chars, rows = result.get("charsets", []), result.get("probability", [])
        if not rows or not chars or any(len(row) != len(chars) for row in rows): return "", 0.0
        text = "".join(chars[max(range(len(row)), key=row.__getitem__)] for row in rows)
        score = min(max(row) for row in rows)
    try: score = float(score)
    except (TypeError, ValueError): return "", 0.0
    return (text, score) if valid_text(text) and 0 <= score <= 1 else ("", 0.0)

@lru_cache(maxsize=2)
def reader(beta):
    import ddddocr
    return ddddocr.DdddOcr(show_ad=False, beta=beta)

@lru_cache(maxsize=1)
def paddle_reader():
    from paddleocr import TextRecognition
    return TextRecognition(model_name="en_PP-OCRv5_mobile_rec", device="cpu")

def read_local(path, engine="consensus", min_confidence=0.85):
    # Model startup/download messages are diagnostics, never an OCR answer.
    with redirect_stdout(sys.stderr):
        return _read_local(path, engine, min_confidence)

def _read_local(path, engine="consensus", min_confidence=0.85):
    try:
        with open(path, "rb") as handle: raw = handle.read()
        if not image_usable(raw): return ""
        if engine == "paddle":
            model = paddle_reader()
            results = list(model.predict(input=path, batch_size=1))
            if len(results) != 1: return ""
            item = results[0].json
            if isinstance(item, str):
                import json
                item = json.loads(item)
            item = item.get("res", item)
            text, score = item.get("rec_text", ""), item.get("rec_score", 0)
            return text if valid_text(text) and float(score) >= min_confidence else ""
        first, score = result_value(reader(True).classification(raw, probability=True))
        if not first or score < min_confidence: return ""
        if engine == "consensus":
            second, score = result_value(reader(False).classification(raw, probability=True))
            if second != first or score < min_confidence: return ""
        return first
    except Exception:
        return ""

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("image")
    parser.add_argument("--engine", choices=["consensus", "ddddocr", "paddle"], default="consensus")
    args = parser.parse_args()
    answer = read_local(args.image, args.engine)
    print(answer, end="")
    return 0 if answer else 1

if __name__ == "__main__": raise SystemExit(main())
