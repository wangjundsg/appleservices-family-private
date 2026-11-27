"""Local OCR with conservative acceptance; confidence is not measured accuracy."""
from __future__ import annotations
import argparse
from functools import lru_cache
import io
import re
import sys
import os
import subprocess
import tempfile
import importlib.util
from pathlib import Path
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

@lru_cache(maxsize=2)
def paddle_reader(engine):
    name = "PP-OCRv6_small_rec" if engine == "paddle-v6" else "en_PP-OCRv5_mobile_rec"
    model_dir = Path.home() / ".paddlex" / "official_models" / name
    # Live runs must never install or download a model while holding a mailbox.
    if not all((model_dir / file).is_file() for file in ("inference.json", "inference.pdiparams", "inference.yml")):
        raise FileNotFoundError("local_model_missing")
    os.environ["PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK"] = "True"
    from paddleocr import TextRecognition
    return TextRecognition(model_name=name, model_dir=str(model_dir), device="cpu")

def paddle_worker(path, engine, min_confidence):
    root = Path(__file__).resolve().parents[2]
    executable = Path(sys.executable) if importlib.util.find_spec("paddleocr") is not None else root / ".tools" / "ocr-paddle" / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
    if not executable.is_file(): return ""
    try:
        # Native inference libraries also write to stdout, bypassing redirect_stdout.
        with tempfile.TemporaryDirectory(prefix="local-ocr-") as directory:
            output = Path(directory) / "answer.txt"
            result = subprocess.run([str(executable), str(Path(__file__).resolve()), str(Path(path).resolve()),
                                     "--engine", engine, "--min-confidence", str(min_confidence), "--result-file", str(output)],
                                    capture_output=True, text=True, timeout=17,
                                    creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
            answer = output.read_text(encoding="utf-8").strip() if result.returncode == 0 else ""
            return answer if valid_text(answer) else ""
    except (OSError, subprocess.TimeoutExpired):
        return ""

def read_local(path, engine="paddle-v6", min_confidence=None):
    if min_confidence is None: min_confidence = 0.98 if engine == "paddle-v6" else 0.85
    # Model startup/download messages are diagnostics, never an OCR answer.
    with redirect_stdout(sys.stderr):
        if engine in ("paddle", "paddle-v6") and importlib.util.find_spec("paddleocr") is None:
            return paddle_worker(path, engine, min_confidence)
        return _read_local(path, engine, min_confidence)

def _read_local(path, engine, min_confidence):
    try:
        with open(path, "rb") as handle: raw = handle.read()
        if not image_usable(raw): return ""
        if engine in ("paddle", "paddle-v6"):
            model = paddle_reader(engine)
            results = list(model.predict(input=path, batch_size=1))
            if len(results) != 1: return ""
            item = results[0].json
            if isinstance(item, str):
                import json
                item = json.loads(item)
            item = item.get("res", item)
            text, score = item.get("rec_text", ""), item.get("rec_score", 0)
            return text if valid_text(text) and min_confidence <= float(score) <= 1 else ""
        if engine not in ("consensus", "ddddocr"): return ""
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
    parser.add_argument("--engine", choices=["consensus", "ddddocr", "paddle", "paddle-v6"], default="paddle-v6")
    parser.add_argument("--min-confidence", type=float)
    parser.add_argument("--result-file", help=argparse.SUPPRESS)
    args = parser.parse_args()
    confidence = args.min_confidence if args.min_confidence is not None else (0.98 if args.engine == "paddle-v6" else 0.85)
    if args.result_file:
        # Worker never launches another worker if its environment is incomplete.
        with redirect_stdout(sys.stderr): answer = _read_local(args.image, args.engine, confidence)
        Path(args.result_file).write_text(answer, encoding="utf-8")
    else:
        answer = paddle_worker(args.image, args.engine, confidence) if args.engine in ("paddle", "paddle-v6") else read_local(args.image, args.engine, confidence)
        print(answer, end="")
    return 0 if answer else 1

if __name__ == "__main__": raise SystemExit(main())
