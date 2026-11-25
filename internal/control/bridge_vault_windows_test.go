//go:build windows

package control

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBridgeCredentialSurvivesRestartAndRejectsAnotherIdentity(t *testing.T) {
	dir := t.TempDir()
	first := &Server{dir: dir}
	first.SetBridgeCredentials("fixture@example.test", "fixture-secret")
	raw, err := os.ReadFile(filepath.Join(dir, "bridge-credential.dpapi"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("fixture-secret")) {
		t.Fatal("plaintext credential persisted")
	}
	restarted := &Server{dir: dir}
	restarted.SetBridgeCredentials("fixture@example.test", "")
	if restarted.bridgeCredentials.Password != "fixture-secret" {
		t.Fatal("restart lost handoff")
	}
	restarted.SetBridgeCredentials("other@example.test", "")
	if restarted.bridgeCredentials.Password != "" {
		t.Fatal("cross-account credential loaded")
	}
	first.SetBridgeCredentials("fixture@example.test", "updated-fixture")
	restarted.SetBridgeCredentials("fixture@example.test", "")
	if restarted.bridgeCredentials.Password != "updated-fixture" {
		t.Fatal("credential update lost")
	}
}
