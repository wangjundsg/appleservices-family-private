package appleweb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestProbeAuthenticatesWithoutRequestingAnyCode(t *testing.T) {
	calls := 0
	client := NewAppleAuthClient()
	client.httpClient.Transport = appleAuthTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		status, body := 200, `{}`
		switch r.URL.Path {
		case "/appleauth/auth/authorize/signin", "/appleauth/auth/federate":
		case "/appleauth/auth/signin/init":
			b, _ := json.Marshal(map[string]any{"b": base64.StdEncoding.EncodeToString([]byte{2}), "salt": base64.StdEncoding.EncodeToString([]byte("test-salt")), "iteration": 1, "protocol": "s2k", "c": "test-challenge"})
			body = string(b)
		case "/appleauth/auth/signin/complete":
			status = 409
		default:
			t.Fatalf("probe requested an extra operation: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	result, err := client.StartLogin(context.Background(), "owner@example.test", "Test123!", "www.icloud.com", "", newAppleAuthPendingStore(), "probe")
	if err != nil || !result.Needs2FA || calls != 4 {
		t.Fatalf("probe did not authenticate: calls=%d err=%v", calls, err)
	}
}

func TestKeepaliveSavesRotatedCookieAndPreservesBrowserIdentity(t *testing.T) {
	SetIdentityOverride("test-browser-UA", "en-GB")
	defer SetIdentityOverride("", "")
	session := ICloudSession{AppleID: "owner@example.test", Host: "www.icloud.com", Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "old", Domain: ".icloud.com", Path: "/"}}}
	session = WithLoginState(session, LoginState{Kind: LoginStateICloudWeb, Host: session.Host, Cookies: session.Cookies})
	validator := &ICloudSessionValidator{httpClient: &http.Client{Transport: appleAuthTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") != "test-browser-UA" || r.Header.Get("Accept-Language") != "en-GB" {
			t.Fatal("profile identity changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"X-APPLE-WEBAUTH-TOKEN=fresh; Domain=.icloud.com; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(`{"dsInfo":{"dsid":"test-dsid","appleId":"owner@example.test"}}`)), Request: r}, nil
	})}}
	next, err := validator.ValidateSession(context.Background(), session, session.Host)
	if err != nil || next.Cookies[0].Value != "fresh" || next.LoginStates[0].Cookies[0].Value != "fresh" {
		t.Fatal("rotated session cookie was lost")
	}
	if session.Cookies[0].Value != "old" {
		t.Fatal("original checkpoint was modified before saving")
	}
}

func TestProbeRequiresBothIdentifiersFromRemoteSession(t *testing.T) {
	s := ICloudSession{AppleID: "owner@example.test", DSID: "123", Host: "www.icloud.com", Cookies: []SessionCookie{{Name: "fixture", Value: "fixture", Domain: ".icloud.com", Path: "/"}}}
	for _, tc := range []struct {
		dsid, email string
		want        bool
	}{
		{"123", "owner@example.test", true}, {"123", "", false}, {"", "owner@example.test", false}, {"456", "owner@example.test", false}, {"123", "other@example.test", false},
	} {
		v := &ICloudSessionValidator{httpClient: &http.Client{Transport: appleAuthTestTransport(func(r *http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]any{"dsInfo": map[string]string{"dsid": tc.dsid, "appleId": tc.email}})
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})}}
		if got := v.ConfirmsIdentity(context.Background(), s); got != tc.want {
			t.Fatalf("dsid=%q email=%q confirmed=%t", tc.dsid, tc.email, got)
		}
	}
}
