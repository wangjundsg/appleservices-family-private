package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandoffMissingRuntimeIsDurableAndCanRetry(t *testing.T) {
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
		if !strings.Contains(response.Body.String(), `"state":"not_started"`) || !strings.Contains(response.Body.String(), `"stage":"native-library"`) {
			t.Fatal(response.Body.String())
		}
		var receipts map[string]handoffReceipt
		_ = readRegisterJSON(filepath.Join(folder, "handoff-actions.json"), &receipts)
		if receipts["start"].State != "not_started" || receipts["start"].Result["stage"] != "native-library" {
			t.Fatal("pre-login failure was lost")
		}
	}
	if _, err := os.Stat(filepath.Join(s.accounts[0].Dir, "daemon.out.log")); !os.IsNotExist(err) {
		t.Fatal("worker was launched")
	}
}

func TestFailedActivationArchivesPrivateEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := archiveActivationAttempt(dir); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "activation-before-restart-*", "session.json"))
	if len(paths) != 1 {
		t.Fatal("missing recovery evidence")
	}
	data, err := os.ReadFile(paths[0])
	if err != nil || string(data) != "private-fixture" {
		t.Fatal("evidence changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.json")); err != nil {
		t.Fatal("original was removed")
	}
}

func TestSavedActivationEndpointDoesNotReuseFailedSession(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "terms", Status: "needs_review", AccountCreated: true})
	_ = writeRegisterJSON(filepath.Join(folder, "input.json"), handoffInput{Email: "owner@example.test", Handoff: "dashboard"})
	w := registerTestCall(s, "POST", "/api/register/"+testRegisterID+"/handoff", `{"action":"activate-saved"}`)
	if !strings.Contains(w.Body.String(), "activation-fresh-login-required") {
		t.Fatal(w.Body.String())
	}
}
