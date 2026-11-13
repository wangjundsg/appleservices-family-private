// Command adicode-stub is a placeholder local ADI verification-code provider.
//
// `trusted-device -check` refuses to start without a provider executable
// (aps/code_provider.go), yet the device-registration and iCloud-terms paths do
// not generate codes at all. This stub satisfies the contract so those paths can
// run: it echoes the device id and returns a fixed code.
//
// It is NOT a code generator. Any flow that actually needs a valid six-digit
// code still fails, because Apple rejects a fabricated one.
package main

import (
	"encoding/json"
	"os"
)

func main() {
	var request struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.NewDecoder(os.Stdin).Decode(&request)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
		"device_id": request.DeviceID,
		"code":      "000000",
	})
}
