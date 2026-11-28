#define WIN32_LEAN_AND_MEAN
#include <winsock2.h>
#include <windows.h>
#include <mswsock.h>
#include <ws2tcpip.h>
#include <string.h>

// The foreign API asks for an APS context even when generating only ADI data.
// Our real APS connection is handled by Apple's native COM daemon. Reject this
// helper's unused port 5223 connections before activation/authentication. This
// changes only this module's IAT in the current helper process, not system rules.
static LPFN_CONNECTEX original_connectex;
typedef int (WSAAPI *IoctlFn)(SOCKET,DWORD,LPVOID,DWORD,LPVOID,DWORD,LPDWORD,LPWSAOVERLAPPED,LPWSAOVERLAPPED_COMPLETION_ROUTINE);
typedef int (WSAAPI *ConnectFn)(SOCKET,const struct sockaddr*,int);
static IoctlFn original_ioctl;
static ConnectFn original_connect;
typedef int (WSAAPI *GetAddrInfoFn)(PCSTR,PCSTR,const ADDRINFOA*,PADDRINFOA*);
static GetAddrInfoFn original_getaddrinfo;

static int WSAAPI ipv4_getaddrinfo(PCSTR node, PCSTR service, const ADDRINFOA* hints, PADDRINFOA* result) {
    ADDRINFOA selected;
    if (hints) selected = *hints;
    else memset(&selected, 0, sizeof(selected));
    selected.ai_family = AF_INET;
    return original_getaddrinfo(node, service, &selected, result);
}

// Only the verified ADI module's DNS import changes. No system DNS, TLS, or
// other process is modified, and addresses continue to come from system DNS.
__declspec(dllexport) int InstallIPv4Resolver(HMODULE module) {
    unsigned char* base = (unsigned char*)module;
    if (!base || ((IMAGE_DOS_HEADER*)base)->e_magic != IMAGE_DOS_SIGNATURE) return -1;
    IMAGE_NT_HEADERS64* nt = (IMAGE_NT_HEADERS64*)(base + ((IMAGE_DOS_HEADER*)base)->e_lfanew);
    if (nt->Signature != IMAGE_NT_SIGNATURE || nt->FileHeader.Machine != IMAGE_FILE_MACHINE_AMD64) return -2;
    IMAGE_IMPORT_DESCRIPTOR* table = (IMAGE_IMPORT_DESCRIPTOR*)(base + nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_IMPORT].VirtualAddress);
    for (; table->Name; table++) {
        if (_stricmp((char*)(base+table->Name), "ws2_32.dll")) continue;
        IMAGE_THUNK_DATA64* names = (IMAGE_THUNK_DATA64*)(base+table->OriginalFirstThunk);
        IMAGE_THUNK_DATA64* targets = (IMAGE_THUNK_DATA64*)(base+table->FirstThunk);
        for (; names->u1.AddressOfData; names++,targets++) {
            if (IMAGE_SNAP_BY_ORDINAL64(names->u1.Ordinal)) continue;
            const char* name = (char*)((IMAGE_IMPORT_BY_NAME*)(base+names->u1.AddressOfData))->Name;
            if (strcmp(name, "getaddrinfo")) continue;
            DWORD previous;
            if (!VirtualProtect(&targets->u1.Function,sizeof(void*),PAGE_READWRITE,&previous)) return -3;
            original_getaddrinfo = (GetAddrInfoFn)targets->u1.Function;
            targets->u1.Function = (ULONGLONG)ipv4_getaddrinfo;
            DWORD ignored;
            VirtualProtect(&targets->u1.Function,sizeof(void*),previous,&ignored);
            return 1;
        }
    }
    return 0;
}

static int blocked(const struct sockaddr* address, int size) {
    if (!address || size < 4) return 0;
    if (address->sa_family != AF_INET && address->sa_family != AF_INET6) return 0;
    const unsigned char* bytes = (const unsigned char*)address;
    return bytes[2] == (5223 >> 8) && bytes[3] == (5223 & 255);
}
static BOOL PASCAL local_connectex(SOCKET socket,const struct sockaddr* address,int size,
                                  PVOID buffer,DWORD count,LPDWORD sent,LPOVERLAPPED overlapped) {
    if (blocked(address,size)) { WSASetLastError(WSAECONNREFUSED); return FALSE; }
    return original_connectex(socket,address,size,buffer,count,sent,overlapped);
}
static int WSAAPI local_connect(SOCKET socket,const struct sockaddr* address,int size) {
    if (blocked(address,size)) { WSASetLastError(WSAECONNREFUSED); return SOCKET_ERROR; }
    return original_connect(socket,address,size);
}
static int WSAAPI local_ioctl(SOCKET socket,DWORD code,LPVOID input,DWORD input_size,
                             LPVOID output,DWORD output_size,LPDWORD returned,
                             LPWSAOVERLAPPED overlapped,LPWSAOVERLAPPED_COMPLETION_ROUTINE completion) {
    int result = original_ioctl(socket,code,input,input_size,output,output_size,returned,overlapped,completion);
    GUID connectex = WSAID_CONNECTEX;
    if (result == 0 && code == SIO_GET_EXTENSION_FUNCTION_POINTER && input_size == sizeof(GUID)
        && input && output && output_size >= sizeof(void*) && !memcmp(input,&connectex,sizeof(GUID))) {
        original_connectex = *(LPFN_CONNECTEX*)output;
        *(LPFN_CONNECTEX*)output = local_connectex;
    }
    return result;
}
__declspec(dllexport) int InstallTransportFence(HMODULE module) {
    unsigned char* base = (unsigned char*)module;
    IMAGE_DOS_HEADER* dos = (IMAGE_DOS_HEADER*)base;
    if (!base || dos->e_magic != IMAGE_DOS_SIGNATURE) return -1;
    IMAGE_NT_HEADERS64* nt = (IMAGE_NT_HEADERS64*)(base + dos->e_lfanew);
    if (nt->Signature != IMAGE_NT_SIGNATURE || nt->FileHeader.Machine != IMAGE_FILE_MACHINE_AMD64) return -2;
    IMAGE_IMPORT_DESCRIPTOR* table = (IMAGE_IMPORT_DESCRIPTOR*)(base + nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_IMPORT].VirtualAddress);
    int count = 0;
    for (; table->Name; table++) {
        if (_stricmp((char*)(base+table->Name),"ws2_32.dll")) continue;
        IMAGE_THUNK_DATA64* names = (IMAGE_THUNK_DATA64*)(base+table->OriginalFirstThunk);
        IMAGE_THUNK_DATA64* targets = (IMAGE_THUNK_DATA64*)(base+table->FirstThunk);
        for (; names->u1.AddressOfData; names++,targets++) {
            if (IMAGE_SNAP_BY_ORDINAL64(names->u1.Ordinal)) continue;
            const char* name = (char*)((IMAGE_IMPORT_BY_NAME*)(base+names->u1.AddressOfData))->Name;
            void* replacement = NULL;
            if (!strcmp(name,"WSAIoctl")) { original_ioctl=(IoctlFn)targets->u1.Function; replacement=local_ioctl; }
            if (!strcmp(name,"connect")) { original_connect=(ConnectFn)targets->u1.Function; replacement=local_connect; }
            if (!replacement) continue;
            DWORD previous;
            if (!VirtualProtect(&targets->u1.Function,sizeof(void*),PAGE_READWRITE,&previous)) return -3;
            targets->u1.Function=(ULONGLONG)replacement;
            DWORD ignored;
            VirtualProtect(&targets->u1.Function,sizeof(void*),previous,&ignored);
            count++;
        }
    }
    return count;
}
