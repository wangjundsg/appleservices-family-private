//go:build windows

package privatefs

import (
	"golang.org/x/sys/windows"
	"os"
	"strings"
	"testing"
)

func TestNewTemporaryFilesInheritOnlyRuntimePrincipals(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(dir, "secret-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, path := range []string{dir, f.Name()} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		text := sd.String()
		for _, broad := range []string{";;;BU)", ";;;AU)", ";;;WD)"} {
			if strings.Contains(text, broad) {
				t.Fatal("broad access inherited")
			}
		}
		if !strings.Contains(text, ";;;SY)") || !strings.Contains(text, ";;;BA)") {
			t.Fatal("required service principals removed")
		}
	}
	if err := os.WriteFile(f.Name(), []byte("fixture"), 0600); err != nil {
		t.Fatal("runtime user lost write access")
	}
}
