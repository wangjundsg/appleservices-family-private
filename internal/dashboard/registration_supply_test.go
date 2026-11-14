package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticRequestUsesPrivateManifestAndDoesNotCollideOnEmptyActivation(t *testing.T) {
	if _, err := exec.LookPath("python"); err != nil {
		t.Skip("Python unavailable")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	s := registerTestServer(t)
	s.config.AllowPurchase = true
	t.Setenv("SMS_API_KEY", "fixture")
	t.Setenv("YESCAPTCHA_KEY", "fixture")
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"address": "new@hotmail.com", "reader": "graph", "status": "ok", "authorization_status": "ready"}})
	}))
	defer pool.Close()
	client := filepath.Join(s.config.RepoRoot, "client.json")
	_ = writeRegisterJSON(client, map[string]string{"baseUrl": pool.URL, "apiToken": "fixture"})
	settings := registerSettings{Roxy: map[string]any{"token": "fixture", "workspaceId": "fixture", "projectId": "fixture"}, CodePoolConfig: client, Purchase: purchasePolicy{"1.10", "2.20", "USD"}}
	config := filepath.Join(s.config.RepoRoot, "config.json")
	t.Setenv("APPLESERVICES_REGISTER_CONFIG", config)
	_ = writeRegisterJSON(config, settings)
	_ = os.MkdirAll(filepath.Join(s.config.RepoRoot, "bin"), 0700)
	for _, name := range []string{"login.exe", "appleweb.exe", "trusted-device.exe"} {
		_ = os.WriteFile(filepath.Join(s.config.RepoRoot, "bin", name), nil, 0600)
	}
	folder := registerFixture(t, s, pipelineReceipt{Status: "failed"})
	_ = writeRegisterJSON(filepath.Join(folder, "request.json"), map[string]any{"email": "prior@hotmail.com", "sms": map[string]any{"auto_purchase": true}})
	script := filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")
	_ = os.MkdirAll(filepath.Dir(script), 0700)
	_ = os.WriteFile(script, []byte("import json,sys,pathlib\np=pathlib.Path(sys.argv[3])\nc=json.loads(p.read_text())\nassert sys.argv[4]=='--allow-purchase'\nassert c['handoff']=='dashboard' and c['sms']['auto_purchase']\nassert c['code_pool_config'] and 'email_code_file' not in c\nassert c['sms']['max_price']=='1.10'\n"), 0600)
	body := registerRequest{Automatic: true, PhoneCC: "GB", AcceptTerms: true, CaptchaProvider: "yes"}
	body.Egress.Mode = "direct"
	id, err := s.startRegistration(body)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code := s.registerView(id)["exitCode"].(*int)
		if code != nil {
			if *code != 0 {
				t.Fatal("automatic manifest rejected")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fixture did not exit")
}

func TestAutomaticPurchaseRequiresExplicitSwitchAndBudget(t *testing.T) {
	s := registerTestServer(t)
	file := filepath.Join(s.config.RepoRoot, "config.json")
	t.Setenv("APPLESERVICES_REGISTER_CONFIG", file)
	_ = writeRegisterJSON(file, registerSettings{Roxy: map[string]any{"token": "fixture", "workspaceId": "fixture", "projectId": "fixture"}})
	body := registerRequest{Automatic: true, PhoneCC: "GB", AcceptTerms: true, CaptchaProvider: "yes"}
	body.Egress.Mode = "direct"
	if _, err := s.startRegistration(body); err == nil || !strings.Contains(err.Error(), "--allow-purchase") {
		t.Fatal("paid switch was not required", err)
	}
	s.config.AllowPurchase = true
	if _, err := s.startRegistration(body); err == nil || !strings.Contains(err.Error(), "预算") {
		t.Fatal("missing price was accepted", err)
	}
	for _, policy := range []purchasePolicy{{"", "", "USD"}, {"NaN", "2", "USD"}, {"1.10", "1.09", "USD"}, {"1", "2", "RUB"}} {
		if policy.valid() {
			t.Fatal("invalid policy accepted", policy)
		}
	}
	if !(purchasePolicy{"1.10", "2.20", "USD"}).valid() {
		t.Fatal("fixture policy rejected")
	}
}

func TestMailboxSelectionChecksAuthorizationAndLocalReservation(t *testing.T) {
	s := registerTestServer(t)
	t.Setenv("SMS_API_KEY", "fixture-only")
	requests := 0
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/api/v1/mailboxes" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("wrong Outlook contract")
		}
		json.NewEncoder(w).Encode([]map[string]string{
			{"address": "blocked@hotmail.com", "reader": "graph", "status": "ok", "authorization_status": "invalid"},
			{"address": "unsupported@outlook.com", "reader": "graph", "status": "ok", "authorization_status": "ready"},
			{"address": "used@hotmail.com", "reader": "graph", "status": "ok", "authorization_status": "ready"},
			{"address": "ready@hotmail.com", "reader": "graph", "status": "ok", "authorization_status": "ready"},
		})
	}))
	defer pool.Close()
	client := filepath.Join(s.config.RepoRoot, "client.json")
	_ = writeRegisterJSON(client, map[string]string{"baseUrl": pool.URL, "apiToken": "fixture"})
	folder := registerFixture(t, s, pipelineReceipt{Status: "failed"})
	_ = writeRegisterJSON(filepath.Join(folder, "request.json"), map[string]string{"email": "USED@hotmail.com"})
	body := registerRequest{}
	if err := s.prepareAutomaticRegistration(&body, registerSettings{CodePoolConfig: client}); err != nil {
		t.Fatal(err)
	}
	if body.Email != "ready@hotmail.com" || !strongRegistrationPassword(body.Password) || requests != 1 {
		t.Fatal("wrong selection or password")
	}
	_ = writeRegisterJSON(filepath.Join(folder, "request.json"), map[string]string{"email": "ready@hotmail.com"})
	s.accounts = append(s.accounts, &Account{AppleID: "used@hotmail.com"})
	if err := s.prepareAutomaticRegistration(&body, registerSettings{CodePoolConfig: client}); err == nil {
		t.Fatal("reused occupied mailbox")
	}
}

func TestHandoffAttachIsDurableAndUnknownCannotReplay(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "login", Status: "running"})
	_ = writeRegisterJSON(filepath.Join(folder, "input.json"), handoffInput{Email: "owner@example.test", Password: "Fixture-only123!", Handoff: "dashboard"})
	endpoint := "/api/register/" + testRegisterID + "/handoff"
	first := registerTestCall(s, "POST", endpoint, `{"action":"attach"}`)
	if !strings.Contains(first.Body.String(), `"ok":true`) || len(s.accounts) != 1 {
		t.Fatal(first.Body.String())
	}
	second := registerTestCall(s, "POST", endpoint, `{"action":"attach"}`)
	if first.Body.String() != second.Body.String() || len(s.accounts) != 1 {
		t.Fatal("attach was replayed")
	}
	if !s.registrationInProgress(s.accounts[0]) {
		t.Fatal("owner lost before activation")
	}
	file := filepath.Join(folder, "handoff-actions.json")
	var receipts map[string]handoffReceipt
	_ = readRegisterJSON(file, &receipts)
	receipts["start"] = handoffReceipt{State: "intent"}
	_ = writeRegisterJSON(file, receipts)
	unknown := registerTestCall(s, "POST", endpoint, `{"action":"start"}`)
	if !strings.Contains(unknown.Body.String(), `"state":"unknown"`) {
		t.Fatal("unknown start was replayed")
	}
	if strings.Contains(unknown.Body.String(), "Fixture-only") {
		t.Fatal("password exposed")
	}
	_ = writeRegisterJSON(filepath.Join(folder, "result.json"), pipelineReceipt{Status: "ready", HandoffComplete: true})
	if s.registrationInProgress(s.accounts[0]) {
		t.Fatal("ownership not released after completion")
	}
	if _, err := os.Stat(filepath.Join(s.accounts[0].Dir, "device.json")); err != nil {
		t.Fatal("identity not created")
	}
}

func TestHandoffDeadlineAndConfigDoNotExposeSecrets(t *testing.T) {
	s := registerTestServer(t)
	registerFixture(t, s, pipelineReceipt{Active: true, Stage: "terms", Status: "running", HeartbeatAt: time.Now().Add(-time.Minute), HeartbeatDeadline: time.Now().Add(time.Minute)})
	if s.registerView(testRegisterID)["active"] != true {
		t.Fatal("long activation marked dead")
	}
	value := registerTestCall(s, "GET", "/api/register/config", "")
	if !strings.Contains(value.Body.String(), `"purchaseEnabled":false`) || strings.Contains(value.Body.String(), s.token) {
		t.Fatal("unsafe config", value.Body.String())
	}
}
