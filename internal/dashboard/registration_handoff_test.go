package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandoffPreflightFailureIsDurableAndCanRetry(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "login", Status: "running"})
	_ = writeRegisterJSON(filepath.Join(folder, "input.json"), handoffInput{Email: "owner@example.test", Password: "Fixture-only123!", Handoff: "dashboard"})
	s.config.Worker = filepath.Join(t.TempDir(), "unused.exe")
	_ = os.WriteFile(s.config.Worker, []byte("unused"), 0600)
	t.Setenv("APPLESERVICES_NATIVE_HTTP_PROXY", "invalid-proxy")
	endpoint := "/api/register/" + testRegisterID + "/handoff"
	registerTestCall(s, "POST", endpoint, `{"action":"attach"}`)
	for i := 0; i < 2; i++ {
		response := registerTestCall(s, "POST", endpoint, `{"action":"start"}`)
		if !strings.Contains(response.Body.String(), `"state":"not_started"`) || !strings.Contains(response.Body.String(), `"stage":"native-proxy"`) {
			t.Fatal(response.Body.String())
		}
		var receipts map[string]handoffReceipt
		_ = readRegisterJSON(filepath.Join(folder, "handoff-actions.json"), &receipts)
		if receipts["start"].State != "not_started" || receipts["start"].Result["stage"] != "native-proxy" {
			t.Fatal("pre-login failure was lost")
		}
	}
	if _, err := os.Stat(filepath.Join(s.accounts[0].Dir, "daemon.out.log")); !os.IsNotExist(err) {
		t.Fatal("worker was launched")
	}
}
