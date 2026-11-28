"""Headless ADI bridge for the verified OpenBubbles Windows 1.18.800.0 module.

Only local ADI provisioning, authentication headers and six-digit generation are
used. Account passwords/tokens are not input. The unused foreign APS transport
is fenced; real push is owned by this project's Windows COM backend.
"""
import base64
import ctypes as C
import json
import os
import pathlib
import plistlib
import struct
import sys
import uuid
import msvcrt

# Native provisioning contains direct stdout prints. Silence both CRT FDs and
# Win32 handles before loading any native code, and reserve a private RPC writer.
rpc_output=os.fdopen(os.dup(sys.stdout.fileno()),'w',encoding='utf-8',buffering=1)
null_fd=os.open(os.devnull,os.O_RDWR)
os.dup2(null_fd,sys.stdout.fileno())
os.dup2(null_fd,sys.stderr.fileno())
kernel=C.WinDLL('kernel32')
kernel.SetStdHandle.argtypes=[C.c_uint32,C.c_void_p]
kernel.SetStdHandle.restype=C.c_int
kernel.SetStdHandle(C.c_uint32(-11),msvcrt.get_osfhandle(null_fd))
kernel.SetStdHandle(C.c_uint32(-12),msvcrt.get_osfhandle(null_fd))

import bridge
from bridge import Bridge, NativeFailure, encode_string, local_failure


def send(response):
    rpc_output.write(json.dumps(response,separators=(',',':'))+'\n')
    rpc_output.flush()


def varint(number):
    result=bytearray()
    while number>127:
        result.append((number&127)|128)
        number>>=7
    result.append(number)
    return bytes(result)


def field(number,value):
    if isinstance(value,str): value=value.encode()
    return varint(number<<3|2)+varint(len(value))+value


class Reader:
    def __init__(self,data): self.data=data; self.position=0
    def take(self,length):
        if length<0 or self.position+length>len(self.data): raise RuntimeError('truncated native response')
        data=self.data[self.position:self.position+length]; self.position+=length
        return data
    def integer(self): return struct.unpack('<i',self.take(4))[0]
    def string(self):
        length=self.integer()
        if length>8192: raise RuntimeError('oversize native header')
        return self.take(length).decode('utf-8')
    def finish(self):
        if self.position!=len(self.data): raise RuntimeError('unexpected trailing native response')


class Provider:
    def __init__(self,request):
        identifier=base64.b64decode(request['identifier'],validate=True)
        token=base64.b64decode(request['push_token'],validate=True)
        if len(identifier)!=16 or len(token)!=32: raise RuntimeError('invalid native identity')
        self.device_id=str(uuid.UUID(bytes=identifier))
        self.profile=pathlib.Path(request['profile']).resolve()
        self.profile.mkdir(parents=True,exist_ok=True)
        self.bridge=b=Bridge(request['library'])
        self.fence=C.CDLL(str(pathlib.Path(request['fence']).resolve(strict=True)))
        self.fence.InstallTransportFence.argtypes=[C.c_void_p]
        self.fence.InstallTransportFence.restype=C.c_int
        if self.fence.InstallTransportFence(b.library._handle)!=2: raise RuntimeError('private transport isolation failed')
        hardware_path=self.profile/'hw_info.plist'
        if not hardware_path.exists():
            mac_address=base64.b64decode(request['mac_address'],validate=True)
            if len(mac_address)!=6 or mac_address[0]&3!=2: raise RuntimeError('invalid synthetic network identity')
            inner=b''.join([field(1,'MacBookPro15,1'),field(2,mac_address),
                           field(3,request['serial']),field(4,self.device_id),field(5,self.device_id),
                           field(6,'Mac-937A206F2EE63C01'),field(7,'22C65')])
            encoded=b''.join([field(1,inner),field(2,'13.1'),varint(3<<3)+varint(1660),
                              field(4,self.device_id),field(5,'com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0'),field(6,'282')])
            config=b.handle(b.call(51,struct.pack('<i',len(encoded))+encoded),'JoinedOSConfig')
            identity=b.handle(b.sync(150),'IDSNGMIdentity')
            b.call(198,encode_string(str(self.profile))+config.pack()+identity.pack())
        hardware=plistlib.loads(hardware_path.read_bytes())
        if hardware['os_config']['inner']['platform_serial_number']!=request['serial']:
            raise RuntimeError('native serial mismatch')
        # Keep account-isolated Mac APS credentials when a profile has them.
        # Updating its bound token must not discard its certificate/private key.
        hardware.setdefault('push',{})['token']=token
        hardware_path.write_bytes(plistlib.dumps(hardware))
        result=b.sync(166,encode_string(str(self.profile)))
        if not result or result[0]!=1: raise RuntimeError('native profile is invalid')
        hardware_handle=b.handle(result[1:],'SavedHardwareState')
        self.config=b.handle(b.sync(18,hardware_handle.pack()),'JoinedOSConfig')
        identity=b.handle(b.sync(17,hardware_handle.pack()),'IDSNGMIdentity')
        push=b.handle(b.sync(19,hardware_handle.pack()),'APSState')
        result=b.call(201,self.config.pack()+identity.pack()+b'\x01'+push.pack(owned=True)+encode_string(str(self.profile)))
        if len(result)<13 or result[12]!=1: raise RuntimeError('unused foreign push transport unexpectedly opened')
        connection=b.handle(result[:12],'APSConnection')
        self.anisette=b.handle(b.call(130,encode_string(str(self.profile))+self.config.pack()+connection.pack()),'ArcAnisetteClientDefaultAnisetteProvider')
        directory=self.profile/'anisette_test'
        directory.mkdir(exist_ok=True)
        state_path=directory/'state.plist'
        if not state_path.exists(): state_path.write_bytes(plistlib.dumps({'keychain_identifier':identifier}))
        state=plistlib.loads(state_path.read_bytes())
        if state.get('keychain_identifier')!=identifier: raise RuntimeError('native provisioning identity mismatch')
        self.code()  # Real generation preflight; the result is not printed.

    def code(self):
        result=self.bridge.call(103,self.anisette.pack())
        if len(result)!=4: raise RuntimeError('invalid native code response')
        value=struct.unpack('<I',result)[0]
        if value>999999: raise RuntimeError('native code out of range')
        return f'{value:06d}'

    def headers(self):
        data=self.bridge.call(106,self.anisette.pack()+self.config.pack())
        reader=Reader(data)
        count=reader.integer()
        if count<1 or count>64: raise RuntimeError('invalid native header count')
        headers={}
        for _ in range(count):
            key=reader.string(); value=reader.string(); headers[key]=value
        reader.finish()
        return headers


def main():
    if C.sizeof(C.c_void_p)!=8 or sys.version_info<(3,10):
        raise RuntimeError('64-bit Python 3.10 or newer is required')
    request=json.loads(sys.stdin.readline(65537))
    provider=Provider(request)
    send({'id':0,'device_id':provider.device_id,'ready':True})
    for line in sys.stdin:
        request=json.loads(line)
        if request['op']=='headers':
            send({'id':request['id'],'device_id':provider.device_id,'headers':provider.headers()})
        elif request['op']=='code':
            send({'id':request['id'],'device_id':provider.device_id,'code':provider.code()})
        else:
            raise RuntimeError('unknown native operation')


if __name__=='__main__':
    try: main()
    except NativeFailure as failure:
        send({'error':failure.code, 'function':failure.function})
        raise SystemExit(1)
    except Exception as failure:
        send({'error':local_failure(failure), 'function':bridge.last_function})
        raise SystemExit(1)
