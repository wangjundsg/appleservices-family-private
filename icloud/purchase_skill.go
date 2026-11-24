package icloud

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/backgroundexec"
	"github.com/Laky-64/appleservices/internal/sap"
	"howett.net/plist"
)

const skillStoreAgent = "Configurator/2.18 (Macintosh; OS X 15.4.1; 24E263) AppleWebKit/0621.1.15.11.10"

func skillAuthenticationAgent() string {
	if os.Getenv("APPLESERVICES_SKILL_STORE_PROFILE") == "compact" {
		return "Configurator/2.18 (Macintosh; OS X 15.4.1; 24E263)"
	}
	return skillStoreAgent
}

type skillPythonTransport struct{}

func (skillPythonTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, errors.New("cannot read private request")
		}
	}
	headers := map[string]string{}
	for k, v := range req.Header {
		headers[k] = strings.Join(v, ", ")
	}
	input, err := json.Marshal(map[string]any{"method": req.Method, "url": req.URL.String(), "headers": headers, "body": body})
	if err != nil {
		return nil, errors.New("cannot encode private request")
	}
	command := exec.CommandContext(req.Context(), "python", "tools/skill-store-transport.py")
	backgroundexec.Configure(command)
	command.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, errors.New("direct Python HTTPS transport failed")
	}
	var decoded struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    []byte              `json:"body"`
		Error   string              `json:"error"`
	}
	if json.Unmarshal(output.Bytes(), &decoded) != nil || decoded.Error != "" || decoded.Status < 100 || decoded.Status > 599 {
		return nil, errors.New("invalid private transport response")
	}
	h := http.Header{}
	for k, vs := range decoded.Headers {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	return &http.Response{StatusCode: decoded.Status, Header: h, Body: io.NopCloser(bytes.NewReader(decoded.Body)), ContentLength: int64(len(decoded.Body)), Request: req}, nil
}

func skillHTTPTransport() http.RoundTripper {
	if os.Getenv("APPLESERVICES_SKILL_HTTP_PYTHON") == "1" {
		return skillPythonTransport{}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if os.Getenv("APPLESERVICES_SKILL_REFERENCE_WIRE") == "1" {
		transport.TLSClientConfig = &tls.Config{
			MinVersion:       tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521},
		}
	}
	return transport
}

func skillMatchedHTTPTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// Keep the established Store TLS fingerprint together with the matched
	// endpoint and exact signed XML wire instead of splitting them across
	// process-global diagnostic switches.
	transport.TLSClientConfig = &tls.Config{
		MinVersion:       tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521},
	}
	return transport
}

// The successful standalone client used this XML layout and Go 1.21 TLS
// defaults. Keep this opt-in comparison separate from the standard encoder.
func skillReferenceLoginBody(appleID, password, guid string) []byte {
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	values := [][2]string{{"appleId", appleID}, {"attempt", "1"}, {"guid", guid}, {"password", password}, {"rmp", "0"}, {"why", "signIn"}}
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, "<key>"+value[0]+"</key><string>"+escape.Replace(value[1])+"</string>")
	}
	return []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n" + strings.Join(items, "\n") + "\n</dict></plist>")
}

// AuthenticateSkillStore follows the repository's icloud-plus-purchase skill.
// It uses the host's physical Store GUID; it never alters the account Mac ADI.
// Routing retries are bounded, and application rejections or 429 stop the run.
func AuthenticateSkillStore(ctx context.Context, appleID, password string, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	guid, hardware, err := storeHardwareIdentity()
	if err != nil || isVirtualInterface("", hardware) {
		return nil, &StorageError{Stage: "purchase-store-physical-hardware"}
	}
	client := &http.Client{Jar: mustCookieJar(), Transport: skillHTTPTransport(), Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return authenticateSkillStoreClient(ctx, client, guid, appleID, password, codeProvider)
}

// AuthenticateSignedSkillStore keeps the physical Store identity while testing
// the signed native endpoint returned by Apple's current App Store bag.
func AuthenticateSignedSkillStore(ctx context.Context, appleID, password string, headers map[string]string, appStoreProfile bool, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	return authenticateSignedSkillStoreMode(ctx, appleID, password, headers, appStoreProfile, os.Getenv("APPLESERVICES_SKILL_MATCHED_BAG") == "1", skillHTTPTransport(), codeProvider)
}

// AuthenticateMatchedSkillStore preserves the authenticate/setup/certificate
// tuple returned by one Apple Store bag without process-global diagnostics.
func AuthenticateMatchedSkillStore(ctx context.Context, appleID, password string, headers map[string]string, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	session, err := authenticateSignedSkillStoreMode(ctx, appleID, password, headers, false, true, skillMatchedHTTPTransport(), codeProvider)
	if err == nil || !storeRoutingFailure(err) || os.Getenv("APPLESERVICES_SKILL_HTTP_PYTHON") == "1" {
		return session, err
	}
	fmt.Println("Apple Store Go transport reached only routing responses; retrying once with direct Python TLS transport")
	return authenticateSignedSkillStoreMode(ctx, appleID, password, headers, false, true, skillPythonTransport{}, codeProvider)
}

func storeRoutingFailure(err error) bool {
	var failure *StorageError
	if !errors.As(err, &failure) || failure.Stage != "purchase-store-authentication" {
		return false
	}
	switch failure.HTTPStatus {
	case 204, 301, 302, 403, 404, 500, 503:
		return true
	default:
		return false
	}
}

func authenticateSignedSkillStoreMode(ctx context.Context, appleID, password string, headers map[string]string, appStoreProfile, matchedBag bool, transport http.RoundTripper, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	guid, hardware, err := storeHardwareIdentity()
	if err != nil || isVirtualInterface("", hardware) {
		return nil, &StorageError{Stage: "purchase-store-physical-hardware"}
	}
	client := &http.Client{Jar: mustCookieJar(), Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	agent := skillAuthenticationAgent()
	if matchedBag {
		agent = storeSAPUserAgent
	}
	if appStoreProfile {
		agent = storeMacUserAgent
	} else {
		headers = nil
	}
	// A matched-bag login must preserve authenticateAccount together with the
	// setup and certificate endpoints from that same bag. Passing only the
	// matched UA still lets the lower layer normalize authenticateAccount to
	// native/fast, recreating the exact tuple mismatch fixed by 1407168.
	bag, err := loadStoreSAPBagForAgentMode(ctx, client, guid, agent, appStoreProfile, matchedBag)
	if err != nil {
		client.CloseIdleConnections()
		return nil, &StorageError{Stage: "purchase-store-bag"}
	}
	u, _ := url.Parse(bag.AuthEndpoint)
	if u.Path == "/auth/v1/native/fast" {
		u.Path += "/"
		bag.AuthEndpoint = u.String()
	}
	fmt.Printf("Apple bag Store endpoint: %s\n", safeEndpointLabel(u))
	signer, err := sap.NewSigner(ctx, sap.Config{SetupURL: bag.SetupURL, CertificateURL: bag.CertificateURL, Version: bag.Version, HardwareID: hardware})
	if err != nil {
		client.CloseIdleConnections()
		return nil, &StorageError{Stage: "purchase-store-sap"}
	}
	defer signer.Close()
	return authenticateSkillStoreConfiguredMode(ctx, client, bag.AuthEndpoint, guid, appleID, password, agent, headers, signer, matchedBag, codeProvider)
}

func authenticateSkillStoreClient(ctx context.Context, client *http.Client, guid, appleID, password string, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	return authenticateSkillStoreConfigured(ctx, client, "https://auth.itunes.apple.com/auth/v1/native/fast/", guid, appleID, password, skillAuthenticationAgent(), nil, nil, codeProvider)
}

func authenticateSkillStoreConfigured(ctx context.Context, client *http.Client, endpoint, guid, appleID, password, agent string, headers map[string]string, signer sap.ActionSigner, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	return authenticateSkillStoreConfiguredMode(ctx, client, endpoint, guid, appleID, password, agent, headers, signer, false, codeProvider)
}

func authenticateSkillStoreConfiguredMode(ctx context.Context, client *http.Client, endpoint, guid, appleID, password, agent string, headers map[string]string, signer sap.ActionSigner, referenceWire bool, codeProvider func() (string, error)) (*StoreCommerceSession, error) {
	maxAttempts := 20
	if raw := os.Getenv("APPLESERVICES_SKILL_AUTH_ATTEMPTS"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return nil, &StorageError{Stage: "purchase-store-retry-limit"}
		}
		maxAttempts = limit
	}
	needsCode := false
	challengeCode := ""
	defer func() { challengeCode = "" }()
	for attempt, redirects := 1, 0; attempt <= maxAttempts; attempt++ {
		code := challengeCode
		var err error
		if codeProvider != nil && needsCode && challengeCode == "" {
			challengeCode, err = codeProvider()
			if err != nil || len(challengeCode) != 6 || strings.IndexFunc(challengeCode, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return nil, &StorageError{Stage: "purchase-store-code"}
			}
			code = challengeCode
		}
		body, err := plistLoginAttemptBody(appleID, password+code, guid, 1)
		if err != nil {
			return nil, &StorageError{Stage: "purchase-store-request"}
		}
		if referenceWire || os.Getenv("APPLESERVICES_SKILL_REFERENCE_WIRE") == "1" {
			body = skillReferenceLoginBody(appleID, password+code, guid)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if signer != nil {
			signature, err := signer.Sign(body)
			if err != nil {
				client.CloseIdleConnections()
				return nil, &StorageError{Stage: "purchase-store-sap"}
			}
			for key, value := range headers {
				req.Header.Set(key, value)
			}
			req.Header.Set("X-Apple-ActionSignature", base64.StdEncoding.EncodeToString(signature))
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", agent)
		if signer != nil {
			req.Header.Set("Accept", "*/*")
			if agent == storeMacUserAgent {
				req.Header.Set("Content-Type", "application/x-apple-plist")
			}
		}
		resp, err := client.Do(req)
		code = ""
		if err != nil {
			client.CloseIdleConnections()
			return nil, &StorageError{Stage: "purchase-store-transport"}
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStoreBagBytes+1))
		resp.Body.Close()
		if readErr != nil || len(data) > maxStoreBagBytes {
			return nil, &StorageError{Stage: "purchase-store-response", HTTPStatus: resp.StatusCode}
		}
		fmt.Printf("skill Store authentication attempt=%d; HTTP=%d; response-bytes=%d\n", attempt, resp.StatusCode, len(data))
		if resp.TLS != nil && (attempt == 1 || resp.StatusCode == 200) {
			fmt.Printf("Store transport: %s; TLS=%x; cipher=%s; verified-chains=%d\n", resp.Proto, resp.TLS.Version, tls.CipherSuiteName(resp.TLS.CipherSuite), len(resp.TLS.VerifiedChains))
		}
		if dir := os.Getenv("APPLESERVICES_STORE_AUTH_DIAGNOSTIC_DIR"); dir != "" {
			f, err := os.CreateTemp(dir, "skill-store-auth-*.json")
			if err != nil {
				return nil, errors.New("cannot create private Store diagnostic")
			}
			writeErr := json.NewEncoder(f).Encode(map[string]any{"HTTP": resp.StatusCode, "headers": resp.Header, "body": data})
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				return nil, errors.New("cannot save private Store diagnostic")
			}
		}
		if resp.StatusCode == 200 {
			identity, err := decodeStoreLogin(data, resp.StatusCode, resp.Header)
			if err != nil {
				var failure *StorageError
				if errors.As(err, &failure) && failure.Stage == "purchase-store-2fa" && codeProvider != nil {
					if !needsCode {
						needsCode = true
						fmt.Println("Apple requests Store 2FA; continuing with the same cookie jar")
						continue
					}
					failure.Stage = "purchase-store-code-unverified"
				}
				client.CloseIdleConnections()
				return nil, err
			}
			identity.GUID = guid
			return &StoreCommerceSession{client: client, identity: identity}, nil
		}
		if resp.StatusCode == 429 {
			client.CloseIdleConnections()
			return nil, &StorageError{Stage: "purchase-store-rate-limited", HTTPStatus: 429}
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			if redirects >= 4 {
				return nil, &StorageError{Stage: "purchase-store-redirect", HTTPStatus: resp.StatusCode}
			}
			endpoint, err = validStoreRedirect(endpoint, resp.Header.Get("Location"))
			if err != nil {
				return nil, &StorageError{Stage: "purchase-store-redirect", HTTPStatus: resp.StatusCode}
			}
			redirects++
			continue
		}
		switch resp.StatusCode {
		case 204, 301, 302, 404, 500, 503:
		case 403:
			if !strings.Contains(strings.ToLower(string(data)), "<html") || resp.Header.Get("Apple-Originating-System") != "" || resp.Header.Get("X-Responding-Instance") != "" {
				client.CloseIdleConnections()
				return nil, &StorageError{Stage: "purchase-store-authentication", HTTPStatus: 403}
			}
		default:
			client.CloseIdleConnections()
			return nil, &StorageError{Stage: "purchase-store-authentication", HTTPStatus: resp.StatusCode}
		}
		if attempt == maxAttempts {
			client.CloseIdleConnections()
			return nil, &StorageError{Stage: "purchase-store-authentication", HTTPStatus: resp.StatusCode}
		}
		select {
		case <-ctx.Done():
			return nil, &StorageError{Stage: "purchase-store-authentication", HTTPStatus: resp.StatusCode}
		case <-time.After(time.Second):
		}
	}
	return nil, &StorageError{Stage: "purchase-store-authentication"}
}

// SavePrivateSkillSession retains commerce credentials only in a new private
// file. The caller must provide an owner-only directory excluded from Git.
func (s *StoreCommerceSession) SavePrivateSkillSession(path, appleID string) error {
	if s == nil || s.identity == nil || s.client == nil {
		return errors.New("Store session unavailable")
	}
	cookies := map[string][]*http.Cookie{}
	endpoints := []string{"https://auth.itunes.apple.com/", "https://auth.itunes.apple.com/auth/v1/native/fast/", "https://buy.itunes.apple.com/", "https://buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/", "https://speedysub.itunes.apple.com/", "https://speedysub.itunes.apple.com/subscription/v3/manage/"}
	if s.identity.Pod != "" {
		endpoints = append(endpoints, "https://p"+s.identity.Pod+"-buy.itunes.apple.com/", "https://p"+s.identity.Pod+"-buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/")
	}
	for _, endpoint := range endpoints {
		u, _ := url.Parse(endpoint)
		cookies[endpoint] = s.client.Jar.Cookies(u)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("private Store output must be a new path")
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(map[string]any{"apple-id": appleID, "identity": s.identity, "cookies": cookies}); err != nil {
		return errors.New("cannot save private Store session")
	}
	return f.Sync()
}

func LoadPrivateSkillSession(path, appleID, expectedDSID string) (*StoreCommerceSession, error) {
	data, err := os.ReadFile(path)
	var saved struct {
		AppleID  string                    `json:"apple-id"`
		Identity storeSession              `json:"identity"`
		Cookies  map[string][]*http.Cookie `json:"cookies"`
	}
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &saved) != nil || !strings.EqualFold(saved.AppleID, appleID) || saved.Identity.DSID != expectedDSID || saved.Identity.PasswordToken == "" || saved.Identity.Anonymous {
		return nil, errors.New("private Store checkpoint account binding failed")
	}
	guid, _, err := storeHardwareIdentity()
	if err != nil || saved.Identity.GUID != guid {
		return nil, errors.New("private Store checkpoint physical GUID mismatch")
	}
	jar, _ := cookiejar.New(nil)
	for endpoint, cookies := range saved.Cookies {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || (u.Hostname() != "auth.itunes.apple.com" && u.Hostname() != "speedysub.itunes.apple.com" && u.Hostname() != "buy.itunes.apple.com" && u.Hostname() != "p"+saved.Identity.Pod+"-buy.itunes.apple.com") {
			return nil, errors.New("unsupported Store cookie scope")
		}
		jar.SetCookies(u, cookies)
	}
	return &StoreCommerceSession{identity: &saved.Identity, client: &http.Client{Jar: jar, Transport: skillHTTPTransport(), Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (s *StoreCommerceSession) skillRequest(ctx context.Context, method, endpoint, agent, contentType string, body []byte) ([]byte, int, error) {
	if s == nil || s.identity == nil || s.client == nil || !s.MatchesAccount(s.identity.DSID) {
		return nil, 0, &StorageError{Stage: "purchase-store-session"}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Hostname() != "speedysub.itunes.apple.com" && !validStoreCommerceURL(u)) {
		return nil, 0, &StorageError{Stage: "purchase-skill-endpoint"}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &StorageError{Stage: "purchase-skill-request"}
	}
	s.identity.setHeaders(req)
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "application/json, application/x-apple-plist, text/html")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, &StorageError{Stage: "purchase-skill-transport"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxStoreBagBytes+1))
	if err != nil || len(data) > maxStoreBagBytes {
		return nil, resp.StatusCode, &StorageError{Stage: "purchase-skill-response", HTTPStatus: resp.StatusCode}
	}
	if dir := os.Getenv("APPLESERVICES_STORE_AUTH_DIAGNOSTIC_DIR"); dir != "" {
		f, err := os.CreateTemp(dir, "skill-commerce-response-*.json")
		if err != nil {
			return nil, resp.StatusCode, errors.New("cannot create private commerce response")
		}
		writeErr := json.NewEncoder(f).Encode(map[string]any{"method": method, "host": u.Hostname(), "path": u.Path, "HTTP": resp.StatusCode, "headers": resp.Header, "body": data})
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return nil, resp.StatusCode, errors.New("cannot retain private commerce response")
		}
	}
	if resp.StatusCode != 200 {
		return data, resp.StatusCode, &StorageError{Stage: "purchase-skill-response", HTTPStatus: resp.StatusCode}
	}
	return data, resp.StatusCode, nil
}

func (s *StoreCommerceSession) skillBuy(ctx context.Context, q url.Values) (map[string]any, error) {
	values := map[string]any{}
	for key, vs := range q {
		if len(vs) != 1 {
			return nil, &StorageError{Stage: "purchase-confirmation-binding"}
		}
		if key == "storagePlanInBytes" || key == "salableAdamId" {
			n, err := strconv.ParseInt(vs[0], 10, 64)
			if err != nil {
				return nil, &StorageError{Stage: "purchase-skill-parameters"}
			}
			values[key] = n
		} else {
			values[key] = vs[0]
		}
	}
	values["guid"] = s.identity.GUID
	data, err := plist.Marshal(values, plist.XMLFormat)
	if err != nil {
		return nil, &StorageError{Stage: "purchase-skill-parameters"}
	}
	host := "buy.itunes.apple.com"
	if s.identity.Pod != "" {
		if _, err := strconv.ParseUint(s.identity.Pod, 10, 16); err != nil {
			return nil, &StorageError{Stage: "purchase-store-pod"}
		}
		host = "p" + s.identity.Pod + "-buy.itunes.apple.com"
	}
	body, status, err := s.skillRequest(ctx, http.MethodPost, "https://"+host+"/WebObjects/MZFinance.woa/wa/buyProduct?guid="+url.QueryEscape(s.identity.GUID), skillStoreAgent, "application/x-apple-plist", data)
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if _, err := plist.Unmarshal(body, &response); err != nil {
		return nil, &StorageError{Stage: "purchase-skill-decode", HTTPStatus: status}
	}
	return response, nil
}

// SubmitSkillPurchase sends the skill's native offer, and at most one exact
// Apple-issued auto-renew confirmation. It never uses presentOffers POST.
func (s *StoreCommerceSession) SubmitSkillPurchase(ctx context.Context, info *StoragePlanInfo, target PurchaseTarget, expectedTerms string) (*PurchaseResult, error) {
	if err := ValidatePurchaseTarget(info, target); err != nil {
		return nil, err
	}
	if _, _, ok := supported50GBTarget(target); !ok {
		return nil, &StorageError{Stage: "purchase-skill-offer"}
	}
	rawOffer, err := DirectStorageOfferURLForTarget(s.identity.GUID, target)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(rawOffer)
	q := u.Query()
	for k, v := range map[string]string{"hasAskedToFulfillPreorder": "true", "hasDoneAgeCheck": "true", "buyWithoutAuthorization": "true", "needDiv": "0", "origPage": "Software-1581031638", "origPageLocation": "Buy"} {
		q.Set(k, v)
	}
	r, err := s.skillBuy(ctx, q)
	if err != nil {
		return nil, err
	}
	if fmt.Sprint(r["failureType"]) == "3038" {
		if err := s.acceptSkillTerms(ctx, r, q, expectedTerms); err != nil {
			return nil, err
		}
		r, err = s.skillBuy(ctx, q)
		if err != nil {
			return nil, err
		}
	}
	if failure, ok := r["failureType"]; ok && fmt.Sprint(failure) != "" && fmt.Sprint(failure) != "0" {
		return nil, &StorageError{Stage: "purchase-skill-buy", HTTPStatus: 200, AppleCode: fmt.Sprint(failure)}
	}
	if dialog, ok := r["dialog"].(map[string]any); ok {
		action, _ := dialog["okButtonAction"].(map[string]any)
		fmt.Printf("Apple purchase dialog: id=%s; action=%s\n", safePurchaseField(dialog["dialogId"]), safePurchaseField(action["kind"]))
		// Some Store pods omit dialogId while retaining the authoritative Buy
		// action and Apple-issued buyParams. The action is the confirmation
		// contract; require it and validate the parameters below.
		if action["kind"] != "Buy" {
			return &PurchaseResult{State: "confirmation-required", HTTPStatus: 200}, nil
		}
		params, _ := action["buyParams"].(string)
		next, err := url.ParseQuery(params)
		if err != nil || len(params) > 65536 {
			return nil, &StorageError{Stage: "purchase-confirmation-binding"}
		}
		if err := validateNativeStorageConfirmation(next, q, s.identity); err != nil {
			return nil, err
		}
		next.Set("hasShownExtraConfirmationNeededForBuyRecurringItem", "true")
		r, err = s.skillBuy(ctx, next)
		if err != nil {
			return nil, err
		}
	}
	if failure, ok := r["failureType"]; ok && fmt.Sprint(failure) != "" && fmt.Sprint(failure) != "0" {
		return nil, &StorageError{Stage: "purchase-skill-buy", HTTPStatus: 200, AppleCode: fmt.Sprint(failure)}
	}
	if r["jingleDocType"] == "purchaseSuccess" && r["jingleAction"] == "purchaseProduct" && fmt.Sprint(r["status"]) == "0" {
		return &PurchaseResult{State: "completed", HTTPStatus: 200}, nil
	}
	return &PurchaseResult{State: "verification-pending", HTTPStatus: 200}, nil
}

func safePurchaseField(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 96 {
		return s[:96]
	}
	return s
}

func findSkillValue(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		if val, ok := x[key]; ok {
			return val
		}
		for _, val := range x {
			if r := findSkillValue(val, key); r != nil {
				return r
			}
		}
	case []any:
		for _, val := range x {
			if r := findSkillValue(val, key); r != nil {
				return r
			}
		}
	}
	return nil
}

func (s *StoreCommerceSession) acceptSkillTerms(ctx context.Context, r map[string]any, offer url.Values, expected string) error {
	if expected == "" {
		return &StorageError{Stage: "purchase-skill-terms-consent"}
	}
	host := "buy.itunes.apple.com"
	if s.identity.Pod != "" {
		host = "p" + s.identity.Pod + "-buy.itunes.apple.com"
	}
	base := "https://" + host + "/WebObjects/MZFinance.woa/wa/"
	data, _, err := s.skillRequest(ctx, http.MethodGet, base+"termsCheckSrv?guid="+url.QueryEscape(s.identity.GUID), skillStoreAgent, "", nil)
	if err != nil {
		return err
	}
	var state map[string]any
	if json.Unmarshal(data, &state) != nil || fmt.Sprint(findSkillValue(state, "latestTermsVersionId")) != expected {
		return &StorageError{Stage: "purchase-skill-terms-version"}
	}
	var page string
	var links func(any)
	links = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.Contains(x, "/termsPage?") {
				u, e := url.Parse(x)
				if e == nil && validStoreCommerceURL(u) {
					page = x
				}
			}
		case map[string]any:
			for _, v := range x {
				links(v)
			}
		case []any:
			for _, v := range x {
				links(v)
			}
		}
	}
	links(r)
	if page == "" {
		return &StorageError{Stage: "purchase-skill-terms-page"}
	}
	data, _, err = s.skillRequest(ctx, http.MethodGet, page, "AppStore/3.0 (Macintosh; OS X 15.4.1; 24E263)", "", nil)
	if err != nil {
		return err
	}
	for _, form := range parseStoreForms(data) {
		if !strings.Contains(form.action, "agreeToTermsUpdate") {
			continue
		}
		origin, _ := url.Parse(page)
		dest, err := storeFormURL(origin, data, form.action)
		if err != nil || dest.Query().Get("termsId") != expected || form.method != http.MethodPost {
			return &StorageError{Stage: "purchase-skill-terms-form"}
		}
		checkboxes := 0
		for i := range form.fields {
			if form.fields[i].typ == "checkbox" {
				form.fields[i].checked = true
				checkboxes++
			}
		}
		if checkboxes != 1 {
			return &StorageError{Stage: "purchase-skill-terms-controls"}
		}
		ack, _, err := s.skillRequest(ctx, http.MethodPost, dest.String(), "AppStore/3.0 (Macintosh; OS X 15.4.1; 24E263)", "application/x-www-form-urlencoded", []byte(encodeStoreForm(form, "")))
		if err != nil {
			return err
		}
		if !strings.Contains(string(ack), "You've accepted the new Terms of Service") {
			return &StorageError{Stage: "purchase-skill-terms-acknowledgement"}
		}
		return nil
	}
	return &StorageError{Stage: "purchase-skill-terms-form"}
}

type SkillSubscriptionInfo struct {
	SubscriptionID string `json:"subscription_id,omitempty"`
	Present        bool   `json:"present"`
	StorageAmount  string `json:"storage_amount,omitempty"`
	StorageUsage   string `json:"storage_usage,omitempty"`
	PaidPrice      string `json:"paid_price,omitempty"`
	PlanName       string `json:"plan_name,omitempty"`
	AutoRenew      bool   `json:"auto_renew"`
	Verified50GB   bool   `json:"verified_50gb"`
}

// MatchesTarget requires the storage amount and Store-formatted paid price to
// match the exact authorized offer.
func (info *SkillSubscriptionInfo) MatchesTarget(target PurchaseTarget) bool {
	if info == nil {
		return false
	}
	amount := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(info.StorageAmount, "\u00a0", ""), " ", ""))
	if amount != "50GB" {
		return false
	}
	price := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(info.PaidPrice, "\u00a0", ""), " ", ""))
	switch {
	case target.Currency == "CNY" && target.PriceMinor == 600:
		return price == "¥6.00" || price == "￥6.00" || price == "CNY6.00"
	case target.Currency == "USD" && target.PriceMinor == 99:
		return price == "$0.99" || price == "US$0.99" || price == "USD0.99"
	default:
		return false
	}
}

func (s *StoreCommerceSession) ReadSkillSubscription(ctx context.Context) (*SkillSubscriptionInfo, error) {
	data, _, err := s.skillRequest(ctx, http.MethodGet, "https://speedysub.itunes.apple.com/subscription/v3/manage/list", skillStoreAgent, "", nil)
	if err != nil {
		return nil, err
	}
	var list map[string]any
	if json.Unmarshal(data, &list) != nil {
		return nil, &StorageError{Stage: "purchase-skill-subscription-list"}
	}
	active, ok := findSkillValue(list, "active").([]any)
	if !ok {
		return nil, &StorageError{Stage: "purchase-skill-subscription-list"}
	}
	for _, entry := range active {
		raw, _ := json.Marshal(entry)
		if !strings.Contains(strings.ToLower(string(raw)), "icloud") && !strings.Contains(string(raw), storeSalableAdamID) {
			continue
		}
		id := findSkillValue(entry, "subscriptionId")
		if id == nil {
			return nil, &StorageError{Stage: "purchase-skill-subscription-id"}
		}
		data, _, err = s.skillRequest(ctx, http.MethodGet, "https://speedysub.itunes.apple.com/subscription/v3/manage/detail?subscriptionId="+url.QueryEscape(fmt.Sprint(id)), skillStoreAgent, "", nil)
		if err != nil {
			return nil, err
		}
		var detail map[string]any
		if json.Unmarshal(data, &detail) != nil {
			return nil, &StorageError{Stage: "purchase-skill-subscription-detail"}
		}
		info := &SkillSubscriptionInfo{Present: true}
		info.SubscriptionID = fmt.Sprint(id)
		info.StorageAmount, _ = findSkillValue(detail, "storageAmount").(string)
		info.StorageUsage, _ = findSkillValue(detail, "storageUsage").(string)
		plan, _ := findSkillValue(detail, "latestPlan").(map[string]any)
		info.PaidPrice, _ = plan["paidPrice"].(string)
		info.PlanName, _ = plan["displayName"].(string)
		var renewKnown bool
		info.AutoRenew, renewKnown = findSkillValue(detail, "isAutoRenewEnabled").(bool)
		if !renewKnown {
			return nil, &StorageError{Stage: "purchase-skill-subscription-renewal-unknown"}
		}
		info.Verified50GB = info.MatchesTarget(PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 600, Currency: "CNY", Period: "month"}) ||
			info.MatchesTarget(PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 99, Currency: "USD", Period: "month"})
		return info, nil
	}
	return &SkillSubscriptionInfo{}, nil
}

// CancelSkillAutoRenew requests cancellation and submits Apple's exact
// confirmation parameters. The service remains active until its expiry.
func (s *StoreCommerceSession) CancelSkillAutoRenew(ctx context.Context, sub *SkillSubscriptionInfo) error {
	if sub == nil || !sub.Present || sub.SubscriptionID == "" || !sub.AutoRenew {
		return &StorageError{Stage: "purchase-skill-cancel-precondition"}
	}
	q := url.Values{"subscriptionId": {sub.SubscriptionID}, "cancelSubscription": {"true"}, "salableAdamId": {storeSalableAdamID}}
	data, _, err := s.skillRequest(ctx, http.MethodPost, "https://buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/buyProduct?guid="+url.QueryEscape(s.identity.GUID), skillStoreAgent, "application/x-www-form-urlencoded", []byte(q.Encode()))
	if err != nil {
		return err
	}
	var first map[string]any
	if _, err := plist.Unmarshal(data, &first); err != nil {
		return &StorageError{Stage: "purchase-skill-cancel-decode"}
	}
	action, _ := findSkillValue(first, "okButtonAction").(map[string]any)
	if fmt.Sprint(action["kind"]) != "Buy" {
		return &StorageError{Stage: "purchase-skill-cancel-confirmation"}
	}
	params := fmt.Sprint(action["buyParams"])
	if params == "" || len(params) > 65536 {
		return &StorageError{Stage: "purchase-skill-cancel-confirmation"}
	}
	next, err := url.ParseQuery(params)
	if err != nil {
		return &StorageError{Stage: "purchase-skill-cancel-confirmation"}
	}
	next.Set("cancelSubscription", "true")
	next.Set("subscriptionId", sub.SubscriptionID)
	next.Set("salableAdamId", storeSalableAdamID)
	next.Set("guid", s.identity.GUID)
	next.Set("hasConfirmedBuySubscription", "true")
	data, _, err = s.skillRequest(ctx, http.MethodPost, "https://buy.itunes.apple.com/WebObjects/MZFinance.woa/wa/buyProduct?guid="+url.QueryEscape(s.identity.GUID), skillStoreAgent, "application/x-www-form-urlencoded", []byte(next.Encode()))
	if err != nil {
		return err
	}
	var result map[string]any
	if _, err := plist.Unmarshal(data, &result); err != nil {
		return &StorageError{Stage: "purchase-skill-cancel-decode"}
	}
	if fmt.Sprint(result["jingleDocType"]) != "purchaseSuccess" || fmt.Sprint(result["status"]) != "0" {
		return &StorageError{Stage: "purchase-skill-cancel-result"}
	}
	return nil
}
