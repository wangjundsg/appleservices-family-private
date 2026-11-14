package appleweb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestICloudValidationRequiresCompleteAccountOnHTTP200(t *testing.T) {
	for _, tc := range []struct {
		name, response, code string
	}{
		{"empty", `{}`, "icloud_validate_incomplete"},
		{"rejected", `{"success":false,"dsInfo":{"dsid":"fixture-dsid"}}`, "icloud_validate_incomplete"},
		{"challenge", `{"hsaChallengeRequired":true,"dsInfo":{"dsid":"fixture-dsid"}}`, "icloud_validate_incomplete"},
		{"terms", `{"termsUpdateNeeded":true,"dsInfo":{"dsid":"fixture-dsid"}}`, "icloud_terms_update_needed"},
		{"repair", `{"isRepairNeeded":true,"dsInfo":{"dsid":"fixture-dsid"}}`, "icloud_account_repair_needed"},
		{"complete", `{"dsInfo":{"dsid":"fixture-dsid","appleId":"fixture-apple-id"},"webservices":{"mail":{"url":"https://mail.icloud.com"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &ICloudSessionValidator{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response)), Request: req}, nil
			})}}
			result, err := client.Validate(context.Background(), nil, "www.icloud.com")
			code, _, _ := ErrorDetails(err)
			if code != tc.code {
				t.Fatalf("expected %q, got %v", tc.code, err)
			}
			if tc.code == "" && (result.DSID != "fixture-dsid" || result.AppleID != "fixture-apple-id" || result.MailBaseURL != "https://mail.icloud.com") {
				t.Fatalf("complete account lost validated fields: %+v", result)
			}
		})
	}
}

func TestICloudValidationRejects421WithoutExposingTokens(t *testing.T) {
	client := &ICloudSessionValidator{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "setup.icloud.com.cn" || req.URL.Path != "/setup/ws/1/validate" {
			t.Fatal("validation used an unexpected endpoint")
		}
		cookie, err := req.Cookie("X-APPLE-WEBAUTH-TOKEN")
		if err != nil || cookie.Value != "fixture-cookie-secret" {
			t.Fatal("validation did not carry its domain's session cookie")
		}
		return &http.Response{StatusCode: http.StatusMisdirectedRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"success":false,"trustTokens":["fixture-trust-secret"],"requestInfo":{"accountName":"fixture-account-secret"}}`)), Request: req}, nil
	})}}
	_, err := client.Validate(context.Background(), []SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-cookie-secret", Domain: ".icloud.com.cn", Path: "/"}}, "www.icloud.com.cn")
	if err == nil || !strings.Contains(err.Error(), "HTTP 421") {
		t.Fatal("validation must reject HTTP 421")
	}
	for _, secret := range []string{"fixture-trust-secret", "fixture-cookie-secret", "fixture-account-secret", "trustTokens"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("validation exposed an upstream credential")
		}
	}
}

func TestICloudValidationPreservesExplicitDomainRedirect(t *testing.T) {
	client := &ICloudSessionValidator{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"domainToUse":"iCloud.com"}`)), Request: req}, nil
	})}}
	_, err := client.Validate(context.Background(), nil, "www.icloud.com.cn")
	var redirect appleDomainRedirectError
	if !errors.As(err, &redirect) || redirect.Host != "www.icloud.com" {
		t.Fatal("validation lost Apple's explicit domain redirect")
	}
}

func TestICloudValidationDiagnosticIsLimitedToFlags(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://setup.icloud.com/setup/ws/1/validate?clientId=fixture-client-secret", nil)
	req.AddCookie(&http.Cookie{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "fixture-cookie-secret"})
	resp := &http.Response{StatusCode: http.StatusMisdirectedRequest, Header: http.Header{}}
	data := []byte(`{"success":false,"trustTokens":["fixture-trust-secret"],"hsaChallengeRequired":true,"requestInfo":{"accountName":"fixture-account-secret"}}`)
	diagnostic := iCloudValidationDiagnostic(req, resp, data)
	for _, expected := range []string{"status=421", "webauth_token=true", "success=false", "challenge_required=true", "trust_tokens_present=true"} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic is missing %s", expected)
		}
	}
	for _, secret := range []string{"fixture-cookie-secret", "fixture-trust-secret", "fixture-account-secret", "fixture-client-secret"} {
		if strings.Contains(diagnostic, secret) {
			t.Fatal("validation diagnostic exposed a credential")
		}
	}
}
