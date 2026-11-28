"""Provision an account-isolated diagnostic Mac APS state through the pinned module.

Uses no Apple account credentials, terms, or SMS. The supplied profile must be
an explicitly selected private diagnostic copy. Native messages are silenced;
only status and lengths are printed. Apple writes certificate/token state into
that private profile on a successful APS connection.
"""
import argparse
import base64
import hashlib
import ctypes as C
import json
import msvcrt
import os
from pathlib import Path
import plistlib
import struct
import sys
import uuid
import subprocess
import tempfile


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--profile", type=Path, required=True)
    ap.add_argument("--library", type=Path, required=True)
    ap.add_argument("--go-state-out", type=Path, help="new private APS state for the Go courier client")
    ap.add_argument("--device", type=Path, help="new private diagnostic device binding when the profile has no hardware")
    args = ap.parse_args()
    profile = args.profile.resolve()
    if args.go_state_out and args.go_state_out.exists():
        raise SystemExit("Go APS output must be a new private path")
    output = os.fdopen(os.dup(sys.stdout.fileno()), "w", encoding="utf-8", buffering=1)
    null_fd = os.open(os.devnull, os.O_RDWR)
    os.dup2(null_fd, sys.stdout.fileno())
    os.dup2(null_fd, sys.stderr.fileno())
    kernel = C.WinDLL("kernel32")
    kernel.SetStdHandle.argtypes = [C.c_uint32, C.c_void_p]
    kernel.SetStdHandle.restype = C.c_int
    kernel.SetStdHandle(C.c_uint32(-11), msvcrt.get_osfhandle(null_fd))
    kernel.SetStdHandle(C.c_uint32(-12), msvcrt.get_osfhandle(null_fd))
    phase = "initialize"
    try:
        sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "nativeadi"))
        from bridge import Bridge, encode_string
        b = Bridge(str(args.library.resolve(strict=True)))
        if os.environ.get('APPLESERVICES_ACTIVATION_IPV4') == '1':
            profile.mkdir(mode=0o700, parents=True, exist_ok=True)
            network_dir=Path(tempfile.mkdtemp(prefix='ipv4-',dir=profile))
            resolver_path=network_dir/'resolver.dll'
            subprocess.run(['gcc','-shared','-O2',str(Path(__file__).resolve().parents[1]/'nativeadi/assets/transport_fence.c'),'-o',str(resolver_path),'-lws2_32'],
                           check=True,timeout=30,capture_output=True,creationflags=subprocess.CREATE_NO_WINDOW)
            resolver=C.CDLL(str(resolver_path))
            resolver.InstallIPv4Resolver.argtypes=[C.c_void_p]
            resolver.InstallIPv4Resolver.restype=C.c_int
            if resolver.InstallIPv4Resolver(b.library._handle)!=1: raise RuntimeError('private IPv4 resolver unavailable')
        initial_hardware = plistlib.loads((profile / "hw_info.plist").read_bytes()) if (profile / "hw_info.plist").exists() else None
        if initial_hardware is not None and args.device:
            binding = json.loads(args.device.read_text(encoding="utf-8-sig"))
            bound_id = uuid.UUID(bytes=base64.b64decode(binding["Identifier"], validate=True))
            config = initial_hardware["os_config"]
            if uuid.UUID(config["device_id"]) != bound_id or config["inner"]["platform_serial_number"] != binding["SerialNumber"]:
                raise RuntimeError("existing requester hardware binding mismatch")
        generated_handles = None
        if initial_hardware is None or (args.device and not initial_hardware.get("push", {}).get("token") and not initial_hardware.get("push", {}).get("keypair")):
            if not args.device:
                raise RuntimeError("new diagnostic hardware requires a device binding")
            device = json.loads(args.device.read_text(encoding="utf-8-sig"))
            identifier = base64.b64decode(device["Identifier"], validate=True)
            if len(identifier) != 16 or not device.get("SerialNumber"):
                raise RuntimeError("invalid diagnostic device binding")
            device_id = str(uuid.UUID(bytes=identifier))
            address = bytearray(hashlib.sha256(b"appleservices-mac-network-v1\x00" + identifier).digest()[:6])
            address[0] = (address[0] | 2) & 0xfe
            def varint(number):
                encoded = bytearray()
                while number > 127:
                    encoded.append((number & 127) | 128)
                    number >>= 7
                encoded.append(number)
                return bytes(encoded)
            def field(number, value):
                raw = value.encode() if isinstance(value, str) else bytes(value)
                return varint(number << 3 | 2) + varint(len(raw)) + raw
            inner = b"".join([field(1, "MacBookPro15,1"), field(2, address), field(3, device["SerialNumber"]),
                              field(4, device_id), field(5, device_id), field(6, "Mac-937A206F2EE63C01"), field(7, "22C65")])
            encoded = b"".join([field(1, inner), field(2, "13.1"), varint(3 << 3) + varint(1660), field(4, device_id),
                                field(5, "com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0"), field(6, "282")])
            config = b.handle(b.call(51, struct.pack("<i", len(encoded)) + encoded), "JoinedOSConfig")
            identity = b.handle(b.sync(150), "IDSNGMIdentity")
            profile.mkdir(mode=0o700, parents=True, exist_ok=True)
            b.call(198, encode_string(str(profile)) + config.pack() + identity.pack())
            hw = plistlib.loads((profile / "hw_info.plist").read_bytes())
            hw["appleservices_diagnostic"] = {"synthetic_board_fields": True, "temporary_requester": True}
            (profile / "hw_info.plist").write_bytes(plistlib.dumps(hw))
            generated_handles = (config, identity)
        hw = plistlib.loads((profile / "hw_info.plist").read_bytes())
        if not hw.get("appleservices_diagnostic", {}).get("synthetic_board_fields"):
            raise RuntimeError("only explicit diagnostic profiles are supported")
        if generated_handles:
            # The native serializer omits the optional token before activation;
            # its reader cannot load that token-less state. Keep the live handles
            # until APS setup writes a real token instead of inserting a fake one.
            config, identity = generated_handles
        else:
            phase = "read-hardware"
            result = b.sync(166, encode_string(str(profile)))
            if not result or result[0] != 1:
                raise RuntimeError("bound hardware unavailable")
            hardware = b.handle(result[1:], "SavedHardwareState")
            phase = "read-config-and-identity"
            config = b.handle(b.sync(18, hardware.pack()), "JoinedOSConfig")
            identity = b.handle(b.sync(17, hardware.pack()), "IDSNGMIdentity")
        # Reuse a certificate already issued for this profile across restarts.
        # A token without a keypair is the legacy Windows daemon input: do not
        # import it into a new Mac certificate's connection.
        if hw.get("push", {}).get("keypair"):
            state = b.handle(b.sync(19, hardware.pack()), "APSState")
            state_arg = b"\x01" + state.pack(owned=True)
        else:
            state_arg = b"\x00"
        phase = "connect-APS"
        result = b.call(201, config.pack() + identity.pack() + state_arg + encode_string(str(profile)))
        if len(result) < 13 or result[12] != 0:
            output.write(json.dumps({"stage": "Mac APS setup", "phase": phase, "connected": False}) + "\n")
            return 1
        phase = "save-APS"
        fresh = plistlib.loads((profile / "hw_info.plist").read_bytes())
        push = fresh.get("push", {})
        token = push.get("token") or b""
        # The native serializer omits application diagnostics; retain the marker.
        fresh["appleservices_diagnostic"] = hw["appleservices_diagnostic"]
        (profile / "hw_info.plist").write_bytes(plistlib.dumps(fresh))
        if args.go_state_out:
            from cryptography import x509
            from cryptography.hazmat.primitives import serialization
            keypair = push["keypair"]
            key = serialization.load_der_private_key(keypair["private"], password=None)
            cert = x509.load_der_x509_certificate(keypair["cert"])
            if key.public_key().public_numbers() != cert.public_key().public_numbers():
                raise RuntimeError("Mac push key/certificate mismatch")
            state = {"CertificatePEM": cert.public_bytes(serialization.Encoding.PEM),
                     "PrivateKeyPEM": key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.TraditionalOpenSSL, serialization.NoEncryption()),
                     "Token": token}
            with args.go_state_out.open("x", encoding="utf-8") as f:
                json.dump({k: base64.b64encode(v).decode("ascii") for k, v in state.items()}, f)
        output.write(json.dumps({"stage": "Mac APS setup", "connected": True,
                                 "token_bytes": len(token), "keypair_present": bool(push.get("keypair")),
                                 "token_unchanged_from_input": token == hw.get("push", {}).get("token"),
                                 "go_state_written": bool(args.go_state_out)}) + "\n")
        return 0 if len(token) == 32 and push.get("keypair") else 1
    except Exception as failure:
        output.write(json.dumps({"stage": "Mac APS setup", "phase": phase, "failed": type(failure).__name__}) + "\n")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
