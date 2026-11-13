"""Offline OCR for Apple's account-creation image captcha.

Apple serves a 126x56 JPEG-style image with 4-5 distorted characters. ddddocr
handles this class of captcha locally, so the creation pipeline no longer needs
a human to read each image.

Usage:
    python tools/captcha/ocr.py <image.png>

Prints the recognised characters on stdout. Exits non-zero when nothing usable
came back, so the caller can fall back to a human read.
"""

import sys

import ddddocr


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: ocr.py <image>", file=sys.stderr)
        return 2
    with open(sys.argv[1], "rb") as handle:
        image = handle.read()
    # beta=True enables the model variant that handles more distortion.
    reader = ddddocr.DdddOcr(show_ad=False, beta=True)
    text = (reader.classification(image) or "").strip()
    text = "".join(ch for ch in text if ch.isalnum())
    if len(text) < 3:
        print("", end="")
        return 1
    print(text, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
