package gsa

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"howett.net/plist"
)

type headerProvider map[string]string

func (p headerProvider) Headers() (map[string]string, error) { return p, nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func plistResponse(t *testing.T, v map[string]any) *http.Response {
	t.Helper()
	body, err := plist.Marshal(v, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
}

func TestGrandSlamNormalizesWireHeaderAndCPD(t *testing.T) {
	oldInfo := "<MacBookPro13,2> <macOS;13.1;22C65> <com.apple.AuthKit/1 (com.apple.dt.Xcode/3594.4.19)>"
	expected := "<MacBookPro13,2> <macOS;13.1;22C65> <com.apple.AuthKit/1 (com.apple.akd/1.0)>"
	p := headerProvider{"x-mme-client-info": oldInfo, "User-Agent": "test-akd", "X-Apple-I-MD": "test-md"}
	c := NewClient(p)
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Mme-Client-Info") != expected || r.Header.Get("User-Agent") != "test-akd" {
			t.Error("GSA wire headers do not carry the compatible provider identity")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]any
		if _, err := plist.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		request := envelope["Request"].(map[string]any)
		cpd := request["cpd"].(map[string]any)
		if cpd["X-Mme-Client-Info"] != expected || cpd["X-Apple-I-MD"] != "test-md" || cpd["User-Agent"] != nil {
			t.Error("GSA CPD differs from compatible identity or contains HTTP-only User-Agent")
		}
		return plistResponse(t, map[string]any{"Response": map[string]any{"Status": map[string]any{"ec": int64(0)}}}), nil
	})
	cpd, err := c.buildCPD()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.doRequest(map[string]any{"o": "init", "cpd": cpd}); err != nil {
		t.Fatal(err)
	}
	if p["x-mme-client-info"] != oldInfo {
		t.Fatal("changed provider-owned header map")
	}
	h, err := c.twoFactorHeaders("test-adsid", "test-idms")
	if err != nil {
		t.Fatal(err)
	}
	if h["X-Mme-Client-Info"] != expected {
		t.Fatal("two-factor still sends legacy client info")
	}
}

func TestBrowserTwoFactorProfilePreservesAnisetteIdentity(t *testing.T) {
	t.Setenv("APPLESERVICES_2FA_PROFILE", "browser")
	c := NewClient(headerProvider{"X-Mme-Device-Id": "test-device", "X-Apple-I-MD": "test-md", "User-Agent": "akd-test"})
	h, err := c.twoFactorHeaders("test-adsid", "test-idms")
	if err != nil || h["X-Mme-Device-Id"] != "test-device" || h["X-Apple-I-MD"] != "test-md" || !strings.HasPrefix(h["User-Agent"], "Mozilla/5.0") {
		t.Fatal("2FA browser comparison changed the authentication identity", err)
	}
}

func TestGrandSlamCarriesAnisetteInHTTPHeadersAndCPD(t *testing.T) {
	provider := headerProvider{
		"X-Apple-I-MD": "test-md", "X-Apple-I-MD-M": "test-mid",
		"X-Apple-I-MD-RINFO": "17106176", "X-Apple-I-MD-LU": "test-lu",
		"X-Mme-Device-Id": "test-device", "X-Apple-I-SRL-NO": "test-serial",
		"X-Apple-I-Client-Time": "2026-11-24T00:00:00Z",
		"Authorization":         "must-not-forward", "X-Unrelated": "must-not-forward",
	}
	for _, operation := range []string{"init", "complete", "apptokens"} {
		t.Run(operation, func(t *testing.T) {
			c := NewClient(provider)
			c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				data, _ := io.ReadAll(r.Body)
				var envelope map[string]any
				if _, err := plist.Unmarshal(data, &envelope); err != nil {
					t.Fatal(err)
				}
				request := envelope["Request"].(map[string]any)
				cpd := request["cpd"].(map[string]any)
				for key, value := range provider {
					if key == "Authorization" || key == "X-Unrelated" {
						if r.Header.Get(key) != "" {
							t.Errorf("forwarded unrelated HTTP header %s", key)
						}
						continue
					}
					if r.Header.Get(key) != value || cpd[key] != value {
						t.Errorf("%s must carry the same device value in HTTP headers and CPD", key)
					}
				}
				return plistResponse(t, map[string]any{"Response": map[string]any{"Status": map[string]any{"ec": int64(0)}}}), nil
			})
			cpd, err := c.buildCPD()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.doRequest(map[string]any{"o": operation, "cpd": cpd}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGrandSlamStopsOnUnknownProtocol(t *testing.T) {
	c := NewClient(headerProvider{})
	requests := 0
	c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return plistResponse(t, map[string]any{"Response": map[string]any{
			"Status": map[string]any{"ec": int64(0)}, "sp": "future-protocol",
			"s": []byte{1}, "B": []byte{2}, "c": "test-challenge", "i": int64(1000),
		}}), nil
	})
	if _, err := c.Login("owner@example.test", "test-only-password"); err == nil || !strings.Contains(err.Error(), "unsupported SRP protocol") {
		t.Fatal("unknown protocol fell back to s2k")
	}
	if requests != 1 {
		t.Fatal("submitted an invalid proof after protocol negotiation failed")
	}
}

func TestGrandSlamDoesNotReplayRefusedOrUnconfirmedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, operation, body string
		status                int
	}{
		{"forbidden", "init", "denied", 403},
		{"not-found", "init", "missing", 404},
		{"rate-limited", "init", "slow down", 429},
		{"empty", "init", "", 200},
		{"unexpected-body", "init", "<html>unavailable</html>", 200},
		{"no-content", "init", "", 204},
		{"complete-unavailable", "complete", "unavailable", 503},
		{"token-unavailable", "apptokens", "unavailable", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(headerProvider{})
			requests := 0
			c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				if requests > 1 {
					return plistResponse(t, map[string]any{"Response": map[string]any{"Status": map[string]any{"ec": int64(0)}}}), nil
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			_, err := c.doRequest(map[string]any{"o": tc.operation})
			if requests != 1 || err == nil {
				t.Fatalf("unconfirmed request replayed: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestGrandSlamErrorsIdentifyStage(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		if !retryableGSAStatus(status) {
			t.Fatalf("status %d is not a temporary gateway failure", status)
		}
		c := NewClient(headerProvider{})
		requests := 0
		c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("test error"))}, nil
		})
		if _, err := c.doRequest(map[string]any{"o": "init"}); err == nil {
			t.Fatal("exhausted retries were reported as success")
		}
		if requests != 3 {
			t.Fatalf("made %d attempts, want 3", requests)
		}
	}
	for _, status := range []int{204, 400, 401, 403, 404, 405, 429, 500} {
		c := NewClient(headerProvider{})
		c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("test error"))}, nil
		})
		_, err := c.doRequest(map[string]any{"o": "init"})
		var authErr *AuthenticationError
		if !errors.As(err, &authErr) || authErr.Operation != "init" || authErr.HTTPStatus != status {
			t.Fatal("HTTP rejection was not distinguished from authentication rejection")
		}
	}
	for _, response := range []map[string]any{
		{"Status": map[string]any{"ec": int64(-22406), "em": "test-private-account-message"}},
		{"ec": int64(-22406), "em": "test-private-account-message"},
	} {
		_, err := checkStatus(response, "complete", "s2k_fo")
		var authErr *AuthenticationError
		if !errors.As(err, &authErr) || authErr.Code != -22406 || authErr.Protocol != "s2k_fo" {
			t.Fatal("lost Apple error code or protocol context")
		}
		if strings.Contains(err.Error(), "wrong password") || strings.Contains(err.Error(), "test-private-account-message") {
			t.Fatal("error guessed a password failure or exposed a raw account message")
		}
	}
}

func TestTrustedDeviceChecksPlistErrorWithHTTP200(t *testing.T) {
	c := NewClient(headerProvider{})
	c.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return plistResponse(t, map[string]any{"ec": int64(-21669)}), nil
	})
	err := c.SubmitTrustedDeviceCode("test-adsid", "test-idms", "123456")
	var authErr *AuthenticationError
	if !errors.As(err, &authErr) || authErr.Operation != "validate" || authErr.Code != -21669 {
		t.Fatal("HTTP 200 hid a rejected verification code")
	}
}

func TestSMSMetadataFormats(t *testing.T) {
	for _, body := range []string{
		`{"trustedPhoneNumbers":[{"id":7}]}`,
		`{"direct":{"phoneNumberVerification":{"trustedPhoneNumbers":[{"id":7}]}}}`,
		`{"direct":{"phoneNumberVerification":{"trustedPhoneNumber":{"id":7}}}}`,
		`<script type="text/javascript" class="boot_args">{"direct":{"phoneNumberVerification":{"trustedPhoneNumber":{"id":7}}}}</script>`,
		`<SCRIPT class='other boot_args extra'>{"trustedPhoneNumbers":[{"id":7}]}</SCRIPT>`,
	} {
		id, err := parseTrustedPhoneID([]byte(body))
		if err != nil || id != 7 {
			t.Fatalf("cannot parse supported SMS metadata: %v", err)
		}
	}
	if _, err := parseTrustedPhoneID([]byte(`{"trustedPhoneNumbers":[]}`)); err == nil {
		t.Fatal("accepted missing phone identifier")
	}
}

func TestSMSSubmitUsesTheRequestedPhone(t *testing.T) {
	c := NewClient(headerProvider{})
	discoveries := 0
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
		}
		if r.URL.Hostname() != "gsa.apple.com" {
			t.Error("SMS verification used a different authentication host")
		}
		if r.Method == "GET" && r.URL.Path == "/auth" {
			discoveries++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"trustedPhoneNumbers":[{"id":7}]}`))}, nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`"id":7`)) {
			t.Error("SMS submission changed phone identifier")
		}
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := c.RequestSMSCode("test-adsid", "test-idms"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubmitSMSCode("test-adsid", "test-idms", "123456"); err != nil {
		t.Fatal(err)
	}
	if discoveries != 1 {
		t.Fatal("SMS submission rediscovered a potentially different phone")
	}
}

func TestExplicitSMSPhoneStaysBoundWithoutMetadata(t *testing.T) {
	c := NewClient(headerProvider{})
	requests := 0
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
		}
		requests++
		if r.Method == "GET" {
			t.Fatal("explicit phone selection unexpectedly fetched metadata")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`"id":7`)) {
			t.Fatal("request and verification did not use the selected phone")
		}
		return &http.Response{StatusCode: 204, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := c.RequestSMSCodeForPhone("test-adsid", "test-idms", 0); err == nil || requests != 0 {
		t.Fatal("invalid phone selection sent a request")
	}
	if err := c.RequestSMSCodeForPhone("test-adsid", "test-idms", 7); err != nil {
		t.Fatal(err)
	}
	if err := c.SubmitSMSCode("test-adsid", "test-idms", "123456"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatal("unexpected request count")
	}
}

func TestSMSLimitStopsBeforeSendingAndRedactsResponse(t *testing.T) {
	c := NewClient(headerProvider{})
	requests := 0
	c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 404, Body: http.NoBody}, nil
		}
		requests++
		if r.Method != "GET" || r.URL.Path != "/auth" {
			t.Fatal("sent SMS after Apple rejected number discovery")
		}
		body := `{"securityCode":{"tooManyCodesSent":true},"noTrustedDevices":true,"serviceErrors":[{"code":"-22981","message":"owner@example.test 123456 private-token"}]}`
		return &http.Response{StatusCode: 423, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	err := c.RequestSMSCode("test-adsid", "test-idms")
	var responseErr *SecondFactorError
	if !errors.As(err, &responseErr) || responseErr.HTTPStatus != 423 || responseErr.Code != -22981 || !responseErr.TooManyCodesSent || !responseErr.NoTrustedDevices {
		t.Fatal("lost Apple's explicit SMS limit state")
	}
	if requests != 1 {
		t.Fatal("retried a rejected second-factor request")
	}
	for _, private := range []string{"owner@example.test", "123456", "private-token"} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("exposed private response fields")
		}
	}
}

func TestAuthKitSecondFactorContextPreservesMacIdentity(t *testing.T) {
	t.Setenv("APPLESERVICES_2FA_PROFILE", "authkit")
	info := "<test-Mac> <macOS;13.1;22C65> <com.apple.AuthKit/1 (com.apple.akd/1.0)>"
	c := NewClient(headerProvider{"X-Mme-Client-Info": info, "X-Mme-Device-Id": "test-device", "X-Apple-I-MD": "test-md", "X-Apple-I-MD-M": "test-mid"})
	if _, err := c.twoFactorHeaders("test-adsid", "test-idms"); err == nil {
		t.Fatal("silently applied a Mac context to a non-native client")
	}
	c.EnableNativeDeviceAuthentication()
	h, err := c.twoFactorHeaders("test-adsid", "test-idms")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h["X-Mme-Client-Info"], "<test-Mac> <macOS;13.1;22C65>") || h["X-Mme-Device-Id"] != "test-device" || h["X-Apple-I-MD"] != "test-md" || h["X-Apple-I-MD-M"] != "test-mid" {
		t.Fatal("AuthKit comparison changed the provisioned identity")
	}
	if h["X-Apple-AK-Context-Type"] != "icloud" || h["X-Apple-I-Client-Bundle-Id"] != "com.apple.systempreferences.AppleIDSettings" {
		t.Fatal("missing native second-factor context")
	}
}
