package appleweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ICloudSessionValidator struct {
	httpClient *http.Client
}

type validateResult struct {
	AppleID            string
	DSID               string
	ClientID           string
	ClientBuildNumber  string
	MasteringNumber    string
	PremiumMailBaseURL string
	MailGatewayBaseURL string
	MailBaseURL        string
	IsICloudPlus       bool
	CanCreateHME       bool
	Cookies            []SessionCookie
}

func NewICloudSessionValidator() *ICloudSessionValidator {
	return &ICloudSessionValidator{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *ICloudSessionValidator) Validate(ctx context.Context, cookies []SessionCookie, defaultHost string) (validateResult, error) {
	host := strings.TrimSpace(defaultHost)
	if host == "" {
		host = "www.icloud.com.cn"
	}
	setupHost := "setup.icloud.com.cn"
	if strings.HasSuffix(host, "icloud.com") && !strings.HasSuffix(host, "icloud.com.cn") {
		setupHost = "setup.icloud.com"
	}

	clientID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	buildNumber := "2622Build20"
	masteringNumber := buildNumber

	u := url.URL{
		Scheme: "https",
		Host:   setupHost,
		Path:   "/setup/ws/1/validate",
	}
	q := u.Query()
	q.Set("clientBuildNumber", buildNumber)
	q.Set("clientMasteringNumber", masteringNumber)
	q.Set("clientId", clientID)
	q.Set("requestId", requestID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return validateResult{}, err
	}
	session := ICloudSession{Host: host, Cookies: cookies}
	setICloudFetchHeaders(req, session, "*/*", "text/plain;charset=UTF-8")
	if cookie := cookieHeader(cookies, u.String()); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return validateResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return validateResult{}, err
	}
	fmt.Fprintln(os.Stderr, iCloudValidationDiagnostic(req, resp, data))
	if redirect, ok := parseAppleDomainRedirect(resp.StatusCode, data); ok {
		return validateResult{}, redirect
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("iCloud 登录态校验失败（HTTP %d）", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusMisdirectedRequest {
			message += "：Apple 未接受当前网页登录态。请重新发起旧接口登录；若仍失败，请检查 iCloud 官网登录是否正常。"
		}
		// Apple validation errors can contain usable trustTokens. Never return the raw body to the UI.
		return validateResult{}, errCode("icloud_validate_failed", message, true)
	}
	var account appleAccountInfo
	if err := json.Unmarshal(data, &account); err != nil {
		return validateResult{}, errCode("icloud_validate_bad_response", "iCloud 登录态校验返回无法解析", true)
	}
	if err := iCloudAccountActionError(account, host); err != nil {
		return validateResult{}, err
	}
	if (account.Success != nil && !*account.Success) || account.HsaChallengeRequired || strings.TrimSpace(account.DSInfo.DSID) == "" {
		return validateResult{}, errCode("icloud_validate_incomplete", "Apple 返回的 iCloud 网页登录态尚未完整通过校验，请完成该账号的网页登录验证后重新登录旧接口。", true)
	}
	refreshed := append([]SessionCookie(nil), cookies...)
	mergeSessionCookies(&refreshed, req.URL, resp.Cookies())
	premium := account.Webservices["premiummailsettings"].URL
	mailGateway := account.Webservices["mccgateway"].URL
	mail := account.Webservices["mail"].URL
	appleID := account.DSInfo.AppleID
	if appleID == "" {
		appleID = account.DSInfo.PrimaryEmail
	}
	return validateResult{
		AppleID:            appleID,
		DSID:               account.DSInfo.DSID,
		ClientID:           clientID,
		ClientBuildNumber:  buildNumber,
		MasteringNumber:    masteringNumber,
		PremiumMailBaseURL: premium,
		MailGatewayBaseURL: mailGateway,
		MailBaseURL:        mail,
		IsICloudPlus:       account.DSInfo.IsHideMyEmailSubscriptionActive,
		CanCreateHME:       account.DSInfo.IsHideMyEmailFeatureAvailable,
		Cookies:            refreshed,
	}, nil
}

// Record only fixed flags and cookie presence, never cookie values or upstream response bodies.
func iCloudValidationDiagnostic(req *http.Request, resp *http.Response, data []byte) string {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	flag := func(key string) string {
		var value bool
		if json.Unmarshal(root[key], &value) == nil {
			return fmt.Sprint(value)
		}
		return "unknown"
	}
	var account map[string]json.RawMessage
	_ = json.Unmarshal(root["dsInfo"], &account)
	var services map[string]json.RawMessage
	_ = json.Unmarshal(root["webservices"], &services)
	var trustTokens []json.RawMessage
	_ = json.Unmarshal(root["trustTokens"], &trustTokens)
	hasCookie := func(name string) bool {
		cookie, err := req.Cookie(name)
		return err == nil && cookie.Value != ""
	}
	redirectHost := ""
	if redirect, ok := parseAppleDomainRedirect(resp.StatusCode, data); ok {
		redirectHost = redirect.Host
	}
	return fmt.Sprintf("iCloud校验诊断 time=%s host=%s status=%d redirect_host=%q client=%s cookies_sent=%t webauth_token=%t webauth_validate=%t webauth_user=%t unique_client=%t response_cookies=%d success=%s trusted_browser=%s challenge_required=%s terms_update_needed=%s repair_needed=%s dsinfo_present=%t webservices=%d trust_tokens_present=%t",
		time.Now().UTC().Format(time.RFC3339), req.URL.Host, resp.StatusCode, redirectHost, appleDebugFingerprint(req.URL.Query().Get("clientId")), req.Header.Get("Cookie") != "",
		hasCookie("X-APPLE-WEBAUTH-TOKEN"), hasCookie("X-APPLE-WEBAUTH-VALIDATE"), hasCookie("X-APPLE-WEBAUTH-USER"), hasCookie("X-APPLE-UNIQUE-CLIENT-ID"), len(resp.Cookies()),
		flag("success"), flag("hsaTrustedBrowser"), flag("hsaChallengeRequired"), flag("termsUpdateNeeded"), flag("isRepairNeeded"), len(account) > 0, len(services), len(trustTokens) > 0)
}
