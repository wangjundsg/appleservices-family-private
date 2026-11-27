package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddAccountReturnsIdentityAfterStartupFailure(t *testing.T) {
	s := registerTestServer(t)
	var first struct {
		OK        bool   `json:"ok"`
		AccountID string `json:"account_id"`
	}
	w := registerTestCall(s, "POST", "/api/accounts/add", `{"apple_id":"new@example.test","password":"private-password"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.OK || first.AccountID == "" {
		t.Fatal("partial add did not return saved account ID")
	}
	w = registerTestCall(s, "POST", "/api/accounts/add", `{"apple_id":"new@example.test","password":"private-password"}`)
	var repeated struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.AccountID != first.AccountID || len(s.accounts) != 1 {
		t.Fatal("retry created another identity")
	}
}

func TestLocalStartIgnoresInheritedProxyAndChecksLocalRuntime(t *testing.T) {
	t.Setenv("APPLESERVICES_NATIVE_HTTP_PROXY", "socks5h://private-marker@localhost:1080")
	s := registerTestServer(t)
	s.config.Worker = filepath.Join(t.TempDir(), "unused.exe")
	if err := os.WriteFile(s.config.Worker, []byte("unused"), 0600); err != nil {
		t.Fatal(err)
	}
	s.provisionMacPush = func(context.Context, string) error { t.Fatal("provisioned without local runtime"); return nil }
	result := s.startIn(context.Background(), t.TempDir(), credentials{AppleID: "owner@example.test", Password: "private-password"}, nil)
	if result.Stage != "native-library" || strings.Contains(result.Message, "private-") {
		t.Fatal("local prerequisites were replaced by a proxy/network gate")
	}
}

func TestNativeFailureIsNotReportedAsAccountRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.err.log")
	if err := os.WriteFile(path, []byte("trusted-device: nativeadi: native_tls_handshake_failed private-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	message := lastProcessError(path)
	if !strings.Contains(message, "TLS") || strings.Contains(message, "private-marker") {
		t.Fatal("missing safe network diagnostic")
	}
}

func TestMacPushDiagnosticIsUsefulAndRedacted(t *testing.T) {
	err := macPushDiagnostic([]byte("private-stderr\n" + `{"stage":"Mac APS setup","phase":"connect-APS","failed":"private-marker"}`))
	message := macPushFailureMessage(err)
	if !strings.Contains(message, "连接 Apple 推送服务") || strings.Contains(message, "private") {
		t.Fatal(message)
	}
	err = macPushDiagnostic([]byte(`{"stage":"Mac APS setup","phase":"private-marker"}`))
	if strings.Contains(err.Error()+macPushFailureMessage(err), "private-marker") {
		t.Fatal("untrusted diagnostic escaped")
	}
}
