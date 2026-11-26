package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceAPIRequiresTokenAndReturnsNoCredentials(t *testing.T) {
	s := registerTestServer(t)
	file := filepath.Join(s.config.Dir, "registration-config.json")
	_ = os.MkdirAll(s.config.Dir, 0700)
	_ = writeRegisterJSON(file, map[string]any{"roxy": map[string]any{"token": "private-marker", "workspaceId": "fixture", "projectId": "fixture", "proxyInfo": map[string]any{"host": "gw.dataimpulse.com", "port": 10005, "proxyUserName": "private-marker__cr.{country}", "proxyPassword": "private-marker"}}})
	w := registerTestCall(s, "GET", "/api/register/resources", "")
	if w.Code != 200 {
		t.Fatalf("resource endpoint: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "private-marker") {
		t.Fatal("credential exposed")
	}
	var view map[string]any
	if json.Unmarshal(w.Body.Bytes(), &view) != nil || view["mailProvider"] != "otpgmail" {
		t.Fatal("new registration must default to OTPGmail")
	}
	request := httptest.NewRequest("GET", s.URL()+"/api/register/resources", nil)
	unauthorized := httptest.NewRecorder()
	s.ServeHTTP(unauthorized, request)
	if unauthorized.Code != 403 {
		t.Fatal("resource endpoint requires token")
	}
}

func TestHeroOnlyDoesNotRequireGrizzlyKey(t *testing.T) {
	s := registerTestServer(t)
	t.Setenv("SMS_API_KEY", "")
	t.Setenv("SMS_API_KEY_HERO", "fixture-only")
	var settings registerSettings
	_ = json.Unmarshal([]byte(`{"resources":{"mailProvider":"otpgmail","smsProvider":"hero"}}`), &settings)
	err := s.prepareAutomaticRegistration(&registerRequest{}, settings)
	if err == nil || strings.Contains(err.Error(), "SMS_API_KEY") {
		t.Fatalf("Hero-only must reach order validation without Grizzly: %v", err)
	}
}

func TestResourceSavePreservesPrivateConfiguration(t *testing.T) {
	s := registerTestServer(t)
	_ = os.MkdirAll(s.config.Dir, 0700)
	private := map[string]any{"roxy": map[string]string{"token": "private-marker", "workspaceId": "fixture", "projectId": "fixture"}, "otherOwner": map[string]string{"value": "retained"}}
	_ = writeRegisterJSON(s.registerConfigFile(), private)
	settings := resourceSettings{MailProvider: "otpgmail", SMSProvider: "hero", CredentialFile: filepath.Join(s.config.Dir, "credential.dpapi"), PhoneCountry: "PL", DurationHours: 24, ProxyProfile: "current"}
	body, _ := json.Marshal(map[string]any{"settings": settings, "purchase": purchasePolicy{"0.2", "0.4", "USD"}})
	w := registerTestCall(s, "POST", "/api/register/resources", string(body))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-marker") {
		t.Fatal("unsafe save response", w.Code)
	}
	var after map[string]json.RawMessage
	if readRegisterJSON(s.registerConfigFile(), &after) != nil || !strings.Contains(string(after["roxy"]), "private-marker") || !strings.Contains(string(after["otherOwner"]), "retained") {
		t.Fatal("unrelated settings lost")
	}
	settings.PhoneCountry = ""
	body, _ = json.Marshal(map[string]any{"settings": settings, "purchase": purchasePolicy{"0.2", "0.4", "USD"}})
	if got := registerTestCall(s, "POST", "/api/register/resources", string(body)); got.Code != 400 {
		t.Fatal("invalid country saved")
	}
	_ = os.WriteFile(s.registerConfigFile(), []byte("broken"), 0600)
	settings.PhoneCountry = "PL"
	body, _ = json.Marshal(map[string]any{"settings": settings, "purchase": purchasePolicy{"0.2", "0.4", "USD"}})
	if got := registerTestCall(s, "POST", "/api/register/resources", string(body)); got.Code != 409 {
		t.Fatal("corrupt config overwritten")
	}
}

type resourceTransport func(*http.Request) (*http.Response, error)

func (f resourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHeroChecksAreReadOnlyAndSchemasFailClosed(t *testing.T) {
	var actions []string
	client := &http.Client{Transport: resourceTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Host != "hero-sms.com" {
			t.Fatal("unexpected provider request")
		}
		action := r.URL.Query().Get("action")
		actions = append(actions, action)
		bodies := map[string]string{"getBalance": "ACCESS_BALANCE:3.2", "getActiveActivations": `{"status":"success","data":[],"activeActivations":{"rows":[]}}`, "serviceCountRent": `{"15":{"24":{"count":25,"price":0.08,"retail_price":0.104}}}`}
		body, ok := bodies[action]
		if !ok {
			t.Fatal("non-readonly action", action)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	result, err := checkHero(context.Background(), "fixture", "PL", 24, client)
	if err != nil || result["activeOrders"] != 0 || len(actions) != 3 {
		t.Fatal("read-only Hero contract failed", err)
	}
	quote := result["quote"].(map[string]any)
	if quote["price"] != 0.104 || quote["currency"] != "USD" || quote["priceCapGuaranteed"] != false {
		t.Fatal("rental quote misrepresented")
	}
	for _, data := range []string{`{}`, `{"status":"error","data":[]}`, `{"status":"success","data":null}`, `{"status":"success","data":[],"activeActivations":[{}]}`} {
		if _, err := heroActivationCount([]byte(data)); err == nil {
			t.Fatal("invalid activation response accepted")
		}
	}
	if _, err := heroQuote([]byte(`{"15":{"24":{"price":0.08,"count":25}}}`), "15", 24); err == nil {
		t.Fatal("wholesale price used as customer price")
	}
}

func TestResourceProxyUsesAccountRegionAndNeverReturnsCredentials(t *testing.T) {
	proxy := map[string]any{"host": "gw.dataimpulse.com", "port": 10005, "proxyUserName": "fixture__cr.{country}", "proxyPassword": "private-marker"}
	settings := registerSettings{Roxy: map[string]any{"proxyInfo": proxy}, Resources: resourceSettings{PhoneCountry: "PL"}}
	p, err := resourceProxy(settings, "US", 0)
	if err != nil || p["proxyUserName"] != "fixture__cr.us" || proxy["proxyUserName"] != "fixture__cr.{country}" {
		t.Fatal("region expansion or original mutation")
	}
	settings.ProxyProfiles = map[string]map[string]any{"711": {"host": "gateway.example.test", "port": 10000, "proxyUserName": "fixture-zone-custom-region-{country}-session-12345678", "proxyPassword": "private-marker"}}
	settings.Resources.ProxyProfile = "711"
	p, err = resourceProxy(settings, "US", 0)
	if err != nil || !strings.Contains(p["proxyUserName"].(string), "region-US") {
		t.Fatal("711 region casing")
	}
	settings.Resources.ProxyProfile = "missing"
	if _, err = resourceProxy(settings, "US", 0); err == nil {
		t.Fatal("missing profile silently fell back")
	}
}

func TestOTPGmailHeroManifestFreezesBindings(t *testing.T) {
	for _, name := range []string{"python", "node"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip("runtime unavailable")
		}
	}
	s := registerTestServer(t)
	s.config.AllowPurchase = true
	t.Setenv("SMS_API_KEY", "")
	t.Setenv("SMS_API_KEY_HERO", "")
	t.Setenv("HERO_API_KEY", "fixture")
	t.Setenv("YESCAPTCHA_KEY", "fixture")
	_ = os.MkdirAll(s.config.Dir, 0700)
	credential := filepath.Join(s.config.Dir, "key.dpapi")
	_ = os.WriteFile(credential, []byte("fixture"), 0600)
	orderFile := filepath.Join(s.config.Dir, "order.json")
	order := otpGmailOrder{Provider: "otpgmail", OrderID: "fixture-order", Email: "new@gmail.com", Warehouse: 2, CredentialFile: credential}
	_ = writeRegisterJSON(orderFile, order)
	settings := registerSettings{Roxy: map[string]any{"token": "fixture", "workspaceId": "fixture", "projectId": "fixture", "proxyInfo": map[string]any{"host": "gw.dataimpulse.com", "port": 10005, "proxyUserName": "fixture__cr.{country}", "proxyPassword": "fixture"}}, Purchase: purchasePolicy{"0.2", "0.4", "USD"}, Resources: resourceSettings{MailProvider: "otpgmail", CredentialFile: credential, OrderFile: orderFile, SMSProvider: "hero", PhoneCountry: "PL", DurationHours: 24, AcceptRentalPriceChange: true}}
	_ = writeRegisterJSON(s.registerConfigFile(), settings)
	prior := resourceProbe
	resourceProbe = `function runCLI(){process.stdout.write('{"orderReady":true,"orderStatus":"waiting_code"}')}`
	t.Cleanup(func() { resourceProbe = prior })
	_ = os.MkdirAll(filepath.Join(s.config.RepoRoot, "bin"), 0700)
	for _, name := range []string{"login.exe", "appleweb.exe", "trusted-device.exe"} {
		_ = os.WriteFile(filepath.Join(s.config.RepoRoot, "bin", name), nil, 0600)
	}
	script := filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")
	_ = os.MkdirAll(filepath.Dir(script), 0700)
	_ = os.WriteFile(script, []byte("import json,sys,pathlib,os\nc=json.loads(pathlib.Path(sys.argv[3]).read_text())\nassert c['sms']['providers']==['hero'] and c['sms']['duration_hours']==24\nassert c['sms']['accept_rental_price_change'] and os.environ['SMS_API_KEY_HERO']=='fixture'\nassert 'code_pool_config' not in c and 'email_code_file' not in c\nassert c['account_country']=='US' and c['roxy']['proxyInfo']['proxyUserName']=='fixture__cr.us'\nassert json.loads(pathlib.Path(c['otpgmail_order_file']).read_text())['orderId']=='fixture-order'\n"), 0600)
	body := registerRequest{Automatic: true, PhoneCC: "PL", AcceptTerms: true, CaptchaProvider: "yes", RegistrationMethod: methodProtocol}
	body.Egress.Mode = "roxy"
	body.Egress.ProxyPort = 10005
	id, err := s.startRegistration(body)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if readRegisterJSON(filepath.Join(s.registerFolder(id), "request.json"), &manifest) != nil || manifest["protocol"].(map[string]any)["auto_codes"] != true {
		t.Fatal("automatic protocol reader not enabled")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code := s.registerView(id)["exitCode"].(*int); code != nil {
			if *code != 0 {
				t.Fatal("manifest rejected by fixture")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := s.registerView(id)["exitCode"].(*int); code == nil {
		t.Fatal("fixture did not exit")
	}
	if _, err = s.startRegistration(body); err == nil || !strings.Contains(err.Error(), "已有任务") {
		t.Fatal("occupied order reused", err)
	}
}
