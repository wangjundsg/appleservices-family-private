package gsa

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVerificationHeaderTokensCompleteSession(t *testing.T) {
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	c := NewClient(headerProvider{})
	c.loginPayload = map[string]any{"adsid": "test-account", "t": map[string]any{}}
	c.collectVerifiedTokens(map[string][]string{
		"X-Apple-PE-Token": {encode("test-account:test-pet")},
		"X-Apple-HB-Token": {encode("com.apple.gs.idms.hb:test-heartbeat")},
		"X-Apple-GS-Token": {encode("com.apple.gs.service1:test-token1:300"), encode("com.apple.gs.service2:test-token2:300")},
	})
	result := c.VerifiedLoginResult()
	if result == nil || result.NeedsTwoFactor || result.SessionPayload["adsid"] != "test-account" {
		t.Fatal("verified header response did not complete the same account session")
	}
	tokens := result.SessionPayload["t"].(map[string]any)
	if len(tokens) != 4 || tokens["com.apple.gs.idms.pet"].(map[string]any)["token"] != "test-pet" {
		t.Fatal("lost PET or repeated service header values")
	}
	if len(c.loginPayload["t"].(map[string]any)) != 0 {
		t.Fatal("modified original challenge payload")
	}
}

func TestSMSDiscovery201DoesNotSendAnotherCode(t *testing.T) {
	c := NewClient(headerProvider{})
	requests := 0
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
		}
		requests++
		if r.Method != "GET" || r.URL.Path != "/auth" {
			t.Error("sent a second SMS after automatic delivery")
		}
		return &http.Response{StatusCode: 201, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"trustedPhoneNumbers":[{"id":7}]}`))}, nil
	})
	if err := c.RequestSMSCode("test-account", "test-idms"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || c.smsPhoneID != 7 {
		t.Fatal("lost already-delivered SMS phone identifier")
	}
}

func TestTrustedValidationRetainsHeaderPET(t *testing.T) {
	c := NewClient(headerProvider{})
	c.loginPayload = map[string]any{"adsid": "test-account"}
	c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := plistResponse(t, map[string]any{"ec": int64(0)})
		response.Header.Set("X-Apple-PE-Token", base64.StdEncoding.EncodeToString([]byte("test-account:test-pet")))
		return response, nil
	})
	if err := c.SubmitTrustedDeviceCode("test-account", "test-idms", "123456"); err != nil {
		t.Fatal(err)
	}
	if c.VerifiedLoginResult() == nil {
		t.Fatal("discarded PET issued after code validation")
	}
}
