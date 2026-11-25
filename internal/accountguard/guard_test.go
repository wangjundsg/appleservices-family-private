package accountguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuardFailsClosedAndOnlyExplicitReleaseClearsIt(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if Check(dir) != nil {
		t.Fatal("new account blocked")
	}
	for _, data := range []string{`broken`, `{"version":2}`, `{"version":1,"blocked":false}`, `{"version":1,"blocked":true,"reason":"apple_account_locked"}`} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if Check(dir) == nil || Check(other) != nil {
			t.Fatal("guard missing or crossed account boundary")
		}
	}
	if Release(dir, false) == nil || Check(dir) == nil {
		t.Fatal("release lacked explicit confirmation")
	}
	// A successful session write never clears the independent guard.
	os.WriteFile(filepath.Join(dir, "runtime.json"), []byte(`{"state":"online","authenticated":true}`), 0600)
	if Check(dir) == nil {
		t.Fatal("cache cleared protection")
	}
	if err := Release(dir, true); err != nil {
		t.Fatal(err)
	}
	if Check(dir) != nil {
		t.Fatal("explicit release failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "control.json")); !os.IsNotExist(err) {
		t.Fatal("release started worker")
	}
}
