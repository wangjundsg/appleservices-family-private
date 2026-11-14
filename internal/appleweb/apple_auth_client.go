package appleweb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appleAuthUserAgent              = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3.1 Safari/605.1.15"
	defaultAppleOAuthClientID       = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	appleAccountManageOAuthClientID = "af1139274f266b22b68c2a3e7ad932cb3c0bbe854e13a79af78dcc73136882c3"
	appleHashcashMaxBits            = 24
	appleHashcashMaxAttempts        = 1 << 24
)

const (
	appleTwoFactorMethodTrustedDevice = "trusted_device"
	appleTwoFactorMethodPhone         = "phone"
)

func normalizeAppleTwoFactorMethod(method string) string {
	if method == "probe" {
		return "probe"
	}
	switch strings.ToLower(strings.TrimSpace(method)) {
	case appleTwoFactorMethodPhone, "sms", "phone_sms", "trusted_phone", "trusted_phone_number":
		return appleTwoFactorMethodPhone
	default:
		return appleTwoFactorMethodTrustedDevice
	}
}

type AppleAuthClient struct {
	httpClient *http.Client
}

type appleAuthEndpoints struct {
	Home  string
	Setup string
	Auth  string
	Host  string
}

type appleAuthSession struct {
	mu                  sync.Mutex
	Endpoints           appleAuthEndpoints
	AppleID             string
	ClientID            string
	FrameID             string
	UserAgent           string
	SessionToken        string
	Scnt                string
	ManageScnt          string
	SessionID           string
	AccountCountry      string
	TrustToken          string
	AuthAttributes      string
	HCBits              int
	HCChallenge         string
	CompleteHCBits      int
	CompleteHCChallenge string
	TwoFactorPhone      json.RawMessage
	TwoFactorMethod     string
	TwoFactorVerified   bool
	TrustEstablished    bool
	TermsRequired       bool
	TermsLocale         string
	Terms               *ICloudTerms
	TermsAccepted       bool
	Cookies             []SessionCookie
}

type appleSRPInitResponse struct {
	Iteration int    `json:"iteration"`
	Salt      string `json:"salt"`
	Protocol  string `json:"protocol"`
	B         string `json:"b"`
	C         string `json:"c"`
}

type appleAuthStartResult struct {
	Session    ICloudSession
	PendingID  string
	Needs2FA   bool
	NeedsTerms bool
	Message    string
	AppleID    string
	ExpiresAt  time.Time
}

type appleDomainRedirectError struct {
	DomainToUse string
	Host        string
}

func (e appleDomainRedirectError) Error() string {
	if e.Host != "" {
		return "Apple 要求切换 iCloud 域：" + e.Host
	}
	return "Apple 要求切换 iCloud 域：" + e.DomainToUse
}

type appleAccountInfo struct {
	DSInfo struct {
		DSID                            string `json:"dsid"`
		AppleID                         string `json:"appleId"`
		PrimaryEmail                    string `json:"primaryEmail"`
		IsHideMyEmailSubscriptionActive bool   `json:"isHideMyEmailSubscriptionActive"`
		IsHideMyEmailFeatureAvailable   bool   `json:"isHideMyEmailFeatureAvailable"`
		HsaVersion                      int    `json:"hsaVersion"`
		LanguageCode                    string `json:"languageCode"`
	} `json:"dsInfo"`
	Webservices map[string]struct {
		URL         string `json:"url"`
		Status      string `json:"status"`
		PcsRequired bool   `json:"pcsRequired"`
	} `json:"webservices"`
	HsaChallengeRequired bool  `json:"hsaChallengeRequired"`
	TermsUpdateNeeded    bool  `json:"termsUpdateNeeded"`
	IsRepairNeeded       bool  `json:"isRepairNeeded"`
	Success              *bool `json:"success"`
}

// An HTTP 200 accountLogin can still contain only a partial web session.
func iCloudAccountActionError(account appleAccountInfo, host string) error {
	home := appleAuthEndpointsForHost(host).Home
	if account.TermsUpdateNeeded {
		return errCode("icloud_terms_update_needed", "Apple 要求确认此账号的 iCloud 条款，请在 "+home+" 登录并完成条款确认，再重新登录旧接口。", false)
	}
	if account.IsRepairNeeded {
		return errCode("icloud_account_repair_needed", "Apple 要求完成此账号的 iCloud 设置，请在 "+home+" 检查并完成提示的账号设置，再重新登录旧接口。", false)
	}
	return nil
}

type appleAuthPending struct {
	ID        string
	Session   *appleAuthSession
	CreatedAt time.Time
	ExpiresAt time.Time
}

type appleAuthPendingStore struct {
	mu    sync.Mutex
	items map[string]appleAuthPending
}

func NewAppleAuthClient() *AppleAuthClient {
	return &AppleAuthClient{httpClient: &http.Client{Timeout: 30 * time.Second}}
}

func newAppleAuthPendingStore() *appleAuthPendingStore {
	return &appleAuthPendingStore{items: make(map[string]appleAuthPending)}
}

func (s *appleAuthPendingStore) put(session *appleAuthSession) (appleAuthPending, error) {
	id, err := randomToken(18)
	if err != nil {
		return appleAuthPending{}, err
	}
	now := time.Now()
	pending := appleAuthPending{
		ID:        id,
		Session:   session,
		CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	s.items[id] = pending
	return pending, nil
}

func (s *appleAuthPendingStore) get(id string) (appleAuthPending, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	pending, ok := s.items[strings.TrimSpace(id)]
	return pending, ok
}

func (s *appleAuthPendingStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, strings.TrimSpace(id))
}

func (s *appleAuthPendingStore) cleanupLocked(now time.Time) {
	for id, pending := range s.items {
		if now.After(pending.ExpiresAt) {
			delete(s.items, id)
		}
	}
}

func (c *AppleAuthClient) StartLogin(ctx context.Context, appleID, password, defaultHost, clientID string, pendingStore *appleAuthPendingStore, twoFactorMethod string) (appleAuthStartResult, error) {
	appleID = strings.ToLower(strings.TrimSpace(appleID))
	if appleID == "" || strings.TrimSpace(password) == "" {
		return appleAuthStartResult{}, errCode("apple_credentials_missing", "缺少 Apple ID 或密码", false)
	}
	method := normalizeAppleTwoFactorMethod(twoFactorMethod)
	result, err := c.startLoginOnHost(ctx, appleID, password, defaultHost, clientID, pendingStore, method)
	if err == nil {
		return result, nil
	}
	var redirect appleDomainRedirectError
	if !errors.As(err, &redirect) || redirect.Host == "" {
		return appleAuthStartResult{}, err
	}
	currentHost := appleAuthEndpointsForHost(defaultHost).Host
	nextHost := appleAuthEndpointsForHost(redirect.Host).Host
	if currentHost == nextHost {
		return appleAuthStartResult{}, err
	}
	return c.startLoginOnHost(ctx, appleID, password, nextHost, clientID, pendingStore, method)
}

func (c *AppleAuthClient) startLoginOnHost(ctx context.Context, appleID, password, host, clientID string, pendingStore *appleAuthPendingStore, twoFactorMethod string) (appleAuthStartResult, error) {
	frameID, err := randomUUID()
	if err != nil {
		return appleAuthStartResult{}, err
	}
	session := &appleAuthSession{
		Endpoints: appleAuthEndpointsForHost(host),
		AppleID:   appleID,
		ClientID:  firstNonEmpty(clientID, defaultAppleOAuthClientID),
		FrameID:   strings.ToLower(frameID),
	}
	if err := c.authStart(ctx, session); err != nil {
		return appleAuthStartResult{}, err
	}
	if err := c.authFederate(ctx, session); err != nil {
		return appleAuthStartResult{}, err
	}
	needs2FA, err := c.authSRP(ctx, session, password)
	if err != nil {
		return appleAuthStartResult{}, err
	}
	// Reconciliation authenticates once but never requests another verification code.
	if twoFactorMethod == "probe" {
		return appleAuthStartResult{AppleID: session.AppleID, Needs2FA: needs2FA}, nil
	}
	if redirect, ok := session.redirectForAccountCountry(); ok {
		return appleAuthStartResult{}, redirect
	}
	if session.SessionToken == "" {
		return appleAuthStartResult{}, errCode("apple_session_token_missing", "Apple 登录未返回 Session Token，请重新协议登录或检查账号安全状态", true)
	}
	if needs2FA {
		session.TwoFactorMethod = normalizeAppleTwoFactorMethod(twoFactorMethod)
		message := "已触发 Apple 2FA，请在受信任设备允许后输入 6 位验证码"
		if session.TwoFactorMethod == appleTwoFactorMethodPhone {
			_ = c.refreshAuthState(ctx, session)
			if err := c.requestPhoneSecurityCode(ctx, session, nil); err != nil {
				return appleAuthStartResult{}, err
			}
			message = "已向受信任手机号发送短信验证码，请输入 6 位验证码"
		} else if err := c.requestTrustedDeviceCode(ctx, session); err != nil {
			var redirect appleDomainRedirectError
			if errors.As(err, &redirect) {
				return appleAuthStartResult{}, err
			}
			message = "Apple 已要求 2FA；自动触发验证码未确认，请查看受信任设备后输入验证码"
		}
		pending, err := pendingStore.put(session)
		if err != nil {
			return appleAuthStartResult{}, err
		}
		return appleAuthStartResult{
			PendingID: pending.ID,
			Needs2FA:  true,
			Message:   message,
			AppleID:   strings.TrimSpace(appleID),
			ExpiresAt: pending.ExpiresAt,
		}, nil
	}

	icloudSession, err := c.authWithTokenAndValidate(ctx, session)
	if err != nil {
		var redirect appleDomainRedirectError
		if errors.As(err, &redirect) {
			return appleAuthStartResult{}, err
		}
		code, _, _ := ErrorDetails(err)
		if code == "icloud_terms_update_needed" {
			pending, putErr := pendingStore.put(session)
			if putErr != nil {
				return appleAuthStartResult{}, putErr
			}
			return appleAuthStartResult{PendingID: pending.ID, NeedsTerms: true, AppleID: session.AppleID, ExpiresAt: pending.ExpiresAt, Message: "请阅读并确认此账号的 iCloud 条款"}, nil
		}
		if code == "icloud_account_repair_needed" {
			return appleAuthStartResult{}, err
		}
		pending, putErr := pendingStore.put(session)
		if putErr != nil {
			return appleAuthStartResult{}, putErr
		}
		return appleAuthStartResult{
			PendingID: pending.ID,
			Needs2FA:  true,
			Message:   "登录已进入二次验证；如果设备已弹码，请输入验证码继续",
			AppleID:   strings.TrimSpace(appleID),
			ExpiresAt: pending.ExpiresAt,
		}, nil
	}
	return appleAuthStartResult{
		Session:  icloudSession,
		Needs2FA: false,
		Message:  "Apple 协议登录成功，登录态已生成",
		AppleID:  strings.TrimSpace(appleID),
	}, nil
}

func (c *AppleAuthClient) Submit2FA(ctx context.Context, pending appleAuthPending, code string, phoneNumber json.RawMessage) (ICloudSession, error) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return ICloudSession{}, errCode("invalid_2fa_code", "2FA 验证码必须是 6 位", false)
	}
	session := pending.Session
	for attempt := 0; attempt < 2; attempt++ {
		icloudSession, err := c.submit2FAWithSession(ctx, session, code, phoneNumber)
		if err == nil {
			return icloudSession, nil
		}
		var redirect appleDomainRedirectError
		if !errors.As(err, &redirect) || !session.switchHost(redirect.Host) {
			return ICloudSession{}, err
		}
	}
	return ICloudSession{}, errCode("apple_domain_switch_failed", "Apple 登录域切换后仍未完成登录，请重新发起协议登录", true)
}

func (c *AppleAuthClient) submit2FAWithSession(ctx context.Context, session *appleAuthSession, code string, phoneNumber json.RawMessage) (ICloudSession, error) {
	if err := c.verify2FAWithSession(ctx, session, code, phoneNumber); err != nil {
		return ICloudSession{}, err
	}
	if !session.TrustEstablished {
		if err := c.trustSession(ctx, session); err != nil {
			return ICloudSession{}, err
		}
		session.TrustEstablished = true
	}
	return c.authWithTokenAndValidate(ctx, session)
}

func (c *AppleAuthClient) verify2FAWithSession(ctx context.Context, session *appleAuthSession, code string, phoneNumber json.RawMessage) error {
	if session.TwoFactorVerified {
		return nil
	}
	var err error
	if session.TwoFactorMethod == appleTwoFactorMethodPhone {
		err = c.validatePhoneSecurityCode(ctx, session, code, phoneNumber)
	} else {
		err = c.validateTrustedDeviceCode(ctx, session, code)
	}
	if err != nil {
		return err
	}
	// A setup-domain redirect or later retry must never replay an accepted OTP.
	session.TwoFactorVerified = true
	return nil
}

func appleAuthEndpointsForHost(host string) appleAuthEndpoints {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, "icloud.com.cn") {
		return appleAuthEndpoints{
			Home:  "https://www.icloud.com.cn",
			Setup: "https://setup.icloud.com.cn/setup/ws/1",
			Auth:  "https://idmsa.apple.com.cn/appleauth/auth",
			Host:  "www.icloud.com.cn",
		}
	}
	return appleAuthEndpoints{
		Home:  "https://www.icloud.com",
		Setup: "https://setup.icloud.com/setup/ws/1",
		Auth:  "https://idmsa.apple.com/appleauth/auth",
		Host:  "www.icloud.com",
	}
}

func appleAccountManageAuthEndpoints() appleAuthEndpoints {
	return appleAuthEndpoints{
		Home: "https://account.apple.com",
		Auth: "https://idmsa.apple.com/appleauth/auth",
		Host: "appleid.apple.com",
	}
}

func appleDomainToHost(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimPrefix(domain, "www.")
	domain = strings.TrimSuffix(domain, "/")
	if strings.Contains(domain, "icloud.com.cn") {
		return "www.icloud.com.cn"
	}
	if strings.Contains(domain, "icloud.com") {
		return "www.icloud.com"
	}
	return ""
}

func appleHostForAccountCountry(country string) string {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return ""
	}
	if country == "CHN" || country == "CN" {
		return "www.icloud.com.cn"
	}
	return "www.icloud.com"
}

func parseAppleDomainRedirect(status int, data []byte) (appleDomainRedirectError, bool) {
	if status < 300 || status >= 400 {
		return appleDomainRedirectError{}, false
	}
	var payload struct {
		DomainToUse string `json:"domainToUse"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &payload); err != nil {
		return appleDomainRedirectError{}, false
	}
	host := appleDomainToHost(payload.DomainToUse)
	if host == "" {
		return appleDomainRedirectError{}, false
	}
	return appleDomainRedirectError{DomainToUse: payload.DomainToUse, Host: host}, true
}

func (c *AppleAuthClient) authStart(ctx context.Context, session *appleAuthSession) error {
	frameTag := "auth-" + session.FrameID
	u, err := url.Parse(session.Endpoints.Auth + "/authorize/signin")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("frame_id", frameTag)
	q.Set("skVersion", "7")
	q.Set("iframeId", frameTag)
	q.Set("client_id", session.ClientID)
	q.Set("redirect_uri", session.Endpoints.Home)
	q.Set("response_type", "code")
	q.Set("response_mode", "web_message")
	q.Set("state", frameTag)
	headers := map[string]string{"Accept": "*/*"}
	if session.isAppleAccountManage() {
		q.Set("authVersion", "8.0.2")
		headers["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
		headers["Referer"] = session.Endpoints.Home + "/"
		applyAppleAccountBrowserHints(headers)
		headers["Sec-Fetch-Dest"] = "iframe"
		headers["Sec-Fetch-Mode"] = "navigate"
		headers["Sec-Fetch-Site"] = "same-site"
	} else {
		q.Set("language", strings.ReplaceAll(langValue(), "-", "_"))
		q.Set("authVersion", "latest")
	}
	u.RawQuery = q.Encode()
	_, _, err = c.do(ctx, session, http.MethodGet, u.String(), headers, nil, nil, false)
	if err != nil {
		return err
	}
	if session.isAppleAccountManage() {
		session.rememberCompleteHashcashChallenge()
	}
	return nil
}

func (c *AppleAuthClient) authDeviceKeyChallenge(ctx context.Context, session *appleAuthSession) error {
	if !session.isAppleAccountManage() {
		return nil
	}
	body := map[string]bool{"passkeyAutofill": false}
	headers := session.srpHeaders()
	delete(headers, "scnt")
	delete(headers, "X-Apple-ID-Session-Id")
	delete(headers, "X-Apple-App-Id")
	_, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/verify/device/key/challenge", headers, body, nil, false)
	return err
}

func (c *AppleAuthClient) authFederate(ctx context.Context, session *appleAuthSession) error {
	u := session.Endpoints.Auth + "/federate?isRememberMeEnabled=true"
	body := map[string]any{"accountName": session.AppleID, "rememberMe": true}
	_, _, err := c.do(ctx, session, http.MethodPost, u, session.srpHeaders(), body, nil, false)
	return err
}

func (c *AppleAuthClient) authSRP(ctx context.Context, session *appleAuthSession, password string) (bool, error) {
	srp, err := newSRPClient()
	if err != nil {
		return false, err
	}
	var initResp appleSRPInitResponse
	initBody := map[string]any{
		"a":           base64.StdEncoding.EncodeToString(srp.ABytes()),
		"accountName": session.AppleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	}
	if _, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/signin/init", session.srpHeaders(), initBody, &initResp, false); err != nil {
		return false, err
	}
	serverB, err := base64.StdEncoding.DecodeString(initResp.B)
	if err != nil {
		return false, fmt.Errorf("解析 SRP B 失败：%w", err)
	}
	salt, err := base64.StdEncoding.DecodeString(initResp.Salt)
	if err != nil {
		return false, fmt.Errorf("解析 SRP salt 失败：%w", err)
	}
	derived, err := deriveAppleSRPPassword(password, salt, initResp.Iteration, initResp.Protocol)
	if err != nil {
		return false, err
	}
	if err := srp.processChallenge([]byte(session.AppleID), derived, salt, serverB); err != nil {
		return false, err
	}
	completeBody := map[string]any{
		"accountName": session.AppleID,
		"m1":          base64.StdEncoding.EncodeToString(srp.M1),
		"m2":          base64.StdEncoding.EncodeToString(srp.M2),
		"c":           initResp.C,
		"rememberMe":  true,
	}
	if !session.isAppleAccountManage() {
		completeBody["trustTokens"] = []string{}
		if session.TrustToken != "" {
			completeBody["trustTokens"] = []string{session.TrustToken}
		}
	}
	headers := session.srpHeaders()
	if session.isAppleAccountManage() {
		bits, challenge := session.completeHashcashChallenge()
		hc, err := generateAppleHashcash(bits, challenge, time.Now())
		if err != nil {
			return false, err
		}
		headers["X-Apple-HC"] = hc
	}
	status, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/signin/complete?isRememberMeEnabled=true", headers, completeBody, nil, true)
	if status == http.StatusUnauthorized {
		return false, errCode("apple_credentials_invalid", "Apple ID 或密码错误，请检查后重新协议登录", false)
	}
	return status == http.StatusConflict, err
}

func generateAppleHashcash(bits int, challenge string, now time.Time) (string, error) {
	challenge = strings.TrimSpace(challenge)
	if bits <= 0 || challenge == "" {
		return "", errCode("apple_hc_missing", "Apple Account 缺少动态验证挑战，请重新协议登录", true)
	}
	if bits > appleHashcashMaxBits {
		return "", errCode("apple_hc_too_hard", "Apple Account 动态验证难度过高，请稍后重试", true)
	}
	prefix := fmt.Sprintf("1:%d:%s:%s::", bits, now.UTC().Format("20060102150405"), challenge)
	for counter := int64(0); counter < appleHashcashMaxAttempts; counter++ {
		value := prefix + strconv.FormatInt(counter, 36)
		sum := sha1.Sum([]byte(value))
		if leadingZeroBits(sum[:]) >= bits {
			return value, nil
		}
	}
	return "", errCode("apple_hc_failed", "Apple Account 动态验证生成失败，请稍后重试", true)
}

func leadingZeroBits(data []byte) int {
	total := 0
	for _, b := range data {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) != 0 {
				return total
			}
			total++
		}
	}
	return total
}

func (c *AppleAuthClient) requestTrustedDeviceCode(ctx context.Context, session *appleAuthSession) error {
	_, _, err := c.do(ctx, session, http.MethodPut, session.Endpoints.Auth+"/verify/trusteddevice/securitycode", session.twoFactorHeaders(), nil, nil, false)
	return err
}

func (c *AppleAuthClient) requestPhoneSecurityCode(ctx context.Context, session *appleAuthSession, phoneNumber json.RawMessage) error {
	phone, err := appleAccountPhoneNumberPayload(appleAccountFallbackPhoneNumber(phoneNumber, session.TwoFactorPhone), false)
	if err != nil {
		return err
	}
	body := map[string]any{
		"phoneNumber": phone,
		"mode":        "sms",
	}
	status, data, err := c.doWithAllowedStatuses(ctx, session, http.MethodPut, session.Endpoints.Auth+"/verify/phone", session.twoFactorHeaders(), body, nil, false, http.StatusPreconditionFailed)
	remembered := false
	if err == nil {
		remembered = session.rememberTwoFactorPhoneNumber(data, phone["id"])
	}
	if status == http.StatusPreconditionFailed && err == nil {
		// Apple 某些账号发送短信后返回 412，并附带 trustedPhoneNumbers；这代表验证码已触发，不是登录失败。
		if !remembered {
			return errCode("apple_phone_code_request_failed", "Apple 返回了 412，但未识别到受信任手机号信息，请重新发起登录", true)
		}
	}
	return err
}

func (c *AppleAuthClient) refreshAuthState(ctx context.Context, session *appleAuthSession) error {
	headers := session.authHeaders()
	headers["Accept"] = "text/html"
	headers["Referer"] = strings.TrimSuffix(session.Endpoints.Auth, "/appleauth/auth") + "/"
	headers["Sec-Fetch-Dest"] = "empty"
	headers["Sec-Fetch-Mode"] = "cors"
	headers["Sec-Fetch-Site"] = "same-origin"
	delete(headers, "Origin")
	delete(headers, "X-Apple-App-Id")
	_, data, err := c.do(ctx, session, http.MethodGet, session.Endpoints.Auth, headers, nil, nil, false)
	if err == nil {
		session.rememberTwoFactorPhoneNumber(data)
	}
	return err
}

func (c *AppleAuthClient) validateTrustedDeviceCode(ctx context.Context, session *appleAuthSession, code string) error {
	body := map[string]any{"securityCode": map[string]string{"code": code}}
	_, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/verify/trusteddevice/securitycode", session.twoFactorHeaders(), body, nil, false)
	if err != nil {
		var redirect appleDomainRedirectError
		if errors.As(err, &redirect) {
			return err
		}
		return errCode("apple_2fa_failed", "Apple 2FA 验证失败："+err.Error(), true)
	}
	return nil
}

func (c *AppleAuthClient) validatePhoneSecurityCode(ctx context.Context, session *appleAuthSession, code string, phoneNumber json.RawMessage) error {
	phone, err := appleAccountPhoneNumberPayload(appleAccountFallbackPhoneNumber(phoneNumber, session.TwoFactorPhone), true)
	if err != nil {
		return err
	}
	body := map[string]any{
		"phoneNumber":  phone,
		"securityCode": map[string]string{"code": code},
		"mode":         "sms",
	}
	status, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Auth+"/verify/phone/securitycode", session.twoFactorHeaders(), body, nil, false)
	if err != nil {
		flow := "icloud_web"
		if session.isAppleAccountManage() {
			flow = "apple_account"
		}
		fmt.Fprintf(os.Stderr, "Apple短信验证失败 flow=%s status=%d phone_id=%v\n", flow, status, phone["id"])
		if status == http.StatusUnauthorized {
			return errCode("apple_2fa_failed", "Apple 拒绝了本次短信验证（HTTP 401）。请取消后重新发起登录，使用本次登录收到的新短信验证码；仍失败时可改用受信任设备验证。", true)
		}
		return errCode("apple_2fa_failed", "Apple 短信 2FA 验证失败："+err.Error(), true)
	}
	return nil
}

func appleAccountFallbackPhoneNumber(phoneNumber json.RawMessage, fallbacks ...json.RawMessage) json.RawMessage {
	phoneNumber = bytes.TrimSpace(phoneNumber)
	if len(phoneNumber) > 0 && string(phoneNumber) != "null" {
		return phoneNumber
	}
	for _, fallback := range fallbacks {
		fallback = bytes.TrimSpace(fallback)
		if len(fallback) > 0 && string(fallback) != "null" {
			return fallback
		}
	}
	return json.RawMessage(`{"id":1,"nonFTEU":true}`)
}

func appleAccountPhoneNumberPayload(phoneNumber json.RawMessage, includeNonFTEU bool) (map[string]any, error) {
	var phone map[string]any
	if err := json.Unmarshal(phoneNumber, &phone); err != nil {
		return nil, errCode("invalid_phone_number_payload", "短信验证码缺少有效 phoneNumber 参数", false)
	}
	if _, ok := phone["id"]; !ok {
		return nil, errCode("invalid_phone_number_payload", "短信验证码缺少有效 phoneNumber 参数", false)
	}
	if !includeNonFTEU {
		delete(phone, "nonFTEU")
	}
	return phone, nil
}

func (s *appleAuthSession) rememberTwoFactorPhoneNumber(data []byte, preferredID ...any) bool {
	if s == nil {
		return false
	}
	payload := extractAppleAppConfigJSON(data)
	if len(payload) == 0 {
		payload = bytes.TrimSpace(data)
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return false
	}
	if phone, ok := firstApplePhoneNumber(root, 0, preferredID...); ok {
		if data, err := json.Marshal(phone); err == nil {
			s.TwoFactorPhone = data
			return true
		}
	}
	return false
}

func extractAppleAppConfigJSON(data []byte) []byte {
	text := string(data)
	for _, marker := range []string{`id="app_config"`, `id='app_config'`} {
		idx := strings.Index(text, marker)
		if idx < 0 {
			continue
		}
		open := strings.Index(text[idx:], ">")
		if open < 0 {
			continue
		}
		start := idx + open + 1
		end := strings.Index(text[start:], "</script>")
		if end < 0 {
			continue
		}
		return []byte(strings.TrimSpace(text[start : start+end]))
	}
	return nil
}

func firstApplePhoneNumber(value any, depth int, preferredID ...any) (map[string]any, bool) {
	if depth > 16 {
		return nil, false
	}
	switch v := value.(type) {
	case map[string]any:
		if phone, ok := normalizeApplePhoneNumber(v["phoneNumber"]); ok {
			return phone, true
		}
		if numbers, ok := v["trustedPhoneNumbers"].([]any); ok {
			if len(preferredID) > 0 {
				for _, item := range numbers {
					if phone, ok := normalizeApplePhoneNumber(item); ok && fmt.Sprint(phone["id"]) == fmt.Sprint(preferredID[0]) {
						return phone, true
					}
				}
			}
			for _, item := range numbers {
				if phone, ok := normalizeApplePhoneNumber(item); ok {
					return phone, true
				}
			}
		}
		for _, item := range v {
			if phone, ok := firstApplePhoneNumber(item, depth+1, preferredID...); ok {
				return phone, true
			}
		}
	case []any:
		for _, item := range v {
			if phone, ok := firstApplePhoneNumber(item, depth+1, preferredID...); ok {
				return phone, true
			}
		}
	}
	return nil, false
}

func normalizeApplePhoneNumber(value any) (map[string]any, bool) {
	phone, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	id, ok := phone["id"]
	if !ok {
		return nil, false
	}
	out := map[string]any{"id": id}
	if nonFTEU, ok := phone["nonFTEU"]; ok {
		out["nonFTEU"] = nonFTEU
	}
	return out, true
}

func (c *AppleAuthClient) trustSession(ctx context.Context, session *appleAuthSession) error {
	return retryAppleTransient(ctx, func() error {
		_, _, err := c.do(ctx, session, http.MethodGet, session.Endpoints.Auth+"/2sv/trust", session.authHeaders(), nil, nil, false)
		return err
	})
}

func (c *AppleAuthClient) authWithTokenAndValidate(ctx context.Context, session *appleAuthSession) (ICloudSession, error) {
	if session.SessionToken == "" {
		return ICloudSession{}, errCode("apple_session_token_missing", "Apple Session Token 缺失，无法换取 iCloud 登录态", true)
	}
	var account appleAccountInfo
	body := map[string]any{
		"accountCountryCode": session.AccountCountry,
		"dsWebAuthToken":     session.SessionToken,
		"extended_login":     true,
		"trustToken":         session.TrustToken,
	}
	headers := session.commonHeaders(map[string]string{})
	if _, _, err := c.do(ctx, session, http.MethodPost, session.Endpoints.Setup+"/accountLogin", headers, body, &account, false); err != nil {
		return ICloudSession{}, err
	}
	session.TermsRequired = account.TermsUpdateNeeded
	session.TermsLocale = firstNonEmpty(account.DSInfo.LanguageCode, "zh_CN")
	if err := iCloudAccountActionError(account, session.Endpoints.Host); err != nil {
		return ICloudSession{}, err
	}
	cookies := session.cloneCookies()
	validate, err := (&ICloudSessionValidator{httpClient: c.httpClient}).Validate(ctx, cookies, session.Endpoints.Host)
	if err != nil {
		return ICloudSession{}, err
	}
	cookies = validate.Cookies
	savedAt := time.Now()
	return ICloudSession{
		SavedAt:            savedAt,
		AppleID:            firstNonEmpty(validate.AppleID, account.DSInfo.AppleID, account.DSInfo.PrimaryEmail, session.AppleID),
		DSID:               validate.DSID,
		ClientID:           validate.ClientID,
		ClientBuildNumber:  validate.ClientBuildNumber,
		MasteringNumber:    validate.MasteringNumber,
		PremiumMailBaseURL: strings.TrimRight(validate.PremiumMailBaseURL, "/"),
		MailGatewayBaseURL: strings.TrimRight(validate.MailGatewayBaseURL, "/"),
		MailBaseURL:        strings.TrimRight(validate.MailBaseURL, "/"),
		Host:               session.Endpoints.Host,
		IsICloudPlus:       validate.IsICloudPlus,
		CanCreateHME:       validate.CanCreateHME,
		Cookies:            cookies,
		LoginStates: []LoginState{
			{
				Kind:      LoginStateICloudWeb,
				Host:      session.Endpoints.Host,
				Origin:    session.Endpoints.Home,
				SavedAt:   savedAt,
				Cookies:   append([]SessionCookie(nil), cookies...),
				UserAgent: uaValue(),
				Note:      "iCloud webservices login state",
			},
		},
		Note: "saved from Go Apple SRP protocol login",
	}, nil
}

func (c *AppleAuthClient) do(ctx context.Context, session *appleAuthSession, method, rawURL string, headers map[string]string, body any, out any, allow409 bool) (int, []byte, error) {
	return c.doWithAllowedStatuses(ctx, session, method, rawURL, headers, body, out, allow409)
}

func (c *AppleAuthClient) doWithAllowedStatuses(ctx context.Context, session *appleAuthSession, method, rawURL string, headers map[string]string, body any, out any, allow409 bool, allowedStatuses ...int) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}
	userAgent := firstNonEmpty(session.UserAgent, uaValue())
	req.Header.Set("User-Agent", userAgent)
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie := cookieHeader(session.Cookies, rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	session.extract(resp)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if diagnostic := appleSMSExchangeDiagnostic(req, resp.StatusCode, session, body, data); diagnostic != "" {
		fmt.Fprintln(os.Stderr, diagnostic)
	}

	if redirect, ok := parseAppleDomainRedirect(resp.StatusCode, data); ok {
		return resp.StatusCode, data, redirect
	}
	if resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, nil, errCode("apple_login_forbidden", "Apple ID 或密码错误，或当前账号被限制登录", true)
	}
	if allow409 && resp.StatusCode == http.StatusConflict {
		return resp.StatusCode, data, nil
	}
	for _, allowedStatus := range allowedStatuses {
		if resp.StatusCode == allowedStatus {
			return resp.StatusCode, data, nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, errCode("apple_protocol_http_error", fmt.Sprintf("Apple 协议 HTTP %d: %s", resp.StatusCode, trimForError(data)), true)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, nil, errCode("apple_protocol_bad_json", "Apple 协议返回无法解析", true)
		}
	}
	return resp.StatusCode, data, nil
}

// Only emit fixed protocol fields and fingerprints, never authentication bodies or credentials.
func appleSMSExchangeDiagnostic(req *http.Request, status int, session *appleAuthSession, body any, data []byte) string {
	step := ""
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/signin/complete"):
		step = "signin_complete"
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/appleauth/auth"):
		step = "auth_state"
	case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/verify/phone"):
		step = "sms_send"
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/verify/phone/securitycode"):
		step = "sms_verify"
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/2sv/trust"):
		step = "trust"
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/setup/ws/1/accountLogin"):
		step = "account_login"
	default:
		return ""
	}
	phoneFingerprint := func(value any) string {
		phone, ok := value.(map[string]any)
		if !ok || phone["id"] == nil {
			return "-"
		}
		return appleDebugFingerprint(fmt.Sprint(phone["id"]))
	}
	flag := func(value any) string {
		if value, ok := value.(bool); ok {
			return strconv.FormatBool(value)
		}
		return "unknown"
	}
	var sentPhone map[string]any
	if body, ok := body.(map[string]any); ok {
		sentPhone, _ = body["phoneNumber"].(map[string]any)
	}
	var storedPhone map[string]any
	_ = json.Unmarshal(session.TwoFactorPhone, &storedPhone)
	payload := extractAppleAppConfigJSON(data)
	if len(payload) == 0 {
		payload = bytes.TrimSpace(data)
	}
	var reply any
	format := "non_json"
	if len(payload) == 0 {
		format = "empty"
	} else if json.Unmarshal(payload, &reply) == nil {
		format = "json"
	}
	replyPhone, _ := firstApplePhoneNumber(reply, 0)
	root, _ := reply.(map[string]any)
	security, _ := root["securityCode"].(map[string]any)
	if security == nil {
		verification, _ := root["phoneNumberVerification"].(map[string]any)
		security, _ = verification["securityCode"].(map[string]any)
	}
	var serviceCodes []int64
	for _, key := range []string{"service_errors", "serviceErrors"} {
		items, _ := root[key].([]any)
		for _, item := range items {
			item, _ := item.(map[string]any)
			// Apple service error codes are negative numbers; never log arbitrary strings or OTPs.
			if code, err := strconv.ParseInt(fmt.Sprint(item["code"]), 10, 64); err == nil && code < 0 {
				serviceCodes = append(serviceCodes, code)
			}
		}
	}
	flow := "icloud_web"
	if session.isAppleAccountManage() {
		flow = "apple_account"
	}
	redirectHost := ""
	if redirect, ok := parseAppleDomainRedirect(status, data); ok {
		redirectHost = redirect.Host
	}
	diagnostic := fmt.Sprintf("Apple短信诊断 time=%s step=%s trace=%s flow=%s host=%s country_route=%q status=%d redirect_host=%q otp_verified=%t trust_established=%t sent_phone=%s stored_phone=%s reply_phone=%s non_fteu=%s req_scnt=%s next_scnt=%s req_session=%s next_session=%s cookies_sent=%t reply_format=%s valid=%s locked=%s too_many_sent=%s too_many_validated=%s cooldown=%s service_codes=%v",
		time.Now().UTC().Format(time.RFC3339), step, appleDebugFingerprint(session.FrameID), flow, req.URL.Host,
		appleHostForAccountCountry(session.AccountCountry), status, redirectHost, session.TwoFactorVerified, session.TrustEstablished, phoneFingerprint(sentPhone), phoneFingerprint(storedPhone), phoneFingerprint(replyPhone), flag(sentPhone["nonFTEU"]),
		appleDebugFingerprint(req.Header.Get("scnt")), appleDebugFingerprint(session.Scnt), appleDebugFingerprint(req.Header.Get("X-Apple-ID-Session-Id")), appleDebugFingerprint(session.SessionID), req.Header.Get("Cookie") != "", format,
		flag(security["valid"]), flag(security["securityCodeLocked"]), flag(security["tooManyCodesSent"]), flag(security["tooManyCodesValidated"]), flag(security["securityCodeCooldown"]), serviceCodes)
	if step == "account_login" {
		account, _ := root["dsInfo"].(map[string]any)
		services, _ := root["webservices"].(map[string]any)
		cookieRequest, _ := http.NewRequest(http.MethodGet, req.URL.String(), nil)
		cookieRequest.Header.Set("Cookie", cookieHeader(session.Cookies, req.URL.String()))
		hasCookie := func(name string) bool {
			cookie, err := cookieRequest.Cookie(name)
			return err == nil && cookie.Value != ""
		}
		diagnostic += fmt.Sprintf(" success=%s terms_update_needed=%s repair_needed=%s challenge_required=%s trusted_browser=%s dsinfo_present=%t webservices=%d webauth_token=%t webauth_validate=%t webauth_repair=%t ds_web_session=%t",
			flag(root["success"]), flag(root["termsUpdateNeeded"]), flag(root["isRepairNeeded"]), flag(root["hsaChallengeRequired"]), flag(root["hsaTrustedBrowser"]), len(account) > 0, len(services),
			hasCookie("X-APPLE-WEBAUTH-TOKEN"), hasCookie("X-APPLE-WEBAUTH-VALIDATE"), hasCookie("X-APPLE-WEBAUTH-REPAIR"), hasCookie("X-APPLE-DS-WEB-SESSION-TOKEN"))
	}
	return diagnostic
}

func appleDebugFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	sum := sha1.Sum([]byte(value))
	return fmt.Sprintf("%x/%d", sum[:4], len(value))
}

func retryAppleTransient(ctx context.Context, fn func() error) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := fn(); err != nil {
			if !isAppleTransientNetworkError(err) {
				return err
			}
			last = err
			if attempt == 2 {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 800 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		return nil
	}
	return last
}

func isAppleTransientNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var coded codedError
	if errors.As(err, &coded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"eof",
		"timeout",
		"i/o timeout",
		"no such host",
		"temporary failure in name resolution",
		"network is unreachable",
		"no route to host",
		"connection reset",
		"connection refused",
		"connection aborted",
		"server closed idle connection",
		"tls handshake timeout",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (s *appleAuthSession) extract(resp *http.Response) {
	s.mergeCookies(resp.Request.URL, resp.Cookies())
	if v := resp.Header.Get("X-Apple-ID-Account-Country"); v != "" {
		s.AccountCountry = v
	}
	if v := resp.Header.Get("X-Apple-ID-Session-Id"); v != "" {
		s.SessionID = v
	}
	if v := resp.Header.Get("X-Apple-Session-Token"); v != "" {
		s.SessionToken = v
	}
	if v := resp.Header.Get("X-Apple-TwoSV-Trust-Token"); v != "" {
		s.TrustToken = v
	}
	if v := resp.Header.Get("scnt"); v != "" {
		s.Scnt = v
	}
	if v := resp.Header.Get("X-Apple-Auth-Attributes"); v != "" {
		s.AuthAttributes = v
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-HC-Bits")); v != "" {
		if bits, err := strconv.Atoi(v); err == nil && bits > 0 {
			s.HCBits = bits
		}
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-HC-Challenge")); v != "" {
		s.HCChallenge = v
	}
}

func (s *appleAuthSession) rememberCompleteHashcashChallenge() {
	if s == nil || s.CompleteHCBits > 0 || s.CompleteHCChallenge != "" {
		return
	}
	if s.HCBits > 0 && strings.TrimSpace(s.HCChallenge) != "" {
		s.CompleteHCBits = s.HCBits
		s.CompleteHCChallenge = s.HCChallenge
	}
}

func (s *appleAuthSession) completeHashcashChallenge() (int, string) {
	if s == nil {
		return 0, ""
	}
	if s.CompleteHCBits > 0 && strings.TrimSpace(s.CompleteHCChallenge) != "" {
		return s.CompleteHCBits, s.CompleteHCChallenge
	}
	return s.HCBits, s.HCChallenge
}

func (s *appleAuthSession) switchHost(host string) bool {
	next := appleAuthEndpointsForHost(host)
	if next.Host == "" || next.Host == s.Endpoints.Host {
		return false
	}
	s.Endpoints = next
	return true
}

func (s *appleAuthSession) redirectForAccountCountry() (appleDomainRedirectError, bool) {
	host := appleHostForAccountCountry(s.AccountCountry)
	if host == "" {
		return appleDomainRedirectError{}, false
	}
	next := appleAuthEndpointsForHost(host)
	if next.Host == "" || next.Host == s.Endpoints.Host {
		return appleDomainRedirectError{}, false
	}
	domain := "iCloud.com"
	if strings.Contains(next.Host, "icloud.com.cn") {
		domain = "iCloud.com.cn"
	}
	return appleDomainRedirectError{DomainToUse: domain, Host: next.Host}, true
}

func (s *appleAuthSession) mergeCookies(requestURL *url.URL, cookies []*http.Cookie) {
	mergeSessionCookies(&s.Cookies, requestURL, cookies)
}

func (s *appleAuthSession) cloneCookies() []SessionCookie {
	return append([]SessionCookie(nil), s.Cookies...)
}

func (s *appleAuthSession) srpHeaders() map[string]string {
	frameTag := "auth-" + s.FrameID
	origin := strings.TrimSuffix(s.Endpoints.Auth, "/appleauth/auth")
	userAgent := firstNonEmpty(s.UserAgent, uaValue())
	headers := map[string]string{
		"Accept":                           "application/json",
		"Content-Type":                     "application/json",
		"Origin":                           origin,
		"Referer":                          origin + "/",
		"X-Apple-Widget-Key":               s.ClientID,
		"X-Apple-OAuth-Client-Id":          s.ClientID,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       s.Endpoints.Home,
		"X-Apple-OAuth-Require-Grant-Code": "true",
		"X-Apple-OAuth-Response-Mode":      "web_message",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-OAuth-State":              frameTag,
		"X-Apple-Frame-Id":                 frameTag,
		"X-Requested-With":                 "XMLHttpRequest",
		"X-Apple-Mandate-Security-Upgrade": "0",
		"X-Apple-I-Require-UE":             "true",
		"X-Apple-I-FD-Client-Info":         browserClientInfo(userAgent),
	}
	if s.isAppleAccountManage() {
		headers["Accept"] = "application/json, text/javascript, */*; q=0.01"
		applyAppleAccountBrowserHints(headers)
		headers["Sec-Fetch-Dest"] = "empty"
		headers["Sec-Fetch-Mode"] = "cors"
		headers["Sec-Fetch-Site"] = "same-origin"
		delete(headers, "X-Apple-OAuth-Require-Grant-Code")
		delete(headers, "X-Apple-Mandate-Security-Upgrade")
		delete(headers, "X-Apple-I-Require-UE")
	}
	if s.AuthAttributes != "" {
		headers["X-Apple-Auth-Attributes"] = s.AuthAttributes
	}
	if s.Scnt != "" {
		headers["scnt"] = s.Scnt
	}
	if s.SessionID != "" {
		headers["X-Apple-ID-Session-Id"] = s.SessionID
	}
	if s.SessionToken != "" {
		headers["X-Apple-Session-Token"] = s.SessionToken
	}
	if s.isAppleAccountManage() {
		headers["X-Apple-Domain-Id"] = "11"
		headers["X-Apple-Privacy-Consent"] = "true"
		headers["X-Apple-Privacy-Consent-Accepted"] = "true"
	}
	return headers
}

func (s *appleAuthSession) authHeaders() map[string]string {
	return s.srpHeaders()
}

func (s *appleAuthSession) twoFactorHeaders() map[string]string {
	headers := s.authHeaders()
	if s.isAppleAccountManage() {
		headers["Accept"] = "application/json, text/plain, */*"
		headers["X-Apple-App-Id"] = s.ClientID
		delete(headers, "X-Requested-With")
	}
	return headers
}

func applyAppleAccountBrowserHints(headers map[string]string) {
	headers["Accept-Language"] = langValue() + ",en;q=0.9"
	headers["Sec-CH-UA"] = `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`
	headers["Sec-CH-UA-Mobile"] = "?0"
	headers["Sec-CH-UA-Platform"] = appleAccountManagePlatform
}

func (s *appleAuthSession) isAppleAccountManage() bool {
	if s == nil {
		return false
	}
	return strings.EqualFold(strings.TrimRight(s.Endpoints.Home, "/"), "https://account.apple.com")
}

func (s *appleAuthSession) commonHeaders(overwrite map[string]string) map[string]string {
	userAgent := firstNonEmpty(s.UserAgent, uaValue())
	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"Origin":       s.Endpoints.Home,
		"Referer":      s.Endpoints.Home + "/",
		"User-Agent":   userAgent,
	}
	for key, value := range overwrite {
		headers[key] = value
	}
	return headers
}
