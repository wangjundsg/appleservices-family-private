package icloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"howett.net/plist"
)

// Synthetic fixtures model fields read by Apple's public commerce frontend.
// They are not captured authenticated responses or proof of live compatibility.
const testGiftCode = "X111111111111111"

type giftFixture struct {
	summaryReads, preflights, redemptions         int
	requests                                      []string
	account, country, billing, preflight, receipt string
	transportFailure                              bool
	redirect                                      bool
	badEndpoint                                   bool
	unchangedBalance                              bool
}

func newGiftFixture() *giftFixture {
	return &giftFixture{
		account: "owner@example.test", country: "US",
		billing:   `{"fullName":"Test Owner","street1":"Test Street","cityStateZip":"Test City"}`,
		preflight: `{"hasError":false,"redeemableOptions":[{"redeemActionType":"RedeemCodeSrv","accountToUse":"Media"}]}`,
		receipt:   `{"status":0,"email":"owner@example.test","redeemedCredit":{"money":"$1.00"},"totalCredit":{"money":"$1.00"}}`,
	}
}

func (f *giftFixture) session(t *testing.T) *StoreCommerceSession {
	t.Helper()
	client := &http.Client{Transport: testStoreTransport(func(req *http.Request) (*http.Response, error) {
		f.requests = append(f.requests, req.URL.Path)
		status, body := 200, ""
		if req.URL.Host == "init.itunes.apple.com" {
			if req.Header.Get("X-Token") != "" {
				t.Fatal("sent credentials to public bag")
			}
			bag := map[string]string{
				"accountSummaryGraphQL": "https://buy.itunes.apple.com/account/v1/summary/graphql",
				"codeDetails":           "https://buy.itunes.apple.com/commerce/codeDetails",
				"redeemCodeSrv":         "https://buy.itunes.apple.com/commerce/redeemCodeSrv",
			}
			if f.badEndpoint {
				bag["redeemCodeSrv"] = "https://untrusted.example.test/commerce/redeemCodeSrv"
			}
			data, _ := plist.Marshal(bag, plist.XMLFormat)
			body = string(data)
		} else {
			if req.Method != "POST" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("X-Dsid") != "test-dsid" || req.Header.Get("X-Token") != "test-token" || req.URL.RawQuery != "" {
				t.Fatal("lost account binding or exposed code in URL")
			}
			var request map[string]any
			if json.NewDecoder(req.Body).Decode(&request) != nil {
				t.Fatal("bad JSON request")
			}
			switch req.URL.Path {
			case "/account/v1/summary/graphql":
				f.summaryReads++
				balance := "$0.00"
				if f.redemptions > 0 && !f.unchangedBalance {
					balance = "$1.00"
				}
				data, _ := json.Marshal(map[string]any{"data": map[string]any{
					"account":  map[string]string{"appleId": f.account, "country": f.country},
					"payments": map[string]any{"balance": balance, "billingAddress": json.RawMessage(f.billing)},
				}})
				body = string(data)
			case "/commerce/codeDetails":
				f.preflights++
				if request["code"] != testGiftCode || request["requestType"] != "PreRedeem" {
					t.Fatal("preflight changed into mutation")
				}
				body = f.preflight
			case "/commerce/redeemCodeSrv":
				f.redemptions++
				if request["code"] != testGiftCode || request["guid"] != "TEST-GUID" || request["isSplitAccount"] != false {
					t.Fatal("wrong redemption binding")
				}
				if f.transportFailure {
					return nil, errors.New("synthetic timeout with sensitive body " + testGiftCode)
				}
				body = f.receipt
				if f.redirect {
					status = 307
				}
			default:
				t.Fatalf("unexpected request (including profile edits/purchases): %s", req.URL.Path)
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://untrusted.example.test/"}}, Request: req, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return &StoreCommerceSession{client: client, identity: &storeSession{DSID: "test-dsid", PasswordToken: "test-token", GUID: "TEST-GUID", StoreFront: "143441-1,29"}}
}

func TestGiftCardReadsExistingBillingAndCreditsOnce(t *testing.T) {
	f := newGiftFixture()
	a, err := f.session(t).PrepareGiftCard(context.Background(), "owner@example.test", "US", testGiftCode)
	if err != nil {
		t.Fatal(err)
	}
	if f.redemptions != 0 || f.preflights != 1 || len(a.Check().MissingFields) != 0 {
		t.Fatal("preflight mutated account or ignored existing profile")
	}
	public, _ := json.Marshal(a.Check())
	if strings.Contains(string(public), "Test Street") || strings.Contains(string(public), testGiftCode) {
		t.Fatal("readiness leaked identity or card")
	}
	result, err := a.Submit(context.Background())
	if err != nil || result.State != "credited" || result.CreditMinor != 100 || result.BalanceMinor != 100 || f.summaryReads != 2 {
		t.Fatalf("credit not verified: %+v %v", result, err)
	}
	if _, err := a.Submit(context.Background()); err == nil || f.redemptions != 1 {
		t.Fatal("redemption replayed")
	}
}

func TestGiftCardPreconditionsBlockMutation(t *testing.T) {
	for _, scenario := range []string{"account", "country", "storefront", "anonymous", "endpoint", "preflight-error", "preflight-empty", "preflight-purchase", "billing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGiftFixture()
			s := f.session(t)
			switch scenario {
			case "account":
				f.account = "other@example.test"
			case "country":
				f.country = "CN"
			case "storefront":
				s.identity.StoreFront = "143465"
			case "anonymous":
				s.identity.Anonymous = true
			case "endpoint":
				f.badEndpoint = true
			case "preflight-error":
				f.preflight = `{"hasError":true}`
			case "preflight-empty":
				f.preflight = `{}`
			case "preflight-purchase":
				f.preflight = `{"hasError":false,"redeemableOptions":[{"redeemActionType":"BuyProduct"}]}`
			case "billing":
				f.billing = `{"fullName":"Existing Name"}`
			}
			a, err := s.PrepareGiftCard(context.Background(), "owner@example.test", "US", testGiftCode)
			if scenario == "billing" {
				if err != nil || len(a.Check().MissingFields) != 2 {
					t.Fatalf("missing fields not reported: %v", err)
				}
				_, err = a.Submit(context.Background())
			}
			if err == nil || f.redemptions != 0 {
				t.Fatalf("unsafe precondition passed: %v", err)
			}
		})
	}
}

func TestGiftCardAmbiguousResponsesNeverReplayOrReportCredit(t *testing.T) {
	for _, scenario := range []string{"timeout", "redirect", "html", "empty", "error-key", "replay", "wrong-account", "unrelated-balance", "unchanged-balance", "dialog"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGiftFixture()
			switch scenario {
			case "timeout":
				f.transportFailure = true
			case "redirect":
				f.redirect = true
			case "html":
				f.receipt = "<html>sign in</html>"
			case "empty":
				f.receipt = `{}`
			case "error-key":
				f.receipt = `{"status":-1,"errorMessageKey":"MZCommerce.GiftCardVerificationNumber"}`
			case "replay":
				f.receipt = `{"errorMessageKey":"MZFinance.RedeemReplayNeeded.WebKit2"}`
			case "wrong-account":
				f.receipt = strings.ReplaceAll(f.receipt, "owner@example.test", "other@example.test")
			case "unrelated-balance":
				f.receipt = strings.Replace(f.receipt, "$1.00", "$2.00", 1)
			case "unchanged-balance":
				f.unchangedBalance = true
			case "dialog":
				f.receipt = `{"status":0,"dialog":{"kind":"confirm"}}`
			}
			a, err := f.session(t).PrepareGiftCard(context.Background(), "owner@example.test", "US", testGiftCode)
			if err != nil {
				t.Fatal(err)
			}
			result, err := a.Submit(context.Background())
			if err == nil || result.State != "unknown" || f.redemptions != 1 || f.summaryReads != 2 {
				t.Fatalf("ambiguous submission mishandled: %+v %v", result, err)
			}
			if strings.Contains(err.Error(), testGiftCode) {
				t.Fatal("error leaked card")
			}
			if _, err := a.Submit(context.Background()); err == nil || f.redemptions != 1 {
				t.Fatal("ambiguous redemption replayed")
			}
		})
	}
}

func TestGiftMoneyRequiresExplicitCurrencyAndCents(t *testing.T) {
	for _, display := range []string{"", "1", "$1", "CAD1.00", "€1.00", "$-1.00", "$1.001", "$1,000.00", "$9999999999.00"} {
		if _, ok := giftUSDMinor(display); ok {
			t.Fatalf("guessed money format %q", display)
		}
	}
	if n, ok := giftUSDMinor("US$\u00a01.00"); !ok || n != 100 {
		t.Fatal("USD cents lost")
	}
}
