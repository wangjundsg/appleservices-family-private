package appleweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type appleAuthTestTransport func(*http.Request) (*http.Response, error)

func (f appleAuthTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOld2FAReportsRequiredICloudActionWithoutReplayingOTP(t *testing.T) {
	for _, tc := range []struct {
		name, response, code string
	}{
		{"terms", `{"termsUpdateNeeded":true,"trustTokens":["fixture-trust-secret"]}`, "icloud_terms_update_needed"},
		{"repair", `{"isRepairNeeded":true}`, "icloud_account_repair_needed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, trusted, exchanged := 0, 0, 0
			client := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
				body := `{}`
				switch req.URL.Path {
				case "/appleauth/auth/verify/phone/securitycode":
					verified++
				case "/appleauth/auth/2sv/trust":
					trusted++
				case "/setup/ws/1/accountLogin":
					exchanged++
					body = tc.response
				default:
					t.Fatalf("partial session must stop before validation: %s", req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}}
			session := &appleAuthSession{Endpoints: appleAuthEndpointsForHost("www.icloud.com.cn"), SessionToken: "test-token", TwoFactorMethod: appleTwoFactorMethodPhone}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := client.Submit2FA(context.Background(), appleAuthPending{Session: session}, "000123", nil)
				code, message, retryable := ErrorDetails(err)
				if code != tc.code || retryable || !strings.Contains(message, "https://www.icloud.com.cn") || strings.Contains(message, "fixture-trust-secret") {
					t.Fatalf("missing safe account action: %v", err)
				}
			}
			if verified != 1 || trusted != 1 || exchanged != 2 {
				t.Fatalf("accepted stages replayed: verify=%d trust=%d exchange=%d", verified, trusted, exchanged)
			}
		})
	}
}

func TestAccountLoginDiagnosticDistinguishesPartialSessionWithoutSecrets(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://setup.icloud.com/setup/ws/1/accountLogin", nil)
	session := &appleAuthSession{Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-REPAIR", Value: "fixture-cookie-secret", Domain: ".icloud.com", Path: "/"}}}
	data := []byte(`{"termsUpdateNeeded":true,"hsaChallengeRequired":false,"dsInfo":{"dsid":"fixture-account-secret"},"webservices":{"mail":{"url":"https://private-mail-url"}},"trustTokens":["fixture-token-secret"]}`)
	diagnostic := appleSMSExchangeDiagnostic(req, http.StatusOK, session, nil, data)
	for _, expected := range []string{"step=account_login", "terms_update_needed=true", "repair_needed=unknown", "challenge_required=false", "dsinfo_present=true", "webservices=1", "webauth_token=false", "webauth_validate=false", "webauth_repair=true"} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("missing %s: %s", expected, diagnostic)
		}
	}
	for _, secret := range []string{"fixture-cookie-secret", "fixture-account-secret", "fixture-token-secret", "private-mail-url"} {
		if strings.Contains(diagnostic, secret) {
			t.Fatalf("diagnostic exposed %s", secret)
		}
	}
}

func TestOld2FAResumesCompletedStagesAfterDomainRedirect(t *testing.T) {
	for _, method := range []string{appleTwoFactorMethodPhone, appleTwoFactorMethodTrustedDevice} {
		for _, redirectStage := range []string{"account_login", "trust"} {
			t.Run(method+"/"+redirectStage, func(t *testing.T) {
				verified, trusted, exchanged := 0, 0, 0
				client := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
					status, body := http.StatusOK, `{}`
					switch req.URL.Path {
					case "/appleauth/auth/verify/phone/securitycode", "/appleauth/auth/verify/trusteddevice/securitycode":
						verified++
						if verified > 1 {
							status = http.StatusUnauthorized // Mirrors an already consumed OTP on the wrong domain.
						}
					case "/appleauth/auth/2sv/trust":
						trusted++
						if redirectStage == "trust" && trusted == 1 {
							status, body = http.StatusTemporaryRedirect, `{"domainToUse":"iCloud.com"}`
						}
					case "/setup/ws/1/accountLogin":
						exchanged++
						if redirectStage == "account_login" && exchanged == 1 {
							status, body = http.StatusTemporaryRedirect, `{"domainToUse":"iCloud.com"}`
						} else {
							status = http.StatusServiceUnavailable // Stop before real session validation; the exchange stage must be reached.
							if req.URL.Host != "setup.icloud.com" {
								t.Errorf("exchange did not follow Apple's target domain: %s", req.URL.Host)
							}
						}
					default:
						return nil, fmt.Errorf("unexpected request %s", req.URL)
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}}
				session := &appleAuthSession{
					Endpoints: appleAuthEndpointsForHost("www.icloud.com.cn"), SessionToken: "test-token",
					TwoFactorMethod: method, TwoFactorPhone: json.RawMessage(`{"id":7,"nonFTEU":false}`),
				}
				pending := appleAuthPending{Session: session}
				_, err := client.Submit2FA(context.Background(), pending, "000123", nil)
				if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
					t.Fatalf("expected exchange-stage failure, not a replayed OTP: %v", err)
				}
				if verified != 1 {
					t.Fatalf("an accepted OTP was submitted %d times", verified)
				}
				wantTrust := 1
				if redirectStage == "trust" {
					wantTrust = 2
				}
				if trusted != wantTrust {
					t.Fatalf("completed trust stage was repeated: got %d, want %d", trusted, wantTrust)
				}
				_, err = client.Submit2FA(context.Background(), pending, "000123", nil)
				if err == nil || !strings.Contains(err.Error(), "HTTP 503") || verified != 1 || trusted != wantTrust {
					t.Fatalf("manual retry replayed completed stages: err=%v verify=%d trust=%d", err, verified, trusted)
				}
			})
		}
	}
}

func Test2FAVerificationDoesNotAdvanceOnRejectedCode(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{Endpoints: appleAuthEndpoints{Auth: server.URL}, TwoFactorMethod: appleTwoFactorMethodPhone}
	for i := 0; i < 2; i++ {
		if err := client.verify2FAWithSession(context.Background(), session, "000123", nil); err == nil {
			t.Fatal("rejected code should fail")
		}
		if session.TwoFactorVerified || session.TrustEstablished {
			t.Fatal("rejected code advanced the login stage")
		}
	}
	if requests != 2 {
		t.Fatal("verification retry was skipped after a rejected code")
	}
}

func TestOld2FADomainRedirectLoopIsBounded(t *testing.T) {
	verified, exchanged := 0, 0
	client := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, `{}`
		switch req.URL.Path {
		case "/appleauth/auth/verify/phone/securitycode":
			verified++
		case "/appleauth/auth/2sv/trust":
		case "/setup/ws/1/accountLogin":
			exchanged++
			status = http.StatusTemporaryRedirect
			body = `{"domainToUse":"iCloud.com"}`
			if req.URL.Host == "setup.icloud.com" {
				body = `{"domainToUse":"iCloud.com.cn"}`
			}
		default:
			return nil, fmt.Errorf("unexpected request %s", req.URL)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}}
	session := &appleAuthSession{Endpoints: appleAuthEndpointsForHost("www.icloud.com.cn"), SessionToken: "test-token", TwoFactorMethod: appleTwoFactorMethodPhone}
	_, err := client.Submit2FA(context.Background(), appleAuthPending{Session: session}, "000123", nil)
	if err == nil || !strings.Contains(err.Error(), "登录域切换") || verified != 1 || exchanged != 2 {
		t.Fatalf("domain loop was not bounded without OTP replay: err=%v verify=%d exchange=%d", err, verified, exchanged)
	}
}

func TestAppleSMSDiagnosticRecordsMetadataWithoutCredentials(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://idmsa.apple.com/appleauth/auth/verify/phone/securitycode", nil)
	req.Header.Set("scnt", "request-scnt-secret")
	req.Header.Set("X-Apple-ID-Session-Id", "request-session-secret")
	req.Header.Set("Cookie", "auth-cookie-secret")
	session := &appleAuthSession{
		AppleID: "private-account@example.com", FrameID: "frame-secret", AccountCountry: "USA",
		Scnt: "next-scnt-secret", SessionID: "next-session-secret", SessionToken: "token-secret",
		TwoFactorPhone: json.RawMessage(`{"id":7,"nonFTEU":false,"numberWithDialCode":"private-phone-number"}`),
	}
	body := map[string]any{
		"phoneNumber":  map[string]any{"id": 7, "nonFTEU": false},
		"securityCode": map[string]string{"code": "654321"}, "password": "password-secret",
	}
	data := []byte(`{"securityCode":{"code":"654321","valid":false,"securityCodeLocked":false,"securityCodeCooldown":true},"service_errors":[{"code":"-21669","message":"private-account@example.com"},{"code":"654321"},{"code":"token-secret"}],"phoneNumber":{"id":7,"obfuscatedNumber":"private-phone-number"}}`)
	diagnostic := appleSMSExchangeDiagnostic(req, http.StatusUnauthorized, session, body, data)
	for _, expected := range []string{"step=sms_verify", "host=idmsa.apple.com", `country_route="www.icloud.com"`, "status=401", "non_fteu=false", "valid=false", "locked=false", "cooldown=true", "service_codes=[-21669]", "sent_phone=" + appleDebugFingerprint("7"), "stored_phone=" + appleDebugFingerprint("7")} {
		if !strings.Contains(diagnostic, expected) {
			t.Errorf("diagnostic missing %q: %s", expected, diagnostic)
		}
	}
	for _, secret := range []string{"private-account@example.com", "frame-secret", "request-scnt-secret", "next-scnt-secret", "request-session-secret", "next-session-secret", "auth-cookie-secret", "private-phone-number", "654321", "password-secret", "token-secret"} {
		if strings.Contains(diagnostic, secret) {
			t.Errorf("diagnostic exposed secret %q", secret)
		}
	}
}

func TestAppleSMSDiagnosticHandlesMissingMetadataAndNonJSON(t *testing.T) {
	for _, reply := range []string{"", "<html>private-account@example.com password-secret</html>", `{"phoneNumberVerification":{"securityCode":{"tooManyCodesSent":true}}}`} {
		req := httptest.NewRequest(http.MethodGet, "https://idmsa.apple.com.cn/appleauth/auth", nil)
		diagnostic := appleSMSExchangeDiagnostic(req, http.StatusOK, &appleAuthSession{}, nil, []byte(reply))
		if !strings.Contains(diagnostic, "stored_phone=-") || !strings.Contains(diagnostic, "sent_phone=-") {
			t.Fatal("missing metadata was not distinguishable")
		}
		if strings.Contains(diagnostic, "private-account") || strings.Contains(diagnostic, "password-secret") {
			t.Fatal("non-JSON response was exposed")
		}
		if strings.Contains(reply, "tooManyCodesSent") && !strings.Contains(diagnostic, "too_many_sent=true") {
			t.Fatal("nested SMS cooldown flags were not recorded")
		}
	}
	req := httptest.NewRequest(http.MethodPost, "https://idmsa.apple.com/appleauth/auth/signin/init", nil)
	if diagnostic := appleSMSExchangeDiagnostic(req, http.StatusOK, &appleAuthSession{}, nil, nil); diagnostic != "" {
		t.Fatal("unrelated authentication requests should not be logged")
	}
}

func TestRequestPhoneSecurityCodeRemembersPhoneFromSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"phoneNumber":{"id":7,"nonFTEU":false}}`))
	}))
	defer server.Close()
	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints:      appleAuthEndpoints{Auth: server.URL},
		TwoFactorPhone: json.RawMessage(`{"id":1,"nonFTEU":true}`),
	}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(session.TwoFactorPhone), `"id":7`) || !strings.Contains(string(session.TwoFactorPhone), `"nonFTEU":false`) {
		t.Fatalf("successful SMS response phone parameters were not kept: %s", session.TwoFactorPhone)
	}
}

func TestRequestPhoneSecurityCodeKeepsRequestedPhoneWhenMultiplePhonesAreReturned(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPreconditionFailed} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"trustedPhoneNumbers":[{"id":7,"nonFTEU":true},{"id":9,"nonFTEU":false}]}`))
		}))
		client := &AppleAuthClient{httpClient: server.Client()}
		session := &appleAuthSession{
			Endpoints:      appleAuthEndpoints{Auth: server.URL},
			TwoFactorPhone: json.RawMessage(`{"id":9,"nonFTEU":false}`),
		}
		err := client.requestPhoneSecurityCode(context.Background(), session, nil)
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(session.TwoFactorPhone), `"id":9`) || !strings.Contains(string(session.TwoFactorPhone), `"nonFTEU":false`) {
			t.Fatalf("HTTP %d changed the requested SMS phone: %s", status, session.TwoFactorPhone)
		}
	}
}

func TestAuthFacadeKeepsSMSPhoneParametersInBothLoginFlows(t *testing.T) {
	for _, flow := range []string{"icloud_web"} {
		for _, explicit := range []bool{false, true} {
			name := flow + "/saved_phone"
			if explicit {
				name = flow + "/explicit_phone"
			}
			t.Run(name, func(t *testing.T) {
				requests := 0
				wantID := float64(7)
				if explicit {
					wantID = 9
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if r.Method != http.MethodPost || r.URL.Path != "/verify/phone/securitycode" {
						t.Errorf("unexpected SMS request: %s %s", r.Method, r.URL.Path)
					}
					var body struct {
						PhoneNumber  map[string]any    `json:"phoneNumber"`
						SecurityCode map[string]string `json:"securityCode"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.PhoneNumber["id"] != wantID || body.SecurityCode["code"] != "000123" {
						t.Errorf("SMS parameters mismatch: phone=%v code=%q", body.PhoneNumber, body.SecurityCode["code"])
					}
					w.WriteHeader(http.StatusUnauthorized)
				}))
				defer server.Close()
				session := &appleAuthSession{
					Endpoints:       appleAuthEndpoints{Auth: server.URL},
					ClientID:        defaultAppleOAuthClientID,
					TwoFactorMethod: appleTwoFactorMethodPhone,
					TwoFactorPhone:  json.RawMessage(`{"id":7,"nonFTEU":true}`),
				}
				if flow == "apple_account" {
					session.ClientID = appleAccountManageOAuthClientID
					session.Endpoints.Home = "https://account.apple.com"
				}
				facade := &AuthFacade{client: &AppleAuthClient{httpClient: server.Client()}, pending: newAppleAuthPendingStore()}
				pending, err := facade.pending.put(session)
				if err != nil {
					t.Fatal(err)
				}
				var phone json.RawMessage
				if explicit {
					phone = json.RawMessage(`{"id":9,"nonFTEU":true}`)
				}
				_, err = facade.Submit2FA(context.Background(), pending.ID, "000123", phone)
				if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "本次登录") {
					t.Fatalf("expected an actionable 401 message, got %v", err)
				}
				if requests != 1 {
					t.Fatalf("verification should not trigger repeated requests: %d", requests)
				}
				if _, ok := facade.pending.get(pending.ID); !ok {
					t.Fatal("a failed verification unexpectedly removed its pending session")
				}
			})
		}
	}
}

func TestRequestPhoneSecurityCodeAccepts412AfterSMSWasSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"trustedPhoneNumbers":[{"id":7,"nonFTEU":true,"pushMode":"sms","lastTwoDigits":"41"}]}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints: appleAuthEndpoints{Auth: server.URL},
		UserAgent: appleAuthUserAgent,
	}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err != nil {
		t.Fatalf("412 短信发送结果不应被判定为失败：%v", err)
	}
	if !strings.Contains(string(session.TwoFactorPhone), `"id":7`) {
		t.Fatalf("未保存 Apple 返回的受信任手机号：%s", session.TwoFactorPhone)
	}
}

func TestRequestPhoneSecurityCodeRejects412WithoutPhoneDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":"precondition_failed"}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{Endpoints: appleAuthEndpoints{Auth: server.URL}}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err == nil {
		t.Fatal("缺少受信任手机号信息的 412 响应不应被当作短信已发送")
	}
}
