package icloud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"howett.net/plist"
)

// GiftCardCheck reports readiness without exposing the card or billing identity.
// This first adapter supports US account credit and the existing USD 50GB flow.
type GiftCardCheck struct {
	Country       string   `json:"country"`
	Currency      string   `json:"currency"`
	BalanceMinor  int64    `json:"balance_minor"`
	MissingFields []string `json:"missing_billing_fields,omitempty"`
}

type GiftCardResult struct {
	State        string `json:"state"`
	CreditMinor  int64  `json:"credit_minor,omitempty"`
	BalanceMinor int64  `json:"balance_minor"`
}

// GiftCardAttempt retains sensitive values in memory and permits one submission.
// Callers must durably record intent before Submit, and never replay an unknown result.
type GiftCardAttempt struct {
	session   *StoreCommerceSession
	appleID   string
	code      string
	endpoints map[string]string
	check     GiftCardCheck
	submitted atomic.Bool
}

var giftCodePattern = regexp.MustCompile(`^X[A-Z0-9]{15}$`)
var usdBalancePattern = regexp.MustCompile(`^(?:US\$|USD|\$)([0-9]+)\.([0-9]{2})$`)

// Only parse explicit USD displays from an authenticated US storefront. Never
// guess units from Apple's moneyRaw or treat an absent balance as zero.
func giftUSDMinor(display string) (int64, bool) {
	display = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(display), "\u00a0", ""), " ", "")
	m := usdBalancePattern.FindStringSubmatch(display)
	if m == nil || len(m[1]) > 9 {
		return 0, false
	}
	dollars, _ := strconv.ParseInt(m[1], 10, 64)
	cents, _ := strconv.ParseInt(m[2], 10, 64)
	return dollars*100 + cents, true
}

func giftFailure(stage string) error { return &StorageError{Stage: "gift-card-" + stage} }

func (s *StoreCommerceSession) giftEndpoint(raw, path string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Fragment == "" &&
		u.RawQuery == "" && u.Port() == "" && u.Path == path &&
		(u.Host == "buy.itunes.apple.com" || (s.identity.Pod != "" && u.Host == "p"+s.identity.Pod+"-buy.itunes.apple.com"))
}

// Keep gift codes and authenticated responses out of the generic diagnostic
// dump path. Redirects and transport retries are forbidden, including on POST.
func (s *StoreCommerceSession) giftRequest(ctx context.Context, endpoint string, body any, bag bool) ([]byte, error) {
	if s == nil || s.identity == nil || s.identity.Anonymous || !s.MatchesAccount(s.identity.DSID) {
		return nil, giftFailure("session")
	}
	method := http.MethodPost
	var encoded []byte
	if bag {
		method = http.MethodGet
	} else {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, giftFailure("request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, giftFailure("request")
	}
	req.Header.Set("User-Agent", storeMacUserAgent)
	if bag {
		req.Header.Set("X-Apple-Store-Front", s.identity.StoreFront)
	} else {
		s.identity.setHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
	}
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, giftFailure("transport")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStoreBagBytes+1))
	if err != nil || len(data) > maxStoreBagBytes {
		return nil, giftFailure("response-size")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StorageError{Stage: "gift-card-response", HTTPStatus: resp.StatusCode}
	}
	return data, nil
}

func (s *StoreCommerceSession) giftEndpoints(ctx context.Context) (map[string]string, error) {
	data, err := s.giftRequest(ctx, storeBagURL+"?"+url.Values{"ix": {"6"}, "guid": {s.identity.GUID}}.Encode(), nil, true)
	if err != nil {
		return nil, err
	}
	var bag map[string]any
	if _, err := plist.Unmarshal(data, &bag); err != nil {
		return nil, giftFailure("bag-decode")
	}
	paths := map[string]string{
		"accountSummaryGraphQL": "/account/v1/summary/graphql",
		"codeDetails":           "/commerce/codeDetails",
		"redeemCodeSrv":         "/commerce/redeemCodeSrv",
	}
	endpoints := make(map[string]string, len(paths))
	for key, path := range paths {
		endpoint, _ := bag[key].(string)
		if !s.giftEndpoint(endpoint, path) {
			return nil, giftFailure("bag-endpoint")
		}
		endpoints[key] = endpoint
	}
	return endpoints, nil
}

func (a *GiftCardAttempt) readAccount(ctx context.Context) (GiftCardCheck, error) {
	check := GiftCardCheck{Country: "US", Currency: "USD"}
	data, err := a.session.giftRequest(ctx, a.endpoints["accountSummaryGraphQL"], map[string]string{"query": `query requestAccountSummary {
 account { appleId country }
 payments { balance billingAddress { fullName street1 street2 street3 cityStateZip daytimePhone } }
}`}, false)
	if err != nil {
		return check, err
	}
	var response struct {
		Errors json.RawMessage `json:"errors"`
		Data   struct {
			Account  struct{ AppleID, Country string } `json:"account"`
			Payments struct {
				Balance string            `json:"balance"`
				Billing map[string]string `json:"billingAddress"`
			} `json:"payments"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil || (len(response.Errors) != 0 && string(response.Errors) != "null" && string(response.Errors) != "[]") {
		return check, giftFailure("account-response")
	}
	if !strings.EqualFold(strings.TrimSpace(response.Data.Account.AppleID), a.appleID) {
		return check, giftFailure("account-binding")
	}
	country := strings.ToUpper(strings.TrimSpace(response.Data.Account.Country))
	if country != "US" && country != "USA" && country != "UNITED STATES" {
		return check, giftFailure("account-country")
	}
	var ok bool
	check.BalanceMinor, ok = giftUSDMinor(response.Data.Payments.Balance)
	if !ok {
		return check, giftFailure("balance-format")
	}
	// Existing values stay on Apple. No update request is made. These are only
	// readiness hints, not a substitute for Apple's region-specific validation.
	for _, key := range []string{"fullName", "street1", "cityStateZip"} {
		if strings.TrimSpace(response.Data.Payments.Billing[key]) == "" {
			check.MissingFields = append(check.MissingFields, key)
		}
	}
	return check, nil
}

// PrepareGiftCard only reads account/code details. The declared card country
// must match the authenticated Store; it never changes the account's region.
func (s *StoreCommerceSession) PrepareGiftCard(ctx context.Context, appleID, country, code string) (*GiftCardAttempt, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if s == nil || s.identity == nil || s.identity.Anonymous || !s.MatchesAccount(s.identity.DSID) || s.identity.GUID == "" || appleID == "" {
		return nil, giftFailure("session")
	}
	if country != "US" || strings.Split(s.identity.StoreFront, "-")[0] != "143441" {
		return nil, giftFailure("storefront-mismatch")
	}
	if !giftCodePattern.MatchString(code) {
		return nil, giftFailure("code-format")
	}
	endpoints, err := s.giftEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	a := &GiftCardAttempt{session: s, appleID: appleID, code: code, endpoints: endpoints}
	a.check, err = a.readAccount(ctx)
	if err != nil {
		return nil, err
	}
	data, err := s.giftRequest(ctx, endpoints["codeDetails"], map[string]string{"code": code, "requestType": "PreRedeem"}, false)
	if err != nil {
		return nil, err
	}
	var details struct {
		HasError   *bool  `json:"hasError"`
		ShouldFail bool   `json:"shouldFailOnError"`
		ErrorKey   string `json:"errorMessageKey"`
		Options    []struct {
			Action  string `json:"redeemActionType"`
			Account string `json:"accountToUse"`
		} `json:"redeemableOptions"`
	}
	if json.Unmarshal(data, &details) != nil || details.HasError == nil || *details.HasError || details.ShouldFail || details.ErrorKey != "" || len(details.Options) != 1 || details.Options[0].Action != "RedeemCodeSrv" || (details.Options[0].Account != "" && details.Options[0].Account != "Media" && details.Options[0].Account != "iCloud") {
		return nil, giftFailure("preflight-rejected-or-unknown")
	}
	return a, nil
}

func (a *GiftCardAttempt) Check() GiftCardCheck {
	check := a.check
	check.MissingFields = append([]string(nil), check.MissingFields...)
	return check
}

// Submit never follows replay, national-ID, terms, or payment dialogs. Such
// responses need their own verified continuation before resuming this flow.
func (a *GiftCardAttempt) Submit(ctx context.Context) (*GiftCardResult, error) {
	if a == nil || a.session == nil || len(a.check.MissingFields) != 0 {
		return nil, giftFailure("billing-profile-required")
	}
	if !a.submitted.CompareAndSwap(false, true) {
		return nil, giftFailure("already-submitted")
	}
	code := a.code
	a.code = ""
	data, submitErr := a.session.giftRequest(ctx, a.endpoints["redeemCodeSrv"], map[string]any{
		"code": code, "guid": a.session.identity.GUID, "response-content-type": "application/json",
		"isSplitAccount": false, "cameraRecognizedCode": false,
	}, false)
	result := &GiftCardResult{State: "unknown", BalanceMinor: a.check.BalanceMinor}
	// A fresh read is required even after timeout; neither HTTP 200 nor an
	// increased balance alone proves this particular code was redeemed.
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	after, readErr := a.readAccount(verifyCtx)
	if readErr == nil {
		result.BalanceMinor = after.BalanceMinor
	}
	if submitErr != nil {
		return result, submitErr
	}
	var receipt struct {
		Status   *int            `json:"status"`
		Email    string          `json:"email"`
		ErrorKey string          `json:"errorMessageKey"`
		Failure  json.RawMessage `json:"failureType"`
		Dialog   json.RawMessage `json:"dialog"`
		Redeemed struct {
			Money string `json:"money"`
		} `json:"redeemedCredit"`
		Total struct {
			Money string `json:"money"`
		} `json:"totalCredit"`
	}
	if json.Unmarshal(data, &receipt) != nil || receipt.ErrorKey != "" || len(receipt.Failure) != 0 || (len(receipt.Dialog) != 0 && string(receipt.Dialog) != "null") || receipt.Status == nil || *receipt.Status != 0 || !strings.EqualFold(receipt.Email, a.appleID) {
		return result, giftFailure("receipt-rejected-or-unknown")
	}
	credit, creditOK := giftUSDMinor(receipt.Redeemed.Money)
	total, totalOK := giftUSDMinor(receipt.Total.Money)
	if readErr != nil || !creditOK || !totalOK || credit <= 0 || total != after.BalanceMinor || after.BalanceMinor-a.check.BalanceMinor != credit {
		return result, giftFailure("credit-not-verified")
	}
	result.State, result.CreditMinor = "credited", credit
	return result, nil
}
