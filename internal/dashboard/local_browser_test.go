package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalBrowserSelectionIndependentOfForge(t *testing.T) {
	body := registerRequest{BrowserMode: "local", CaptchaProvider: "manual"}
	body.Egress.Mode = "local"
	if err := validateBrowserSelection(body); err != nil {
		t.Fatal(err)
	}
	body.CaptchaProvider = "yes"
	if err := validateBrowserSelection(body); err != nil {
		t.Fatal("remote OCR rejected", err)
	}
	body.CaptchaProvider = "local-first"
	if validateBrowserSelection(body) == nil {
		t.Fatal("disabled local OCR still accepted")
	}
	body.CaptchaProvider = "manual"
	body.BrowserMode = "forge"
	if validateBrowserSelection(body) == nil {
		t.Fatal("Forge accepted local routing")
	}
	body.Egress.Mode = "roxy"
	if err := validateBrowserSelection(body); err != nil {
		t.Fatal(err)
	}
	body.BrowserMode = "local"
	if validateBrowserSelection(body) == nil {
		t.Fatal("local accepted Forge routing")
	}
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRegisterJSON(s.registerConfigFile(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerSettings(); err != nil {
		t.Fatal("local settings still require Forge", err)
	}
}

func TestPageLocaleIsExplicitAndLocalOnly(t *testing.T) {
	for _, locale := range []string{"", "en-US", "zh-CN", "invalid"} {
		body := registerRequest{BrowserMode: "local", PageLocale: locale, CaptchaProvider: "manual"}
		body.Egress.Mode = "local"
		if (validateBrowserSelection(body) == nil) != (locale != "invalid") {
			t.Fatalf("unexpected locale validation: %s", locale)
		}
		body.BrowserMode, body.Egress.Mode = "forge", "roxy"
		if locale != "" && validateBrowserSelection(body) == nil {
			t.Fatal("local page language changed Forge configuration")
		}
	}
}

func TestLocalBrowserProxyRejectsCredentialsAndExternalHosts(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:7897", "socks5://[::1]:1080", "https://localhost:443"} {
		if !validLocalBrowserProxy(value) {
			t.Fatal("valid proxy rejected", value)
		}
	}
	for _, value := range []string{"", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user:secret@127.0.0.1:7897", "http://external.test:7897", "http://127.0.0.1:7897/path", "file:///tmp/a"} {
		if validLocalBrowserProxy(value) {
			t.Fatal("invalid proxy accepted")
		}
	}
}

func TestTaskBrowserModeReadbackUsesFrozenManifest(t *testing.T) {
	s := registerTestServer(t)
	dir := registerFixture(t, s, pipelineReceipt{})
	if s.registerView(testRegisterID)["browserMode"] != "forge" {
		t.Fatal("legacy task changed mode")
	}
	if err := writeRegisterJSON(filepath.Join(dir, "request.json"), map[string]string{"browser_mode": "local"}); err != nil {
		t.Fatal(err)
	}
	if s.registerView(testRegisterID)["browserMode"] != "local" {
		t.Fatal("task lost frozen browser mode")
	}
}

func TestLocalTaskLaunchUsesExistingPipelineAndFreezesMode(t *testing.T) {
	for _, provider := range []string{"manual", "yes", "local-first"} {
		t.Run(provider, func(t *testing.T) {
			expected := provider
			if expected == "local-first" {
				expected = "yes"
			}
			if _, err := exec.LookPath("python"); err != nil {
				t.Skip("Python unavailable")
			}
			s := registerTestServer(t)
			t.Setenv("GRIZZLY_API_KEY", "fixture-sms-key")
			t.Setenv("YESCAPTCHA_KEY", "")
			t.Setenv("APPLESERVICES_REGISTER_CONFIG", filepath.Join(s.config.RepoRoot, "config.json"))
			if err := writeRegisterJSON(s.registerConfigFile(), registerSettings{Resources: resourceSettings{LocalBrowserProxy: "http://127.0.0.1:7897"}}); err != nil {
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
			if err := os.WriteFile(script, []byte("import sys,pathlib,json\np=pathlib.Path(sys.argv[3])\nc=json.loads(p.read_text())\nassert c['browser_mode']=='local' and c['roxy']=={} and c['handoff']=='dashboard' and c['page_locale']=='en-US' and c['captcha_solver']=='"+expected+"'\n(p.parent/'result.json').write_text(json.dumps({'stage':'create','status':'failed'}))\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body := registerRequest{BrowserMode: "local", PageLocale: "en-US", Email: "owner@example.test", Password: "Fixture-only123!", Phone: "7700900123", PhoneCC: "GB", Platform: "grizzly", Activation: "fixture-number", ExpiresAt: time.Now().Add(time.Minute * 10), AcceptTerms: true, CaptchaProvider: "manual"}
			body.Egress.Mode = "local"
			body.CaptchaProvider = provider
			data, _ := json.Marshal(body)
			if expected == "yes" {
				if denied := registerTestCall(s, "POST", "/api/register", string(data)); denied.Code != 400 {
					t.Fatal("missing remote key did not fail before starting")
				}
				t.Setenv("YESCAPTCHA_KEY", "fixture-remote-key")
			}
			w := registerTestCall(s, "POST", "/api/register", string(data))
			if w.Code != 202 {
				t.Fatal(w.Body.String())
			}
			var reply struct {
				JobID string `json:"jobId"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := readRegisterJSON(filepath.Join(s.registerFolder(reply.JobID), "request.json"), &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest["local_browser_proxy"] != "http://127.0.0.1:7897" || manifest["browser_mode"] != "local" || manifest["captcha_solver"] != expected {
				t.Fatal("browser configuration not frozen")
			}
			deadline := time.Now().Add(time.Second * 10)
			for time.Now().Before(deadline) {
				if code := s.registerView(reply.JobID)["exitCode"].(*int); code != nil {
					if *code != 0 {
						t.Fatal("existing pipeline fixture failed")
					}
					return
				}
				time.Sleep(time.Millisecond * 20)
			}
			t.Fatal("pipeline did not exit")
		})
	}
}
