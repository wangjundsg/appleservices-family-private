package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAutomaticMailboxPurchaseIsFrozenBeforePipelineStarts(t *testing.T) {
	for _, mode := range []string{"local", "forge"} {
		t.Run(mode, func(t *testing.T) {
			s := registerTestServer(t)
			s.config.AllowPurchase = true
			t.Setenv("HERO_API_KEY", "fixture")
			if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
				t.Fatal(err)
			}
			credential := filepath.Join(s.config.Dir, "key.dpapi")
			if err := os.WriteFile(credential, []byte("fixture-not-a-real-key"), 0600); err != nil {
				t.Fatal(err)
			}
			settings := registerSettings{Roxy: map[string]any{"token": "fixture", "workspaceId": "fixture", "projectId": "fixture"},
				Purchase: purchasePolicy{"0.2", "0.4", "USD"}, Resources: resourceSettings{CredentialFile: credential}}
			if err := writeRegisterJSON(s.registerConfigFile(), settings); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(s.config.RepoRoot, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"login.exe", "appleweb.exe", "trusted-device.exe"} {
				if err := os.WriteFile(filepath.Join(s.config.RepoRoot, "bin", name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")
			if err := os.MkdirAll(filepath.Dir(script), 0700); err != nil {
				t.Fatal(err)
			}
			code := "import sys,json,pathlib\np=pathlib.Path(sys.argv[3]);c=json.loads(p.read_text())\nassert '--allow-purchase' in sys.argv\nassert c['email']=='' and c['password']\nassert c['mail_purchase']['warehouse']==2 and c['mail_purchase']['max_price_vnd']==700\nassert c['sms']['auto_purchase'] and c['handoff']=='dashboard'\nassert c['otpgmail_order_file']==str(p.parent/'otpgmail-order.json')\nassert not pathlib.Path(c['otpgmail_order_file']).exists()\n"
			if err := os.WriteFile(script, []byte(code), 0600); err != nil {
				t.Fatal(err)
			}
			config := registerTestCall(s, "GET", "/api/register/config", "")
			var view map[string]any
			if json.Unmarshal(config.Body.Bytes(), &view) != nil || view["mailboxConfigured"] != true {
				t.Fatal("unbound mailbox purchase not available")
			}
			body := registerRequest{Automatic: true, BrowserMode: mode, PhoneCC: "PL", AcceptTerms: true, CaptchaProvider: "manual"}
			body.Egress.Mode = "direct"
			id, err := s.startRegistration(body)
			if err != nil {
				t.Fatal(err)
			}
			waitMailboxFixture(t, s, id)
		})
	}
}

func waitMailboxFixture(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code := s.registerView(id)["exitCode"].(*int); code != nil {
			if *code != 0 {
				t.Fatal("mailbox fixture failed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("mailbox fixture did not exit")
}

func TestMailboxResumeCannotReplayRegistrationOrBypassPurchaseSwitch(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "mailbox", Status: "needs_review"})
	manifest := map[string]any{"mail_purchase": map[string]any{"warehouse": 2, "max_price_vnd": 700}, "browser_mode": "local"}
	if err := writeRegisterJSON(filepath.Join(folder, "input.json"), manifest); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/register/" + testRegisterID + "/continue-mailbox"
	if w := registerTestCall(s, "POST", endpoint, "{}"); w.Code != 409 {
		t.Fatal("purchase switch bypassed")
	}
	s.config.AllowPurchase = true
	for _, receipt := range []pipelineReceipt{{Stage: "create", Status: "needs_review"}, {Stage: "mailbox", Active: true}, {Stage: "mailbox", AccountCreated: true}, {Stage: "mailbox", RegistrationStarted: true}} {
		if err := writeRegisterJSON(filepath.Join(folder, "result.json"), receipt); err != nil {
			t.Fatal(err)
		}
		if w := registerTestCall(s, "POST", endpoint, "{}"); w.Code != 409 {
			t.Fatal("unsafe resume accepted")
		}
	}
	if err := writeRegisterJSON(filepath.Join(folder, "result.json"), pipelineReceipt{Stage: "mailbox", Status: "needs_review"}); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")
	if err := os.MkdirAll(filepath.Dir(script), 0700); err != nil {
		t.Fatal(err)
	}
	code := "import sys,pathlib,json\nassert sys.argv[1]=='resume' and sys.argv[4]=='--allow-purchase'\np=pathlib.Path('.cache/panel')/sys.argv[3]/'input.json'\nc=json.loads(p.read_text())\nassert c['browser_mode']=='local' and c['mail_purchase']['max_price_vnd']==700 and c['panel_bridge']['token']\n"
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if w := registerTestCall(s, "POST", endpoint, "{}"); w.Code != 200 {
		t.Fatal("safe resume rejected", w.Body.String())
	}
	if w := registerTestCall(s, "POST", endpoint, "{}"); w.Code != 409 {
		t.Fatal("duplicate resume accepted")
	}
	waitMailboxFixture(t, s, testRegisterID)
	if err := os.WriteFile(filepath.Join(folder, "cancel.request"), []byte("cancel"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := registerTestCall(s, "POST", endpoint, "{}"); w.Code != 409 {
		t.Fatal("cancelled task resumed")
	}
}
