"""Evaluate a private, labelled manifest without printing images or answers."""
import argparse
import json
import time
from pathlib import Path
from ocr import read_local, valid_text

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("manifest")
    parser.add_argument("--engine",choices=["ddddocr","consensus","paddle","paddle-v6"],default="paddle-v6")
    args=parser.parse_args()
    entries=json.loads(Path(args.manifest).read_text(encoding="utf-8-sig"))
    if not isinstance(entries,list) or not entries or any(not valid_text(item.get("expected")) for item in entries):
        raise ValueError("labelled_manifest_invalid")
    started=time.monotonic();accepted=correct=strict=0
    for item in entries:
        result=read_local(item["image"],args.engine)
        accepted+=bool(result);correct+=result.casefold()==item["expected"].casefold();strict+=result==item["expected"]
    print(json.dumps({"engine":args.engine,"samples":len(entries),"accepted":accepted,"exact":correct,
        "exact_rate":correct/len(entries),"strict_exact":strict,"case_sensitive":False,
        "coverage":accepted/len(entries),"seconds":round(time.monotonic()-started,3)}))

if __name__=="__main__":main()
