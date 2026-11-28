"""Mac NAC emulation on Windows using pinned pypush hooks and local hardware.

Third-party runtime belongs in --runtime; no donor data.plist is used. Missing
board fields can be filled with explicitly synthetic diagnostic values, whose
acceptance by Apple remains unverified. Original profile is never written.
"""
import argparse
import ctypes
import hashlib
import json
import logging
import os
from pathlib import Path
import plistlib
import re
import sys
import subprocess
import uuid

import requests


def fetch_certificate(transport, output):
    command = [str(transport), "-op", "cert", "-out", str(output)]
    try:
        subprocess.run(command, check=True, timeout=40)
        return []
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
        # Only this public GET is replayed. Carry the selected route into the
        # subsequent session POST, which must never be retried automatically.
        subprocess.run(command + ["-ipv4"], check=True, timeout=40)
        return ["-ipv4"]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--profile", type=Path, required=True)
    ap.add_argument("--runtime", type=Path, required=True)
    ap.add_argument("--encrypt-dll", type=Path, required=True)
    ap.add_argument("--validation-out", type=Path, required=True)
    ap.add_argument("--synthetic-missing-board-fields", action="store_true")
    ap.add_argument("--transport", type=Path, help="compiled nacexchange with GSA's verified Apple TLS transport")
    args = ap.parse_args()
    profile, runtime, dll, output = [p.resolve() for p in (args.profile, args.runtime, args.encrypt_dll, args.validation_out)]
    roots_source = (Path(__file__).resolve().parents[1] / "gsa" / "apple_root_ca.go").read_text()
    apple_root = re.search(r"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----", roots_source, re.S).group()
    ca_path = runtime / "apple-ca-bundle.pem"
    ca_path.write_text(Path(requests.certs.where()).read_text() + "\n" + apple_root + "\n")
    with ca_path.open("a") as bundle:
        bundle.write((Path(__file__).resolve().parents[1] / "gsa" / "apple_root_ca_g3.pem").read_text())
    apple = requests.Session()
    apple.verify = str(ca_path)
    transport = args.transport.resolve() if args.transport else None
    if output.exists():
        raise SystemExit("Use a new private validation output path")
    profile_data = plistlib.loads(profile.read_bytes())
    hw = profile_data["os_config"]["inner"].copy()
    synthetic = bool(profile_data.get("appleservices_diagnostic", {}).get("synthetic_board_fields"))
    if synthetic and not args.synthetic_missing_board_fields:
        raise SystemExit("Profile contains synthetic board fields; choose explicit diagnostic mode")
    if not hw["rom"] or not hw["mlb"]:
        if not args.synthetic_missing_board_fields:
            raise SystemExit("Missing ROM/MLB; choose explicit synthetic diagnostic mode or provide hardware")
        hw["rom"] = hw["rom"] or bytes(hw["io_mac_address"])
        hw["mlb"] = hw["mlb"] or "C02" + hashlib.sha256(hw["platform_uuid"].encode()).hexdigest()[:14].upper()
        synthetic = True
    if len(hw["rom"]) != 6 or not hw["mlb"]:
        raise SystemExit("Invalid ROM/MLB input lengths")
    ctypes.windll.kernel32.SetErrorMode(0x8003)
    lib = ctypes.CDLL(str(dll))
    encrypt = lib.mac_io_encrypt
    encrypt.argtypes = [ctypes.c_void_p, ctypes.c_uint64, ctypes.c_void_p]
    encrypt.restype = None

    def enc(data):
        data = bytes(data)
        src, dst = ctypes.create_string_buffer(data), ctypes.create_string_buffer(17)
        encrypt(src, len(data), dst)
        return dst.raw

    for name, raw in [("platform_serial_number", hw["platform_serial_number"].encode()),
                      ("platform_uuid", uuid.UUID(hw["platform_uuid"]).bytes),
                      ("root_disk_uuid", uuid.UUID(hw["root_disk_uuid"]).bytes),
                      ("rom", hw["rom"]), ("mlb", hw["mlb"].encode())]:
        if not hw[name + "_enc"]:
            hw[name + "_enc"] = enc(raw)
    mapping = {"Gq3489ugfi": hw["platform_serial_number_enc"], "Fyp98tpgj": hw["platform_uuid_enc"],
               "kbjfrfpoJU": hw["root_disk_uuid_enc"], "abKPld1EcMni": hw["mlb_enc"], "oycqAZloTNDm": hw["rom_enc"],
               "IOMACAddress": bytes(hw["io_mac_address"]), "IOPlatformSerialNumber": hw["platform_serial_number"],
               "IOPlatformUUID": hw["platform_uuid"], "product-name": hw["product_name"].encode() + b"\0",
               "board-id": hw["board_id"].encode() + b"\0",
               "4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:MLB": hw["mlb"].encode(),
               "4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:ROM": hw["rom"]}
    (runtime / "emulated" / "data.plist").write_bytes(plistlib.dumps({"iokit": mapping, "root_disk_uuid": hw["root_disk_uuid"]}))
    # This private file makes the diagnostic reproducible without changing the
    # live native-adi profile. Logs omit every hardware identifier.
    (runtime / "diagnostic-hardware.plist").write_bytes(plistlib.dumps({"hardware": hw, "synthetic-board-fields": synthetic}))
    sys.path[:0] = [str(runtime / "dependencies"), str(runtime)]
    os.chdir(runtime)
    logging.disable(logging.CRITICAL)
    from emulated import nac
    from unicorn.x86_const import UC_X86_REG_RIP
    # Upstream reads the file on import; enforce the selected local profile.
    nac.FAKE_DATA = {"iokit": mapping, "root_disk_uuid": hw["root_disk_uuid"]}
    binary = runtime / "emulated" / "IMDAppleServices"
    if not binary.exists():
        r = requests.get("https://raw.githubusercontent.com/JJTech0130/nacserver/0a2aa2406542fb09c14a37db17ff14865b8aea29/IMDAppleServices", timeout=45)
        r.raise_for_status()
        if hashlib.sha1(r.content).hexdigest() != nac.BINARY_HASH:
            raise SystemExit("NAC binary hash mismatch")
        binary.write_bytes(r.content)
    j = nac.load_nac()
    emu_start = j.uc.emu_start

    def bounded_start(begin, until, *unused, **kwargs):
        emu_start(begin, until, timeout=20_000_000, count=500_000_000)
        if j.uc.reg_read(UC_X86_REG_RIP) != until:
            print(json.dumps({"stage": "emulation-budget", "instruction": hex(j.uc.reg_read(UC_X86_REG_RIP))}), flush=True)
            raise RuntimeError("NAC emulation did not return within its budget")

    j.uc.emu_start = bounded_start
    if transport:
        cert_path = runtime / "certificate.plist"
        transport_args = fetch_certificate(transport, cert_path)
        cert = plistlib.loads(cert_path.read_bytes())["cert"]
    else:
        cert_response = apple.get("https://static.ess.apple.com/identity/validation/cert-1.0.plist", verify=str(ca_path), timeout=30)
        cert_response.raise_for_status()
        cert = plistlib.loads(cert_response.content)["cert"]
    ctx, request = nac.nac_init(j, cert)
    print(json.dumps({"stage": "NACInit", "request-bytes": len(request), "synthetic-board-fields": synthetic}), flush=True)
    payload = plistlib.dumps({"session-info-request": bytes(request)})
    if transport:
        request_path, response_path = runtime / "initialize-request.plist", runtime / "initialize-response.plist"
        request_path.write_bytes(payload)
        subprocess.run([str(transport), "-op", "initialize", "-in", str(request_path), "-out", str(response_path), *transport_args], check=True, timeout=40)
        fields = plistlib.loads(response_path.read_bytes())
    else:
        response = apple.post("https://identity.ess.apple.com/WebObjects/TDIdentityService.woa/wa/initializeValidation", data=payload, verify=str(ca_path), timeout=30)
        response.raise_for_status()
        fields = plistlib.loads(response.content)
    if fields.get("status", 0) != 0 or not isinstance(fields.get("session-info"), bytes):
        raise SystemExit("Apple NAC session response refused or missing session-info")
    nac.nac_key_establishment(j, ctx, fields["session-info"])
    validation = bytes(nac.nac_sign(j, ctx))
    if not validation:
        raise SystemExit("NACSign returned empty validation")
    with output.open("xb") as f:
        f.write(validation)
    print(json.dumps({"stage": "NACSign", "validation-bytes": len(validation), "iCloud-activation-verified": False}), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        # Native hooks can include hardware in their exception messages.
        print("Mac NAC diagnostic failed: " + type(failure).__name__, file=sys.stderr)
        raise SystemExit(1)
