import ctypes
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(os.name=='nt' and shutil.which('gcc'), 'Windows MinGW fixture')
class IPv4ResolverTests(unittest.TestCase):
    def test_module_dns_is_ipv4_without_changing_system_resolver(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            fixture=root/'fixture.c'
            fixture.write_text('''
#define WIN32_LEAN_AND_MEAN
#include <winsock2.h>
#include <ws2tcpip.h>
#include <string.h>
__declspec(dllexport) int Families(void) {
 WSADATA w; WSAStartup(MAKEWORD(2,2),&w);
 ADDRINFOA hints; memset(&hints,0,sizeof(hints)); hints.ai_family=AF_UNSPEC;
 ADDRINFOA* list=0; if(getaddrinfo("localhost","443",&hints,&list)) return -1;
 int mask=0; for(ADDRINFOA* i=list;i;i=i->ai_next) mask|=i->ai_family==AF_INET?1:2;
 freeaddrinfo(list); return mask;
}
''')
            outputs=[]
            for source,name in [(fixture,'fixture'),(Path(__file__).parent/'assets/transport_fence.c','fence')]:
                out=root/(name+'.dll')
                subprocess.run(['gcc','-shared','-O2',str(source),'-o',str(out),'-lws2_32'],check=True,capture_output=True)
                outputs.append(out)
            # Load in a child so Windows releases the DLLs before temp cleanup.
            code='''
import ctypes as c, socket, sys
before=socket.getaddrinfo('localhost',443)
fixture=c.CDLL(sys.argv[1]); fence=c.CDLL(sys.argv[2])
fence.InstallIPv4Resolver.argtypes=[c.c_void_p]
assert fence.InstallIPv4Resolver(fixture._handle)==1
assert fixture.Families()==1
assert socket.getaddrinfo('localhost',443)==before
'''
            import sys
            subprocess.run([sys.executable,'-c',code,*map(str,outputs)],check=True,capture_output=True)


if __name__=='__main__': unittest.main()
