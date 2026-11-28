import ctypes as C
import pathlib
import struct
import queue
import hashlib

last_function = 0

class NativeFailure(RuntimeError):
    def __init__(self, code, function=0):
        self.code = code
        self.function = function
        super().__init__(code)

def local_failure(error):
    # Fixed labels only: exception messages can include paths or device material.
    if isinstance(error, queue.Empty):
        return 'native_bridge_timeout'
    if isinstance(error, OSError):
        return 'native_local_io_error'
    if isinstance(error, (ValueError, KeyError, struct.error)):
        return 'native_local_value_error'
    return 'native_local_operation_failed'

def native_failure(result):
    # Classify in memory; never expose the native payload or its URLs/metadata.
    raw = result.lower()
    if b'handshake' in raw and (b'eof' in raw or b'tls' in raw):
        return NativeFailure('native_tls_handshake_failed')
    if b'timed out' in raw or b'timeout' in raw:
        return NativeFailure('native_network_timeout')
    if b'connection refused' in raw or b'dns error' in raw:
        return NativeFailure('native_connection_failed')
    return NativeFailure('native_operation_failed')

class SyncResult(C.Structure):
    _fields_ = [('ptr', C.c_void_p), ('length', C.c_int32)]
class ExternalData(C.Structure):
    _fields_ = [('kind', C.c_int32), ('length', C.c_ssize_t), ('data', C.c_void_p),
                ('peer', C.c_void_p), ('callback', C.c_void_p)]
class TypedData(C.Structure):
    _fields_ = [('kind', C.c_int32), ('length', C.c_ssize_t), ('data', C.c_void_p)]
class ObjectValue(C.Union):
    _fields_ = [('external', ExternalData), ('typed', TypedData), ('align', C.c_uint64*5)]
class Object(C.Structure):
    _fields_ = [('kind', C.c_int32), ('value', ObjectValue)]

def encode_string(value):
    raw=value.encode('utf-8')
    return struct.pack('<i',len(raw))+raw

class Bridge:
    def __init__(self,library_path):
        path=pathlib.Path(library_path).resolve(strict=True)
        if hashlib.sha256(path.read_bytes()).hexdigest()!='ca88e816dda449d7ba24d54a5ed709a2d392daeae28c25a61dcd2e7496e44cfe':
            raise RuntimeError('native module hash does not match the verified Store release')
        self.library=C.CDLL(str(path))
        self.library.frb_get_rust_content_hash.restype=C.c_int32
        if self.library.frb_get_rust_content_hash()!=-208244658:
            raise RuntimeError('native binding mismatch')
        self.library.rust_vec_u8_new.argtypes=[C.c_int32]
        self.library.rust_vec_u8_new.restype=C.c_void_p
        self.library.frb_pde_ffi_dispatcher_sync.argtypes=[C.c_int32,C.c_void_p,C.c_int32,C.c_int32]
        self.library.frb_pde_ffi_dispatcher_sync.restype=SyncResult
        self.library.free_wire_sync_rust2dart_sse.argtypes=[SyncResult]
        self.library.free_wire_sync_rust2dart_sse.restype=None
        self.library.frb_pde_ffi_dispatcher_primary.argtypes=[C.c_int32,C.c_int64,C.c_void_p,C.c_int32,C.c_int32]
        self.library.frb_pde_ffi_dispatcher_primary.restype=None
        self.responses=queue.Queue()
        def post(port, obj_ptr):
            obj=obj_ptr.contents
            if obj.kind==8:
                data=obj.value.external
                self.responses.put((port,C.string_at(data.data,data.length)))
                C.CFUNCTYPE(None,C.c_void_p,C.c_void_p)(data.callback)(None,data.peer)
                return True
            if obj.kind==7:
                data=obj.value.typed
                self.responses.put((port,C.string_at(data.data,data.length)))
                return True
            return False
        self.callback=C.CFUNCTYPE(C.c_bool,C.c_int64,C.POINTER(Object))(post)
        self.library.store_dart_post_cobject.argtypes=[C.c_void_p]
        self.library.store_dart_post_cobject(self.callback)
    def prepare(self,data):
        ptr=self.library.rust_vec_u8_new(len(data))
        if data: C.memmove(ptr,data,len(data))
        return ptr
    def unwrap(self,result):
        if not result or result[0]!=0:
            failure = native_failure(result or b'')
            failure.function = getattr(self, 'function', 0)
            raise failure
        return result[1:]
    def sync(self,func,data=b''):
        global last_function
        self.function = last_function = func
        result=self.library.frb_pde_ffi_dispatcher_sync(func,self.prepare(data),len(data),len(data))
        try: raw=C.string_at(result.ptr,result.length)
        finally: self.library.free_wire_sync_rust2dart_sse(result)
        return self.unwrap(raw)
    def call(self,func,data=b''):
        global last_function
        self.function = last_function = func
        self.library.frb_pde_ffi_dispatcher_primary(func,1,self.prepare(data),len(data),len(data))
        port,raw=self.responses.get(timeout=60)
        return self.unwrap(raw)
    def handle(self,result,kind):
        if len(result)!=12: raise RuntimeError('native handle has invalid length')
        return Handle(self,struct.unpack('<Qi',result),kind)

class Handle:
    def __init__(self,bridge,value,kind): self.bridge,self.value,self.kind=bridge,value,kind
    def pack(self,owned=False):
        if owned: return struct.pack('<Q',self.value[0])
        symbol='frbgen_bluebubbles_rust_arc_increment_strong_count_RustOpaque_flutter_rust_bridgefor_generatedRustAutoOpaqueInner'+self.kind
        increment=getattr(self.bridge.library,symbol)
        increment.argtypes=[C.c_void_p]
        increment.restype=None
        increment(self.value[0])
        return struct.pack('<Q',self.value[0])
