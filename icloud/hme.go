package icloud

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/internal/srp"
)

const (
	hmeBuildNumber      = "2640Build24"
	hmeOAuthClient      = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	hmeWebUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
	hmeSetupBase        = "https://setup.icloud.com/setup/ws/1"
	hmeSetupBaseCN      = "https://setup.icloud.com.cn/setup/ws/1"
	hmeWebSessionCookie = "X-APPLE-DS-WEB-SESSION-TOKEN"
)

var (
	ErrHMEAuthenticationRequired      = errors.New("hide my email web authentication required")
	ErrHMEWebRepairRequired           = errors.New("Apple 要求先接受当前 iCloud Web 条款")
	ErrHMEWebRepairSessionUnavailable = errors.New("没有可复用的 iCloud Web repair 会话")
)

type HideMyEmailAlias struct {
	Email           string `json:"email"`
	Label           string `json:"label,omitempty"`
	Note            string `json:"note,omitempty"`
	Active          bool   `json:"active"`
	CreateTimestamp int64  `json:"create_timestamp,omitempty"`
	ForwardToEmail  string `json:"forward_to_email,omitempty"`
	Origin          string `json:"origin,omitempty"`
	anonymousID     string
}

type HideMyEmailInfo struct {
	Aliases         []HideMyEmailAlias `json:"aliases"`
	ActiveCount     int                `json:"active_count"`
	InactiveCount   int                `json:"inactive_count"`
	ForwardToEmails []string           `json:"forward_to_emails,omitempty"`
	SelectedForward string             `json:"selected_forward_to,omitempty"`
	CheckedAt       time.Time          `json:"checked_at"`
}

type hmeCookie struct {
	Scope   string    `json:"scope"`
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Path    string    `json:"path,omitempty"`
	Secure  bool      `json:"secure,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
}

type hmeSession struct {
	AppleID        string      `json:"apple_id"`
	DSID           string      `json:"dsid"`
	ClientID       string      `json:"client_id"`
	ServiceURL     string      `json:"service_url"`
	AccountCountry string      `json:"account_country,omitempty"`
	Cookies        []hmeCookie `json:"cookies"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type HideMyEmailClient struct {
	appleID     string
	sessionPath string
	authBase    string
	setupBase   string
	http        *http.Client
	jar         http.CookieJar
	session     hmeSession
}

// NewHideMyEmailClient creates an account-bound client. Web cookies are kept
// only in the account's ignored state directory and are never returned to the
// dashboard API.
func NewHideMyEmailClient(accountDir, appleID string) (*HideMyEmailClient, error) {
	appleID = strings.TrimSpace(appleID)
	if accountDir == "" || appleID == "" {
		return nil, errors.New("hide my email account binding is incomplete")
	}
	c := &HideMyEmailClient{
		appleID:     appleID,
		sessionPath: filepath.Join(accountDir, "hme-session.json"),
		authBase:    "https://idmsa.apple.com/appleauth/auth",
		setupBase:   hmeSetupBase,
	}
	c.resetHTTP()
	_ = c.loadSession()
	return c, nil
}

func (c *HideMyEmailClient) resetHTTP() {
	jar, _ := cookiejar.New(nil)
	transport := gsa.AppleRootTransport().(*http.Transport)
	transport.Proxy = nil
	c.jar = jar
	c.http = &http.Client{Jar: jar, Transport: transport, Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func randomHex(bytesCount int) string {
	b := make([]byte, bytesCount)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", bytesCount*2)
	}
	return hex.EncodeToString(b)
}

func (c *HideMyEmailClient) loadSession() error {
	data, err := os.ReadFile(c.sessionPath)
	if err != nil {
		return err
	}
	var saved hmeSession
	if json.Unmarshal(data, &saved) != nil || !strings.EqualFold(saved.AppleID, c.appleID) || saved.DSID == "" || saved.ServiceURL == "" || saved.ClientID == "" {
		return errors.New("hide my email session binding is invalid")
	}
	c.session = saved
	for _, item := range saved.Cookies {
		u, parseErr := url.Parse(item.Scope)
		if parseErr != nil || u.Scheme != "https" || u.Hostname() == "" || item.Name == "" {
			continue
		}
		c.jar.SetCookies(u, []*http.Cookie{{Name: item.Name, Value: item.Value, Path: item.Path, Secure: item.Secure, Expires: item.Expires}})
	}
	return nil
}

func (c *HideMyEmailClient) saveSession() error {
	c.session.AppleID = c.appleID
	c.session.UpdatedAt = time.Now().UTC()
	c.session.Cookies = nil
	scopes := []string{c.authBase, c.setupBase, "https://www.icloud.com", c.session.ServiceURL}
	seen := map[string]bool{}
	for _, scope := range scopes {
		u, err := url.Parse(scope)
		if err != nil || u.Hostname() == "" {
			continue
		}
		origin := "https://" + u.Hostname()
		for _, cookie := range c.jar.Cookies(u) {
			key := origin + "\x00" + cookie.Name
			if cookie.Value == "" || seen[key] {
				continue
			}
			seen[key] = true
			c.session.Cookies = append(c.session.Cookies, hmeCookie{Scope: origin, Name: cookie.Name, Value: cookie.Value, Path: cookie.Path, Secure: cookie.Secure, Expires: cookie.Expires})
		}
	}
	data, err := json.Marshal(c.session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.sessionPath), 0700); err != nil {
		return err
	}
	temp := c.sessionPath + ".tmp"
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, c.sessionPath)
}

type hmeAuthState struct {
	frameID, clientID, srpSession  string
	authAttributes, sessionID      string
	scnt, sessionToken, trustToken string
	grantCode                      string
	accountCountry                 string
}

func (c *HideMyEmailClient) captureAuth(state *hmeAuthState, response *http.Response) {
	for name, target := range map[string]*string{
		"X-Apple-Auth-Attributes":    &state.authAttributes,
		"X-Apple-ID-Session-Id":      &state.sessionID,
		"scnt":                       &state.scnt,
		"X-Apple-Session-Token":      &state.sessionToken,
		"X-Apple-TwoSV-Trust-Token":  &state.trustToken,
		"X-Apple-OAuth-Grant-Code":   &state.grantCode,
		"X-Apple-ID-Account-Country": &state.accountCountry,
	} {
		if value := response.Header.Get(name); value != "" {
			*target = value
		}
	}
}

func hmeFDClientInfo() string {
	return fmt.Sprintf(`{"U":%q,"L":"zh-CN","Z":"GMT+08:00","V":"1.1","F":%q}`, hmeWebUserAgent, "."+randomHex(48))
}

func (c *HideMyEmailClient) authHeaders(state *hmeAuthState) http.Header {
	h := make(http.Header)
	if state.scnt != "" {
		h.Set("scnt", state.scnt)
	}
	if state.sessionID != "" {
		h.Set("X-Apple-ID-Session-Id", state.sessionID)
	}
	if state.authAttributes != "" {
		h.Set("X-Apple-Auth-Attributes", state.authAttributes)
	}
	if state.sessionToken != "" {
		h.Set("X-Apple-Session-Token", state.sessionToken)
	}
	h.Set("X-Apple-OAuth-Client-Id", state.clientID)
	h.Set("X-Apple-OAuth-Client-Type", "firstPartyAuth")
	h.Set("X-Apple-OAuth-Redirect-URI", "https://www.icloud.com")
	h.Set("X-Apple-OAuth-Require-Grant-Code", "true")
	h.Set("X-Apple-OAuth-Response-Mode", "web_message")
	h.Set("X-Apple-OAuth-Response-Type", "code")
	h.Set("X-Apple-OAuth-State", state.frameID)
	h.Set("X-Apple-Widget-Key", state.clientID)
	h.Set("X-Apple-Frame-Id", state.frameID)
	h.Set("X-Apple-Domain-Id", "6")
	h.Set("X-Apple-Locale", "zh_CN")
	h.Set("X-Apple-Offer-Security-Upgrade", "1")
	h.Set("X-Apple-Privacy-Consent", "true")
	h.Set("X-Apple-I-FD-Client-Info", hmeFDClientInfo())
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Origin", c.authBase)
	h.Set("Referer", c.authBase+"/")
	h.Set("User-Agent", hmeWebUserAgent)
	return h
}

func (c *HideMyEmailClient) authRequest(ctx context.Context, state *hmeAuthState, method, endpoint string, body []byte, headers http.Header) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header = headers
	response, err := c.http.Do(request)
	if err != nil {
		return nil, nil, err
	}
	c.captureAuth(state, response)
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	response.Body.Close()
	if readErr != nil || len(data) > 2<<20 {
		return response, nil, errors.New("Apple authentication response was invalid")
	}
	return response, data, nil
}

// Authenticate creates a dedicated iCloud Web session. codeProvider runs only
// after Apple has issued a trusted-device challenge for this same account.
func (c *HideMyEmailClient) Authenticate(ctx context.Context, password string, codeProvider func(context.Context) (string, error)) error {
	return c.authenticate(ctx, password, codeProvider, false)
}

// AuthenticateAndAcceptTerms creates a fresh iCloud Web session and accepts
// the exact iCloud terms version Apple advertises for this account. Callers
// must obtain explicit user authorization immediately before invoking it.
func (c *HideMyEmailClient) AuthenticateAndAcceptTerms(ctx context.Context, password string, codeProvider func(context.Context) (string, error)) error {
	return c.authenticate(ctx, password, codeProvider, true)
}

// WebSessionCookie is one cookie taken from an existing iCloud web login,
// for example a browser export. Values are credentials: callers must never
// print them, log them, or store them outside the account's private directory.
// Domain scopes the cookie to Apple's hosts; the ".icloud.com" cookies matter
// because Hide My Email answers on a per-account mail host, not on
// www.icloud.com.
type WebSessionCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
}

// WebSession is an existing iCloud web login captured by another tool. AppleID
// and DSID are optional cross-checks: when present they must match what Apple
// reports for the same session.
type WebSession struct {
	AppleID        string             `json:"apple_id,omitempty"`
	DSID           string             `json:"dsid,omitempty"`
	AccountCountry string             `json:"account_country,omitempty"`
	Cookies        []WebSessionCookie `json:"cookies"`
}

func (c *HideMyEmailClient) setupBaseFor(country string) string {
	if strings.EqualFold(strings.TrimSpace(country), "CHN") || strings.EqualFold(strings.TrimSpace(country), "CN") {
		return hmeSetupBaseCN
	}
	return c.setupBase
}

// UseWebSession adopts an existing iCloud web login. The captured cookies are
// replayed into this client's jar, and the iCloud web session token is handed
// to Apple's own accountLogin endpoint so Apple returns the account's DSID and
// Hide My Email service host. After it succeeds, List, Generate, Reserve,
// Update, SetActive and Delete need neither the password nor a trusted-device
// code: they use the cookies Apple itself issued.
//
// Nothing here re-implements a sign-in protocol; the only new input is data
// Apple already handed the browser. A rejected or expired web session leaves
// the client exactly as it was, so callers keep their existing fallback to
// Authenticate.
func (c *HideMyEmailClient) UseWebSession(ctx context.Context, web WebSession) error {
	if bound := strings.TrimSpace(web.AppleID); bound != "" && !strings.EqualFold(bound, c.appleID) {
		return errors.New("iCloud Web 会话不属于当前账号")
	}
	raw := make([]*http.Cookie, 0, len(web.Cookies))
	for _, item := range web.Cookies {
		raw = append(raw, &http.Cookie{Name: strings.TrimSpace(item.Name), Value: item.Value})
	}
	if err := validateHMEWebCookies(raw); err != nil {
		return err
	}
	token := ""
	for _, item := range web.Cookies {
		if strings.TrimSpace(item.Name) == hmeWebSessionCookie && item.Value != "" {
			token = item.Value
		}
	}
	if token == "" {
		return errors.New("iCloud Web 会话缺少 Web 会话令牌")
	}
	tokens := []string{token}
	if decoded, decodeErr := url.QueryUnescape(token); decodeErr == nil && decoded != token {
		tokens = append(tokens, decoded)
	}
	countries := []string{strings.TrimSpace(web.AccountCountry)}
	if countries[0] == "" {
		// The export does not always carry the account country, and China
		// accounts answer on their own setup host, so retry there rather than
		// asking the operator for a value Apple already knows.
		countries = append(countries, "CHN")
	}
	previousJar, previousHTTP, previousSession := c.jar, c.http, c.session
	var lastErr error
	for _, country := range countries {
		for _, candidate := range tokens {
			c.resetHTTP()
			installHMEWebCookies(c.jar, web.Cookies)
			state := &hmeAuthState{sessionToken: candidate, accountCountry: country}
			setupBase := c.setupBaseFor(country)
			if strings.TrimSpace(setupBase) == "" {
				setupBase = hmeSetupBase
			}
			candidate, err := c.loginCandidate(ctx, state, false, setupBase)
			if err != nil {
				lastErr = err
				continue
			}
			// Review item B6: every check runs against the in-memory candidate
			// before anything is persisted, so a mismatching DSID leaves the
			// session file byte-for-byte untouched. Previously the handshake
			// saved the session and only then compared the DSID, which left a
			// rejected web session on disk.
			if dsid := strings.TrimSpace(web.DSID); dsid != "" && candidate.DSID != dsid {
				c.restoreSession(previousJar, previousHTTP, previousSession)
				return errors.New("iCloud Web 会话与 iCloud 返回的账号标识不一致")
			}
			if err := c.adoptSession(candidate); err != nil {
				c.restoreSession(previousJar, previousHTTP, previousSession)
				return err
			}
			return nil
		}
	}
	c.restoreSession(previousJar, previousHTTP, previousSession)
	if lastErr == nil {
		lastErr = errors.New("iCloud Web 会话不可用")
	}
	return fmt.Errorf("iCloud Web 会话不可用：%w", lastErr)
}

// installHMEWebCookies replays captured browser cookies into the jar. Cookies
// Apple issued for ".icloud.com" stay domain cookies, so they also travel to
// the per-account mail service host.
func installHMEWebCookies(jar http.CookieJar, cookies []WebSessionCookie) {
	for _, item := range cookies {
		name, value := strings.TrimSpace(item.Name), item.Value
		if name == "" || value == "" {
			continue
		}
		domain := strings.TrimSpace(item.Domain)
		host := strings.TrimPrefix(domain, ".")
		if host == "" {
			host = "www.icloud.com"
		}
		if net.ParseIP(host) != nil {
			// A cookie jar has no domain concept for IP hosts.
			domain = ""
		}
		path := item.Path
		if path == "" {
			path = "/"
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: "/"}, []*http.Cookie{{Name: name, Value: value, Domain: domain, Path: path, Secure: item.Secure, HttpOnly: item.HTTPOnly}})
	}
}

func (c *HideMyEmailClient) restoreSession(jar http.CookieJar, client *http.Client, session hmeSession) {
	c.jar = jar
	c.session = session
	if client != nil {
		client.Jar = jar
		c.http = client
	}
}

// SessionActive reports whether a Hide My Email web session is bound to this
// Apple Account. It never contacts Apple.
func (c *HideMyEmailClient) SessionActive() bool {
	return c.session.ServiceURL != "" && c.session.DSID != "" && strings.EqualFold(c.session.AppleID, c.appleID)
}

// AcceptPendingTerms uses a previously saved, account-bound repair session.
// It does not authenticate, create aliases, or alter family membership.
func (c *HideMyEmailClient) AcceptPendingTerms(ctx context.Context) error {
	setupBase := c.setupBaseFor(c.session.AccountCountry)
	locale := "en-us"
	if setupBase == hmeSetupBaseCN {
		locale = "zh-cn"
	}
	setupURL, err := url.Parse(setupBase)
	if err != nil {
		return errors.New("iCloud Web repair endpoint is invalid")
	}
	hasRepair := false
	for _, cookie := range c.jar.Cookies(setupURL) {
		if cookie.Name == "X-APPLE-WEBAUTH-REPAIR" && cookie.Value != "" {
			hasRepair = true
			break
		}
	}
	if !hasRepair {
		return ErrHMEWebRepairSessionUnavailable
	}
	if err := c.acceptHMEWebTerms(ctx, setupBase, locale); err != nil {
		return err
	}
	return c.saveSession()
}

func (c *HideMyEmailClient) authenticate(ctx context.Context, password string, codeProvider func(context.Context) (string, error), acceptTerms bool) error {
	if password == "" {
		return errors.New("hide my email web login requires the current account password")
	}
	c.resetHTTP()
	state := &hmeAuthState{frameID: randomHex(16), clientID: hmeOAuthClient}
	query := url.Values{"frame_id": {"auth-" + state.frameID}, "language": {"en_US"}, "skVersion": {"7"}, "iframeId": {"auth-" + state.frameID}, "client_id": {state.clientID}, "redirect_uri": {"https://www.icloud.com"}, "response_type": {"code"}, "response_mode": {"web_message"}, "state": {"auth-" + state.frameID}, "authVersion": {"latest"}}
	startHeaders := make(http.Header)
	startHeaders.Set("Accept", "*/*")
	startHeaders.Set("User-Agent", hmeWebUserAgent)
	response, _, err := c.authRequest(ctx, state, http.MethodGet, c.authBase+"/authorize/signin?"+query.Encode(), nil, startHeaders)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Apple web authentication could not be initialized")
	}

	federateBody, _ := json.Marshal(map[string]any{"accountName": c.appleID, "rememberMe": true})
	response, _, err = c.authRequest(ctx, state, http.MethodPost, c.authBase+"/federate?isRememberMeEnabled=true", federateBody, c.authHeaders(state))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Apple web account federation failed")
	}

	params := srp.NewParams()
	srpClient := srp.NewClient(params, c.appleID)
	initBody, _ := json.Marshal(map[string]any{"a": base64.StdEncoding.EncodeToString(srpClient.PublicA().Bytes()), "accountName": c.appleID, "protocols": []string{"s2k", "s2k_fo"}})
	response, initData, err := c.authRequest(ctx, state, http.MethodPost, c.authBase+"/signin/init", initBody, c.authHeaders(state))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Apple web SRP challenge failed")
	}
	var challenge struct {
		Iteration            int `json:"iteration"`
		Salt, Protocol, B, C string
	}
	if json.Unmarshal(initData, &challenge) != nil || challenge.Iteration <= 0 || challenge.Salt == "" || challenge.B == "" || challenge.C == "" {
		return errors.New("Apple web SRP challenge was incomplete")
	}
	state.srpSession = challenge.C
	salt, saltErr := base64.StdEncoding.DecodeString(challenge.Salt)
	bBytes, bErr := base64.StdEncoding.DecodeString(challenge.B)
	if saltErr != nil || bErr != nil {
		return errors.New("Apple web SRP challenge encoding was invalid")
	}
	passwordHash := sha256.Sum256([]byte(password))
	passwordKey, keyErr := pbkdf2.Key(sha256.New, string(passwordHash[:]), salt, challenge.Iteration, 32)
	if keyErr != nil {
		return keyErr
	}
	x := params.DeriveX(salt, passwordKey)
	m1, _, proofErr := srpClient.ProcessChallenge(x, salt, new(big.Int).SetBytes(bBytes))
	if proofErr != nil {
		return proofErr
	}
	m2, proofErr := srpClient.ExpectedM2()
	if proofErr != nil {
		return proofErr
	}
	completeBody, _ := json.Marshal(map[string]any{"accountName": c.appleID, "rememberMe": true, "trustTokens": []string{}, "m1": base64.StdEncoding.EncodeToString(m1), "c": state.srpSession, "m2": base64.StdEncoding.EncodeToString(m2)})
	response, _, err = c.authRequest(ctx, state, http.MethodPost, c.authBase+"/signin/complete?isRememberMeEnabled=true", completeBody, c.authHeaders(state))
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusConflict {
		if codeProvider == nil {
			return ErrHMEAuthenticationRequired
		}
		response, _, err = c.authRequest(ctx, state, http.MethodPut, c.authBase+"/verify/trusteddevice/securitycode", nil, c.authHeaders(state))
		if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			return errors.New("Apple trusted-device code request failed")
		}
		codeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		code, codeErr := codeProvider(codeCtx)
		cancel()
		if codeErr != nil || len(strings.TrimSpace(code)) != 6 {
			return errors.New("local trusted-device code generation failed")
		}
		verifyBody, _ := json.Marshal(map[string]any{"securityCode": map[string]string{"code": strings.TrimSpace(code)}})
		response, verifyData, err := c.authRequest(ctx, state, http.MethodPost, c.authBase+"/verify/trusteddevice/securitycode", verifyBody, c.authHeaders(state))
		for index := range verifyBody {
			verifyBody[index] = 0
		}
		code = ""
		if err != nil || !((response.StatusCode >= 200 && response.StatusCode < 300) || (response.StatusCode == http.StatusConflict && state.sessionToken != "" && bytes.Contains(verifyData, []byte(`"valid":true`)))) {
			return errors.New("Apple rejected the trusted-device verification code")
		}
	} else if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Apple web credentials were rejected")
	}
	response, _, err = c.authRequest(ctx, state, http.MethodGet, c.authBase+"/2sv/trust", nil, c.authHeaders(state))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Apple web session trust failed")
	}
	return c.accountLogin(ctx, state, acceptTerms)
}

func validateHMEWebCookies(cookies []*http.Cookie) error {
	hasWebSession, requiresRepair := false, false
	for _, cookie := range cookies {
		switch cookie.Name {
		case "X-APPLE-DS-WEB-SESSION-TOKEN", "X-APPLE-WEBAUTH-TOKEN":
			hasWebSession = cookie.Value != ""
		case "X-APPLE-WEBAUTH-REPAIR":
			requiresRepair = cookie.Value != ""
		}
	}
	if hasWebSession {
		return nil
	}
	if requiresRepair {
		return ErrHMEWebRepairRequired
	}
	return errors.New("Apple 未签发 iCloud Web 会话令牌")
}

type hmeAccountLoginResult struct {
	DSInfo struct {
		DSID         string `json:"dsid"`
		LanguageCode string `json:"languageCode"`
	} `json:"dsInfo"`
	Webservices map[string]struct {
		URL string `json:"url"`
	} `json:"webservices"`
	TermsUpdateNeeded bool `json:"termsUpdateNeeded"`
}

// accountLogin adopts the session Apple reports for a freshly authenticated
// password/trusted-device login. That path has already proven the account
// binding, so the candidate is adopted (and persisted) right away.
func (c *HideMyEmailClient) accountLogin(ctx context.Context, state *hmeAuthState, acceptTerms bool) error {
	return c.accountLoginAt(ctx, state, acceptTerms, c.setupBaseFor(state.accountCountry))
}

// accountLoginAt is accountLogin against an explicit setup host.
func (c *HideMyEmailClient) accountLoginAt(ctx context.Context, state *hmeAuthState, acceptTerms bool, setupBase string) error {
	candidate, err := c.loginCandidate(ctx, state, acceptTerms, setupBase)
	if err != nil {
		return err
	}
	return c.adoptSession(candidate)
}

// loginCandidate runs Apple's accountLogin handshake and returns the session it
// describes without touching c.session, the cookie jar or the account
// directory. Review item B6: callers that still have to cross-check the session
// (UseWebSession compares the caller-supplied DSID) must be able to reject it
// before a single byte reaches the disk — the previous order saved first and
// compared afterwards, so a rejected web session was already persisted.
func (c *HideMyEmailClient) loginCandidate(ctx context.Context, state *hmeAuthState, acceptTerms bool, setupBase string) (hmeSession, error) {
	loginBody, _ := json.Marshal(map[string]any{"dsWebAuthToken": state.sessionToken, "accountCountryCode": state.accountCountry, "extended_login": true, "trustToken": state.trustToken})
	data, err := c.hmeSetupRequest(ctx, http.MethodPost, setupBase+"/accountLogin", loginBody)
	if err != nil {
		return hmeSession{}, errors.New("Apple iCloud Web session establishment failed")
	}
	var result hmeAccountLoginResult
	if json.Unmarshal(data, &result) != nil || result.DSInfo.DSID == "" {
		return hmeSession{}, errors.New("iCloud+ Hide My Email is unavailable for this account")
	}
	if result.TermsUpdateNeeded {
		if !acceptTerms {
			return hmeSession{}, ErrHMEWebRepairRequired
		}
		if err := c.acceptHMEWebTerms(ctx, setupBase, result.DSInfo.LanguageCode); err != nil {
			return hmeSession{}, err
		}
		data, err = c.hmeSetupRequest(ctx, http.MethodPost, setupBase+"/accountLogin", loginBody)
		result = hmeAccountLoginResult{}
		if err != nil || json.Unmarshal(data, &result) != nil || result.TermsUpdateNeeded {
			return hmeSession{}, errors.New("Apple 未确认 iCloud Web 条款接受状态")
		}
	}
	if result.Webservices["premiummailsettings"].URL == "" {
		return hmeSession{}, errors.New("iCloud+ Hide My Email is unavailable for this account")
	}
	candidate := hmeSession{AppleID: c.appleID, DSID: result.DSInfo.DSID, ClientID: randomHex(16), ServiceURL: strings.TrimRight(result.Webservices["premiummailsettings"].URL, "/"), AccountCountry: state.accountCountry}
	serviceURL, _ := url.Parse(candidate.ServiceURL)
	if err := validateHMEWebCookies(c.jar.Cookies(serviceURL)); err != nil {
		return hmeSession{}, err
	}
	return candidate, nil
}

// adoptSession binds an already validated candidate to this client and writes
// it atomically. A failed write rolls the in-memory session back, so memory and
// disk never disagree about which account this client is bound to.
func (c *HideMyEmailClient) adoptSession(candidate hmeSession) error {
	previous := c.session
	c.session = candidate
	if err := c.saveSession(); err != nil {
		c.session = previous
		return err
	}
	return nil
}

func (c *HideMyEmailClient) hmeSetupRequest(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Origin", "https://www.icloud.com")
	request.Header.Set("Referer", "https://www.icloud.com/")
	request.Header.Set("User-Agent", hmeWebUserAgent)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if readErr != nil || len(data) > 4<<20 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("Apple iCloud Web setup request failed")
	}
	return data, nil
}

func (c *HideMyEmailClient) acceptHMEWebTerms(ctx context.Context, setupBase, locale string) error {
	if strings.TrimSpace(locale) == "" {
		locale = "zh-cn"
	}
	body, _ := json.Marshal(map[string]string{"locale": locale})
	data, err := c.hmeSetupRequest(ctx, http.MethodGet, strings.TrimRight(setupBase, "/")+"/getTerms", body)
	if err != nil {
		return errors.New("无法读取 Apple 当前 iCloud Web 条款版本")
	}
	var terms struct {
		ICloudTerms struct {
			Version int `json:"version"`
		} `json:"iCloudTerms"`
	}
	if json.Unmarshal(data, &terms) != nil || terms.ICloudTerms.Version <= 0 {
		return errors.New("Apple 未返回有效的 iCloud Web 条款版本")
	}
	body, _ = json.Marshal(map[string]int{"acceptedICloudTerms": terms.ICloudTerms.Version})
	if _, err = c.hmeSetupRequest(ctx, http.MethodGet, strings.TrimRight(setupBase, "/")+"/repairDone", body); err != nil {
		return errors.New("Apple 未接受 iCloud Web 条款确认")
	}
	return nil
}

type hmeEnvelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		ErrorCode            int `json:"errorCode"`
		ErrorMessage, Reason string
	} `json:"error"`
}

func (c *HideMyEmailClient) request(ctx context.Context, method, path string, payload any, out any) error {
	if c.session.ServiceURL == "" || c.session.DSID == "" {
		return ErrHMEAuthenticationRequired
	}
	endpoint, err := url.Parse(c.session.ServiceURL + path)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("clientBuildNumber", hmeBuildNumber)
	query.Set("clientMasteringNumber", hmeBuildNumber)
	query.Set("clientId", c.session.ClientID)
	query.Set("dsid", c.session.DSID)
	endpoint.RawQuery = query.Encode()
	var body io.Reader
	if payload != nil {
		data, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return marshalErr
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return err
	}
	origin := "https://www.icloud.com"
	if strings.HasSuffix(strings.ToLower(endpoint.Hostname()), ".icloud.com.cn") {
		origin = "https://www.icloud.com.cn"
	}
	request.Header.Set("Origin", origin)
	request.Header.Set("Referer", origin+"/")
	request.Header.Set("User-Agent", hmeWebUserAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Content-Type", "text/plain")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	response.Body.Close()
	if readErr != nil || len(data) > 4<<20 {
		return errors.New("Hide My Email response was invalid")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == 421 {
		return ErrHMEAuthenticationRequired
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Hide My Email returned HTTP %d", response.StatusCode)
	}
	var envelope hmeEnvelope
	if json.Unmarshal(data, &envelope) != nil || !envelope.Success {
		if envelope.Error != nil && envelope.Error.ErrorCode != 0 {
			return fmt.Errorf("Hide My Email rejected the request (code %d)", envelope.Error.ErrorCode)
		}
		return errors.New("Hide My Email rejected the request")
	}
	if out != nil && json.Unmarshal(envelope.Result, out) != nil {
		return errors.New("Hide My Email returned an unexpected result")
	}
	_ = c.saveSession()
	return nil
}

func (c *HideMyEmailClient) List(ctx context.Context) (*HideMyEmailInfo, error) {
	var result struct {
		ForwardToEmails []string `json:"forwardToEmails"`
		SelectedForward string   `json:"selectedForwardTo"`
		Emails          []struct {
			HME, Label, Note                    string
			IsActive                            bool
			CreateTimestamp                     int64
			AnonymousID, ForwardToEmail, Origin string
		} `json:"hmeEmails"`
	}
	if err := c.request(ctx, http.MethodGet, "/v2/hme/list", nil, &result); err != nil {
		return nil, err
	}
	info := &HideMyEmailInfo{Aliases: make([]HideMyEmailAlias, 0, len(result.Emails)), ForwardToEmails: append([]string(nil), result.ForwardToEmails...), SelectedForward: result.SelectedForward, CheckedAt: time.Now().UTC()}
	for _, item := range result.Emails {
		alias := HideMyEmailAlias{Email: item.HME, Label: item.Label, Note: item.Note, Active: item.IsActive, CreateTimestamp: item.CreateTimestamp, ForwardToEmail: item.ForwardToEmail, Origin: item.Origin, anonymousID: item.AnonymousID}
		if alias.Active {
			info.ActiveCount++
		} else {
			info.InactiveCount++
		}
		info.Aliases = append(info.Aliases, alias)
	}
	return info, nil
}

func forwardingTargetAvailable(info *HideMyEmailInfo, target string) bool {
	target = strings.TrimSpace(target)
	if info == nil || target == "" {
		return false
	}
	if strings.EqualFold(target, strings.TrimSpace(info.SelectedForward)) {
		return true
	}
	for _, candidate := range info.ForwardToEmails {
		if strings.EqualFold(target, strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

// UpdateForwardTo changes the account-wide Hide My Email forwarding target.
// Apple applies this one target to every alias. The target must be one of the
// account-associated addresses returned by the live list response.
func (c *HideMyEmailClient) UpdateForwardTo(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	info, err := c.List(ctx)
	if err != nil {
		return err
	}
	if !forwardingTargetAvailable(info, target) {
		return errors.New("forwarding target is not available for this Apple Account")
	}
	if strings.EqualFold(target, strings.TrimSpace(info.SelectedForward)) {
		return nil
	}
	return c.request(ctx, http.MethodPost, "/v1/hme/updateForwardTo", map[string]string{"forwardToEmail": target}, nil)
}

func (c *HideMyEmailClient) Generate(ctx context.Context, language string) (string, error) {
	if language == "" {
		language = "zh-cn"
	}
	var result struct {
		HME string `json:"hme"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/hme/generate", map[string]string{"langCode": language}, &result); err != nil {
		return "", err
	}
	if result.HME == "" {
		return "", errors.New("Hide My Email did not return a candidate")
	}
	return result.HME, nil
}

func (c *HideMyEmailClient) Reserve(ctx context.Context, candidate, label, note string) error {
	if candidate == "" || label == "" {
		return errors.New("candidate and label are required")
	}
	return c.request(ctx, http.MethodPost, "/v1/hme/reserve", map[string]string{"hme": candidate, "label": label, "note": note}, nil)
}

func (c *HideMyEmailClient) findAlias(ctx context.Context, email string) (*HideMyEmailAlias, error) {
	info, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var match *HideMyEmailAlias
	for index := range info.Aliases {
		if strings.EqualFold(info.Aliases[index].Email, strings.TrimSpace(email)) {
			if match != nil {
				return nil, errors.New("Hide My Email alias binding is ambiguous")
			}
			copy := info.Aliases[index]
			match = &copy
		}
	}
	if match == nil || match.anonymousID == "" {
		return nil, errors.New("Hide My Email alias was not found")
	}
	return match, nil
}

func (c *HideMyEmailClient) Update(ctx context.Context, email, label, note string) error {
	alias, err := c.findAlias(ctx, email)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "/v1/hme/updateMetaData", map[string]string{"anonymousId": alias.anonymousID, "label": label, "note": note}, nil)
}

func (c *HideMyEmailClient) SetActive(ctx context.Context, email string, active bool) error {
	alias, err := c.findAlias(ctx, email)
	if err != nil {
		return err
	}
	if alias.Active == active {
		return nil
	}
	path := "/v1/hme/deactivate"
	if active {
		path = "/v1/hme/reactivate"
	}
	return c.request(ctx, http.MethodPost, path, map[string]string{"anonymousId": alias.anonymousID}, nil)
}

func (c *HideMyEmailClient) Delete(ctx context.Context, email string) error {
	alias, err := c.findAlias(ctx, email)
	if err != nil {
		return err
	}
	if alias.Active {
		return errors.New("Hide My Email address must be deactivated before deletion")
	}
	return c.request(ctx, http.MethodPost, "/v1/hme/delete", map[string]string{"anonymousId": alias.anonymousID}, nil)
}
