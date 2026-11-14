package appleweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestICloudTermsConsentContinuesVerifiedLogin(t *testing.T) {
	for _, host := range []string{"www.icloud.com", "www.icloud.com.cn"} {
		t.Run(host, func(t *testing.T) {
			reads, accepts, logins, validates := 0, 0, 0, 0
			c := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, `{}`
				switch req.URL.Path {
				case "/setup/ws/1/getTerms":
					reads++
					body = `{"iCloudTerms":{"version":7,"text":"Read current terms","url":"https://evil.example/terms"},"trustTokens":["never-return-this"]}`
					if req.Method != http.MethodGet {
						t.Fatal("unexpected terms method")
					}
					cookie, e := req.Cookie("X-APPLE-WEBAUTH-REPAIR")
					if e != nil || cookie.Value != "fixture-repair" {
						t.Fatal("missing pending repair cookie")
					}
				case "/setup/ws/1/repairDone":
					accepts++
					var payload map[string]any
					if e := json.NewDecoder(req.Body).Decode(&payload); e != nil || payload["acceptedICloudTerms"] != float64(7) {
						t.Fatal("wrong accepted version")
					}
					body = `{"success":true}`
				case "/setup/ws/1/accountLogin":
					logins++
					body = `{"dsInfo":{"dsid":"fixture-id"}}`
				case "/setup/ws/1/validate":
					validates++
					body = `{"dsInfo":{"dsid":"fixture-id","appleId":"fixture@example.com"},"webservices":{"mail":{"url":"https://mail.icloud.com"}}}`
				default:
					return nil, fmt.Errorf("must not replay authentication: %s", req.URL.Path)
				}
				if req.URL.Host != strings.TrimPrefix(host, "www.") && req.URL.Host != "setup."+strings.TrimPrefix(host, "www.") {
					t.Fatal("terms changed account region")
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}}
			s := &appleAuthSession{Endpoints: appleAuthEndpointsForHost(host), AppleID: "fixture@example.com", SessionToken: "fixture-token", TwoFactorVerified: true, TrustEstablished: true, TermsRequired: true, Cookies: []SessionCookie{{Name: "X-APPLE-WEBAUTH-REPAIR", Value: "fixture-repair", Domain: "." + strings.TrimPrefix(host, "www."), Path: "/"}}}
			facade := &AuthFacade{client: c, pending: newAppleAuthPendingStore()}
			pending, e := facade.pending.put(s)
			if e != nil {
				t.Fatal(e)
			}
			terms, e := facade.Terms(context.Background(), pending.ID)
			if e != nil || terms.Version != "7" || terms.Content != "Read current terms" || strings.Contains(terms.URL, "evil") {
				t.Fatalf("wrong public terms: %+v %v", terms, e)
			}
			for _, args := range []struct {
				v string
				a bool
			}{{"7", false}, {"8", true}} {
				_, e = facade.AcceptTerms(context.Background(), pending.ID, args.v, args.a)
				if e == nil {
					t.Fatal("accepted absent consent or unseen version")
				}
			}
			if accepts != 0 || reads != 1 {
				t.Fatal("consent rejection made upstream requests")
			}
			result, e := facade.AcceptTerms(context.Background(), pending.ID, "7", true)
			if e != nil || result.DSID != "fixture-id" {
				t.Fatalf("verified login failed: %+v %v", result, e)
			}
			if reads != 2 || accepts != 1 || logins != 1 || validates != 1 {
				t.Fatalf("unexpected stages reads=%d accept=%d login=%d validate=%d", reads, accepts, logins, validates)
			}
			if _, ok := facade.pending.get(pending.ID); ok {
				t.Fatal("completed pending session was retained")
			}
		})
	}
}

func TestICloudTermsVersionChangesRequireNewConsent(t *testing.T) {
	accepts := 0
	c := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "repairDone") {
			accepts++
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"iCloudTerms":{"version":8}}`)), Request: req}, nil
	})}}
	s := &appleAuthSession{Endpoints: appleAuthEndpointsForHost("www.icloud.com"), TermsRequired: true, Terms: &ICloudTerms{Version: "7"}}
	_, err := c.acceptICloudTerms(context.Background(), s, "7", true)
	code, _, _ := ErrorDetails(err)
	if code != "icloud_terms_version_mismatch" || accepts != 0 || s.Terms.Version != "8" {
		t.Fatalf("changed terms were silently accepted: %v", err)
	}
}

func TestICloudTermsRetryDoesNotRepeatConfirmedAcceptance(t *testing.T) {
	accepts, logins := 0, 0
	c := &AppleAuthClient{httpClient: &http.Client{Transport: appleAuthTestTransport(func(req *http.Request) (*http.Response, error) {
		status, body := 200, `{}`
		switch req.URL.Path {
		case "/setup/ws/1/getTerms":
			body = `{"iCloudTerms":{"version":7}}`
		case "/setup/ws/1/repairDone":
			accepts++
			body = `{"success":true}`
		case "/setup/ws/1/accountLogin":
			logins++
			body = `{"dsInfo":{"dsid":"fixture-id"}}`
		case "/setup/ws/1/validate":
			if logins == 1 {
				status = 503
			} else {
				body = `{"dsInfo":{"dsid":"fixture-id"}}`
			}
		default:
			t.Fatal("OTP or trust was replayed")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}}
	s := &appleAuthSession{Endpoints: appleAuthEndpointsForHost("www.icloud.com"), SessionToken: "fixture-token", TermsRequired: true, Terms: &ICloudTerms{Version: "7"}}
	if _, err := c.acceptICloudTerms(context.Background(), s, "7", true); err == nil {
		t.Fatal("failed validation reported success")
	}
	if _, err := c.acceptICloudTerms(context.Background(), s, "7", true); err != nil {
		t.Fatal(err)
	}
	if accepts != 1 || logins != 2 {
		t.Fatal("retry replayed confirmation")
	}
}

func TestICloudTermsExpiredSessionDoesNotMakeRequests(t *testing.T) {
	f := &AuthFacade{client: &AppleAuthClient{}, pending: newAppleAuthPendingStore()}
	f.pending.items["expired"] = appleAuthPending{ID: "expired", ExpiresAt: time.Now().Add(-time.Minute), Session: &appleAuthSession{}}
	if _, err := f.Terms(context.Background(), "expired"); err == nil {
		t.Fatal("expired terms session accepted")
	}
	if _, err := f.AcceptTerms(context.Background(), "expired", "7", true); err == nil {
		t.Fatal("expired confirmation accepted")
	}
}
