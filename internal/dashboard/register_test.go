package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appleservices "github.com/Laky-64/appleservices"
)

const testRegisterID = "reg-20261114-abcdefabcdefabcd"

func registerTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	s, err := New(Config{Dir: filepath.Join(root, "state"), RepoRoot: root, Python: "python", Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.listener.Close() })
	return s
}

func registerTestCall(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.URL()+path, strings.NewReader(body))
	r.Header.Set("X-App-Token", s.token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func registerFixture(t *testing.T, s *Server, receipt pipelineReceipt) string {
	t.Helper()
	folder := s.registerFolder(testRegisterID)
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	for file, value := range map[string]any{"panel.json": registerMeta{StartedAt: time.Now()}, "result.json": receipt} {
		if err := writeRegisterJSON(filepath.Join(folder, file), value); err != nil {
			t.Fatal(err)
		}
	}
	return folder
}

func TestRegisterAuthValidationAndTraversal(t *testing.T) {
	s := registerTestServer(t)
	r := httptest.NewRequest("POST", s.URL()+"/api/register", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("registration bypassed capability")
	}
	for _, body := range []string{`{}`, `{"platformKey":"must-not-be-accepted"}`, `{"email":"a@example.test","password":"weak"}`} {
		w = registerTestCall(s, "POST", "/api/register", body)
		if w.Code != 400 {
			t.Fatalf("invalid input accepted: %d", w.Code)
		}
	}
	folder := registerFixture(t, s, pipelineReceipt{Stage: "create", Status: "unknown"})
	_ = os.WriteFile(filepath.Join(folder, "input.json"), []byte(`{"password":"secret"}`), 0600)
	for _, path := range []string{"/api/register/../input.json", "/api/register/" + testRegisterID + "/evidence?name=../input.json", "/api/register/" + testRegisterID + "/evidence?name=input.json"} {
		w = registerTestCall(s, "GET", path, "")
		if w.Code != 404 {
			t.Fatalf("private artifact exposed: %s", path)
		}
	}
}

func TestRegisterProgressAndLogsExposeOnlySafeFields(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "login", Status: "unknown"})
	_ = os.WriteFile(filepath.Join(folder, "events.jsonl"), []byte("{\"stage\":\"login\",\"status\":\"unknown\",\"reason\":\"password-secret\",\"code\":\"123456\"}\n{\"stage\":\"token-secret\",\"status\":\"unknown\"}\n"), 0600)
	for _, suffix := range []string{"", "/log?tail=200"} {
		w := registerTestCall(s, "GET", "/api/register/"+testRegisterID+suffix, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "123456") {
			t.Fatal("unsafe progress", w.Body.String())
		}
	}
	view := s.registerView(testRegisterID)
	if view["canRetry"] != false {
		t.Fatal("unknown registration can replay")
	}
}

func TestRegisterCancelIsDurableAndCodeIsNotEchoed(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "create", Status: "waiting_input", Active: true})
	w := registerTestCall(s, "POST", "/api/register/"+testRegisterID+"/input", `{"code":"123456"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "123456") {
		t.Fatal("OTP rejected or leaked")
	}
	if data, _ := os.ReadFile(filepath.Join(folder, "email-code.txt")); string(data) != "123456" {
		t.Fatal("code not delivered")
	}
	w = registerTestCall(s, "POST", "/api/register/"+testRegisterID+"/cancel", `{}`)
	if w.Code != 202 {
		t.Fatal("cancel rejected")
	}
	if _, err := os.Stat(filepath.Join(folder, "cancel.request")); err != nil {
		t.Fatal("cancellation not durable")
	}
	_ = writeRegisterJSON(filepath.Join(folder, "result.json"), pipelineReceipt{Status: "ready"})
	if registerTestCall(s, "POST", "/api/register/"+testRegisterID+"/cancel", `{}`).Code != 409 {
		t.Fatal("completed account can be refunded")
	}
}

func TestRegistrationImportsOnlyVerifiedAccountOnce(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Status: "ready", RegistrationAccepted: true})
	_ = writeRegisterJSON(filepath.Join(folder, "input.json"), map[string]string{"email": "owner@example.test"})
	s.importRegistration(testRegisterID)
	if s.lookup(testRegisterID) != nil {
		t.Fatal("imported without bound sessions")
	}
	session := appleservices.Session{AppleID: "owner@example.test", DSID: "test-gsa", Tokens: map[string]string{"com.apple.gs.idms.pet": "test-pet"}}
	accepted := acceptedICloudCheckpoint{Session: appleservices.Session{AppleID: session.AppleID, DSID: "test-icloud", Tokens: map[string]string{"mmeAuthToken": "test-token"}}, ADSID: session.DSID, ServiceData: map[string]any{}}
	_ = writeRegisterJSON(filepath.Join(folder, "session.json"), session)
	_ = writeRegisterJSON(filepath.Join(folder, "icloud-accepted.json"), accepted)
	s.importRegistration(testRegisterID)
	s.importRegistration(testRegisterID)
	if len(s.accounts) != 1 || s.accounts[0].Dir != folder {
		t.Fatal("import missing or duplicated")
	}
	entries, err := loadRegistry(s.config.Dir, s.config.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("registry not persisted")
	}
	if s.registerView(testRegisterID)["imported"] != true {
		t.Fatal("import not acknowledged")
	}
}

func TestRegisterRunsExistingPipelineWithoutSecretArguments(t *testing.T) {
	if _, err := exec.LookPath("python"); err != nil {
		t.Skip("Python unavailable")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	s := registerTestServer(t)
	t.Setenv("GRIZZLY_API_KEY", "test-platform-secret")
	t.Setenv("YESCAPTCHA_KEY", "test-captcha-secret")
	t.Setenv("APPLESERVICES_REGISTER_CONFIG", filepath.Join(s.config.RepoRoot, "config.json"))
	_ = writeRegisterJSON(os.Getenv("APPLESERVICES_REGISTER_CONFIG"), registerSettings{Roxy: map[string]any{"token": "test-forge-secret", "workspaceId": "test-workspace", "projectId": "test-project"}})
	_ = os.MkdirAll(filepath.Join(s.config.RepoRoot, "bin"), 0700)
	for _, name := range []string{"login.exe", "appleweb.exe", "trusted-device.exe"} {
		_ = os.WriteFile(filepath.Join(s.config.RepoRoot, "bin", name), nil, 0600)
	}
	script := filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")
	_ = os.MkdirAll(filepath.Dir(script), 0700)
	// This fixture cannot call Apple, Forge or a paid provider.
	_ = os.WriteFile(script, []byte("import json,os,sys,pathlib\nassert len(sys.argv)==4\np=pathlib.Path(sys.argv[3])\nc=json.loads(p.read_text())\nassert c['password']=='Fixture-only123!'\nassert pathlib.Path(os.environ['APPLESERVICES_PIPELINE_STATE'])==p.parent.parent\n(p.parent/'result.json').write_text(json.dumps({'stage':'create','status':'failed','creation_rejected':True}))\n(p.parent/'input.json').write_text(json.dumps(c))\nprint('test-platform-secret')\n"), 0600)
	body := registerRequest{Email: "owner@example.test", Password: "Fixture-only123!", Phone: "7700900123", PhoneCC: "GB", Platform: "grizzly", Activation: "test-123", ExpiresAt: time.Now().Add(10 * time.Minute), AcceptTerms: true, CaptchaProvider: "yes"}
	body.Egress.Mode = "direct"
	data, _ := json.Marshal(body)
	w := registerTestCall(s, "POST", "/api/register", string(data))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var answer struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	deadline := time.Now().Add(10 * time.Second)
	for s.registerView(answer.JobID)["exitCode"].(*int) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	view := s.registerView(answer.JobID)
	code := view["exitCode"].(*int)
	if code == nil || *code != 0 || view["canRetry"] != true {
		t.Fatal("stub pipeline did not complete", view)
	}
	public := registerTestCall(s, "GET", "/api/register", "").Body.String()
	for _, secret := range []string{body.Password, "test-platform-secret", "test-forge-secret", body.Email} {
		if strings.Contains(public, secret) {
			t.Fatal("credential exposed")
		}
	}
	if registerTestCall(s, "POST", "/api/register", string(data)).Code != 400 {
		t.Fatal("duplicate registration accepted")
	}
	body.RetryOf = answer.JobID
	body.Activation = "new-activation"
	data, _ = json.Marshal(body)
	if registerTestCall(s, "POST", "/api/register", string(data)).Code != 202 {
		t.Fatal("proven failure retry rejected")
	}
	// Wait for every stub before the temporary workspace is removed.
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "panel.json"))
		finished := true
		for _, file := range files {
			var meta registerMeta
			if readRegisterJSON(file, &meta) != nil || meta.ExitCode == nil {
				finished = false
			}
		}
		if finished {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("retry child did not exit")
}
