//go:build windows

package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows user protects this handoff credential; no plaintext password or
// portable decryption key is written beside account state.
func saveBridgeCredential(dir string, credentials BridgeCredentials) error {
	if credentials.AppleID == "" || credentials.Password == "" {
		return errors.New("bridge credential missing")
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	if err = windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	encrypted := unsafe.Slice(output.Data, int(output.Size))
	file, err := os.CreateTemp(dir, ".bridge-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(encrypted); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	// MoveFileEx replaces atomically on Windows, including an existing vault.
	from, _ := windows.UTF16PtrFromString(name)
	to, _ := windows.UTF16PtrFromString(filepath.Join(dir, "bridge-credential.dpapi"))
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func loadBridgeCredential(dir, appleID string) (BridgeCredentials, error) {
	data, err := os.ReadFile(filepath.Join(dir, "bridge-credential.dpapi"))
	if err != nil {
		return BridgeCredentials{}, err
	}
	if len(data) == 0 {
		return BridgeCredentials{}, errors.New("empty bridge vault")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	if err = windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return BridgeCredentials{}, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	var result BridgeCredentials
	if err = json.Unmarshal(unsafe.Slice(output.Data, int(output.Size)), &result); err != nil {
		return BridgeCredentials{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(result.AppleID), strings.TrimSpace(appleID)) || result.Password == "" {
		return BridgeCredentials{}, errors.New("bridge identity mismatch")
	}
	return result, nil
}
