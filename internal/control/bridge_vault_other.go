//go:build !windows

package control

import "errors"

func saveBridgeCredential(string, BridgeCredentials) error {
	return errors.New("persistent bridge credential requires Windows")
}
func loadBridgeCredential(string, string) (BridgeCredentials, error) {
	return BridgeCredentials{}, errors.New("persistent bridge credential requires Windows")
}
