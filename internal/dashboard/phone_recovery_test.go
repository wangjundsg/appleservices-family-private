package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPortugalPhoneResourceConfiguration(t *testing.T) {
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRegisterJSON(s.registerConfigFile(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	settings := resourceSettings{MailProvider: "otpgmail", SMSProvider: "hero", CredentialFile: filepath.Join(s.config.Dir, "credential.dpapi"), PhoneCountry: "PT", ProxyProfile: "current"}
	body, _ := json.Marshal(map[string]any{"settings": settings, "purchase": purchasePolicy{"0.2", "0.4", "USD"}})
	response := registerTestCall(s, "POST", "/api/register/resources", string(body))
	if response.Code != 200 {
		t.Fatalf("Portugal resource configuration rejected: %d", response.Code)
	}
	if registrationCountries["PT"][0] != "pt-PT" {
		t.Fatal("Portugal is missing from form countries")
	}
}
