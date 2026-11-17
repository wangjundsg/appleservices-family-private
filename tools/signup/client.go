// Pure-HTTP Apple Account creation: the wire client.
//
// No browser, no DOM, no WebKit. Every request below is one the account widget
// itself makes; the header set and the JSON bodies were lifted from a captured
// Chrome run (.cache/capture6/network.jsonl) and from the widget bundle
// (widget-*.js on appleid.cdn-apple.com), then reduced to the minimum that the
// server actually accepts.
//
// What the widget needs that a plain HTTP client must reproduce themselves:
//
//	scnt                     server-issued, rotates on every response header
//	X-Apple-ID-Session-Id    server-issued on the config call, stable after that
//	X-Apple-HC               SHA1 proof of work, see hashcash.go
//	captcha id + token       single use, from POST /captcha
//
// Preflight/captcha responses do not establish which inputs are required for
// registration. See docs/PROTOCOL-SIGNUP-AUDIT-2026-11-15.md for evidence limits.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

const (
	defaultBase      = "https://appleid.apple.com"
	portalBootstrap  = "https://account.apple.com/bootstrap/portal"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

// widgetKey is published by account.apple.com/bootstrap/portal as serviceKey.
// It has been stable for a long time, but Bootstrap refreshes it anyway.
const widgetKeyFallback = "af1139274f266b22b68c2a3e7ad932cb3c0bbe854e13a79af78dcc73136882c3"

// ProtocolError carries the HTTP status plus a trimmed body so a failed stage
// reads like Apple's own verdict instead of "status 400".
type ProtocolError struct {
	Stage  string
	Method string
	URL    string
	Status int
	Body   string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s: %s %s -> HTTP %d: %s", e.Stage, e.Method, e.URL, e.Status, e.Body)
}

// Session is one browser identity: cookies, scnt counter, session id and the
// hashcash challenge dropped by the last response.
type Session struct {
	base      string
	portal    string
	userAgent string
	locale    string
	timezone  string
	widgetKey string
	client    *http.Client

	scnt          string
	sessionID     string
	hcBits        int
	hcChallenge   string
	fdClientInfo  string
	profileHints  *fingerprint.ClientHints
	adiHeaders    map[string]string
	engineApplied bool
	lastWarmupURL string
	lastRequest   string

	// Transport switches describe the requested engine, not server acceptance.
	UseCurl     bool
	SkipLanding bool
	CHHints     bool
	HTTP2       bool
	proxyURL    string
	curlJar     string
	direct      bool
}

// SessionOptions configures the transport and the browser identity it claims.
type SessionOptions struct {
	Base         string
	UserAgent    string
	Locale       string
	Timezone     string
	Proxy        string
	FDClientInfo string
	HTTP2        bool
	SkipLanding  bool
	CHHints      bool
	UseCurl      bool
	Timeout      time.Duration
	Direct       bool
}

func newSession(opts SessionOptions) (*Session, error) {
	jar, err := newRecordedJar(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: opts.HTTP2}
	if opts.Direct {
		transport.Proxy = nil
	}
	if strings.TrimSpace(opts.Proxy) != "" {
		parsed, err := url.Parse(strings.TrimSpace(opts.Proxy))
		if err != nil {
			return nil, fmt.Errorf("invalid proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	// Keep protocol selection explicit; a 404 alone does not identify its cause.
	if !opts.HTTP2 {
		transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	if opts.Base == "" {
		opts.Base = defaultBase
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}
	if opts.Locale == "" {
		opts.Locale = "en-US"
	}
	if opts.Timezone == "" {
		opts.Timezone = "America/New_York"
	}
	if opts.FDClientInfo != "" {
		var fd struct {
			UserAgent string `json:"U"`
			Locale    string `json:"L"`
		}
		if json.Unmarshal([]byte(opts.FDClientInfo), &fd) != nil || fd.UserAgent != opts.UserAgent || fd.Locale != opts.Locale {
			return nil, errors.New("FD profile must be valid JSON with U/L matching the configured User-Agent and locale")
		}
	}
	if opts.CHHints && (len(chromeMajor.FindStringSubmatch(opts.UserAgent)) != 2 || !strings.Contains(opts.UserAgent, "Windows NT") || strings.Contains(opts.UserAgent, "Mobile")) {
		return nil, errors.New("client hints only support the configured Windows desktop Chrome profile; disable -ch-hints for other clients")
	}
	return &Session{
		adiHeaders:   adiHeadersFromEnv(),
		base:         strings.TrimSuffix(opts.Base, "/"),
		portal:       portalURLFor(strings.TrimSuffix(opts.Base, "/")),
		userAgent:    opts.UserAgent,
		locale:       opts.Locale,
		timezone:     opts.Timezone,
		widgetKey:    widgetKeyFallback,
		fdClientInfo: opts.FDClientInfo,
		client:       &http.Client{Transport: transport, Jar: jar, Timeout: opts.Timeout},
		UseCurl:      opts.UseCurl, SkipLanding: opts.SkipLanding, CHHints: opts.CHHints, HTTP2: opts.HTTP2,
		proxyURL: strings.TrimSpace(opts.Proxy),
		direct:   opts.Direct,
	}, nil
}

var chromeMajor = regexp.MustCompile(`Chrome/(\d+)\.`)

// portalURLFor keeps the real portal for the real host and follows the test
// server otherwise, so the offline suite exercises the same code path.
func portalURLFor(base string) string {
	if base == "" || base == defaultBase {
		return portalBootstrap
	}
	return base + "/bootstrap/portal"
}

// headers builds the widget's header set. The values that must rotate are read
// from the session at call time, never cached in a struct.
// adiHeaders parses the optional research hook once per session.
func adiHeadersFromEnv() map[string]string {
	raw := strings.TrimSpace(os.Getenv("APPLESERVICES_ADI_HEADERS"))
	if raw == "" {
		return nil
	}
	var parsed map[string]string
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return nil
	}
	return parsed
}

func (s *Session) headers(contentType string, hashcash bool, now time.Time) (http.Header, error) {
	h := http.Header{}
	h.Set("User-Agent", s.userAgent)
	h.Set("Accept-Language", s.locale)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Referer", s.base+"/")
	h.Set("Origin", s.base)
	h.Set("X-Apple-Widget-Key", s.widgetKey)
	h.Set("X-Apple-I-Request-Context", "account")
	h.Set("X-Apple-I-Timezone", s.timezone)
	if s.CHHints {
		version := chromeMajor.FindStringSubmatch(s.userAgent)
		if len(version) != 2 {
			return nil, errors.New("client hints require a Chrome version in User-Agent")
		}
		h.Set("Sec-Ch-Ua", fmt.Sprintf(`"Not-A.Brand";v="99", "Google Chrome";v="%s", "Chromium";v="%s"`, version[1], version[1]))
		h.Set("Sec-Ch-Ua-Mobile", "?0")
		h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
		h.Set("Sec-Fetch-Dest", "empty")
		h.Set("Sec-Fetch-Mode", "cors")
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Priority", "u=1, i")
	}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if s.scnt != "" {
		h.Set("scnt", s.scnt)
	}
	if s.sessionID != "" {
		h.Set("X-Apple-ID-Session-Id", s.sessionID)
	}
	if s.fdClientInfo != "" {
		h.Set("X-Apple-I-FD-Client-Info", s.fdClientInfo)
	}
	if hashcash {
		stamp, err := hashcashStamp(s.hcBits, s.hcChallenge, now)
		if err != nil {
			return nil, err
		}
		h.Set("X-Apple-HC", stamp)
	}
	// Research hook: an ADI header set may be injected to measure whether Apple
	// treats the request as a device client. It is read from the environment once
	// and never persisted, so a normal run is unaffected.
	for name, value := range s.adiHeaders {
		h.Set(name, value)
	}
	return h, nil
}

// absorb copies the rotating server state out of a response header set. scnt
// is re-issued on all of them; dropping it is exactly the defect that made an
// earlier pure-HTTP attempt answer 403.
func (s *Session) absorb(header http.Header) {
	if v := strings.TrimSpace(header.Get("scnt")); v != "" {
		s.scnt = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-ID-Session-Id")); v != "" {
		s.sessionID = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-HC-Bits")); v != "" {
		var bits int
		if _, err := fmt.Sscanf(v, "%d", &bits); err == nil && bits > 0 {
			s.hcBits = bits
		}
	}
	if v := strings.TrimSpace(header.Get("X-Apple-HC-Challenge")); v != "" {
		s.hcChallenge = v
	}
}

// send selects exactly one transport; it never retries through another engine.
func (s *Session) send(ctx context.Context, method, url string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	if s.UseCurl {
		return s.curlRoundTrip(ctx, method, url, header, body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = header
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// do performs one call, absorbs server state and returns the raw body.
func (s *Session) do(ctx context.Context, stage, method, path string, body any, hashcash bool) ([]byte, error) {
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = raw
	}
	url := s.base + path
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	header, err := s.headers(contentType, hashcash, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stage, err)
	}

	status, respHeader, raw, err := s.send(ctx, method, url, header, payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stage, err)
	}
	s.absorb(respHeader)
	if status < 200 || status > 299 {
		return nil, &ProtocolError{Stage: stage, Method: method, URL: url, Status: status, Body: clip(string(raw), 600)}
	}
	var verdict struct {
		HasError           bool              `json:"hasError"`
		Errors             []json.RawMessage `json:"errors"`
		ServiceErrors      []json.RawMessage `json:"service_errors"`
		ServiceErrorsCamel []json.RawMessage `json:"serviceErrors"`
		ValidationErrors   []json.RawMessage `json:"validationErrors"`
	}
	if json.Unmarshal(raw, &verdict) == nil && (verdict.HasError || len(verdict.Errors)+len(verdict.ServiceErrors)+len(verdict.ServiceErrorsCamel)+len(verdict.ValidationErrors) > 0) {
		return nil, &ProtocolError{Stage: stage, Method: method, URL: url, Status: status, Body: clip(string(raw), 600)}
	}
	return raw, nil
}

// refreshWidgetKey pulls the live serviceKey. A failure is not fatal: the
// known-good constant is used and the caller keeps going.
func (s *Session) refreshWidgetKey(ctx context.Context) error {
	header := http.Header{}
	header.Set("User-Agent", s.userAgent)
	header.Set("Accept", "application/json")
	status, _, raw, err := s.send(ctx, http.MethodGet, s.portal, header, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("portal bootstrap HTTP %d", status)
	}
	var portal struct {
		ServiceKey string `json:"serviceKey"`
	}
	if err := json.Unmarshal(raw, &portal); err != nil {
		return err
	}
	if portal.ServiceKey != "" {
		s.widgetKey = portal.ServiceKey
	}
	return nil
}

// getAbsolute performs a GET against a full URL on the same cookie jar. The
// portal lives on account.apple.com while every API call lives on
// appleid.apple.com, and the jar has to carry cookies across both.
func (s *Session) getAbsolute(ctx context.Context, stage, url string) error {
	header := http.Header{}
	header.Set("User-Agent", s.userAgent)
	header.Set("Accept-Language", s.locale)
	header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	status, respHeader, _, err := s.send(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	s.absorb(respHeader)
	if status < 200 || status > 299 {
		return &ProtocolError{Stage: stage, Method: http.MethodGet, URL: url, Status: status}
	}
	return nil
}

// Bootstrap establishes the session: cookies from the portal, then the create
// page config, whose response headers carry scnt, the session id and the
// hashcash challenge. Nothing here writes anything on Apple's side.
func (s *Session) Bootstrap(ctx context.Context, countryISO3 string) error {
	// A warm-up (or a restored session) may already hold server state; the
	// browser path must not be walked twice within one task.
	if s.scnt != "" && s.sessionID != "" {
		return nil
	}
	if countryISO3 == "" {
		countryISO3 = "USA"
	}
	if err := s.refreshWidgetKey(ctx); err != nil {
		warnf("portal bootstrap: %v (using built-in widget key)", err)
	}
	// The landing page is the cheapest way to collect the portal cookies. It is
	// rate limited by IP (503, docs/signup-protocol.md §8) and the config call
	// works without it, so a failure here is reported and ignored.
	landing := strings.Replace(s.base, "//appleid.", "//account.", 1) + "/account"
	if !s.SkipLanding {
		if err := s.getAbsolute(ctx, "landing", landing); err != nil {
			warnf("landing page: %v (continuing)", err)
		}
	}
	// The config call is the one request Apple's edge is fussy about: a bare
	// UA/Accept pair passes while the full widget header set answers 404 with
	// www.apple.com/errors/us_error.html. Measured 2026-11-15, same exit IP.
	header := http.Header{}
	header.Set("User-Agent", s.userAgent)
	header.Set("Accept-Language", s.locale)
	header.Set("Accept", "application/json, text/plain, */*")
	url := s.base + "/account?countryCode=" + url.QueryEscape(countryISO3)
	status, respHeader, raw, err := s.send(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	s.absorb(respHeader)
	if status < 200 || status > 299 {
		return &ProtocolError{Stage: "bootstrap", Method: http.MethodGet, URL: url, Status: status, Body: clip(string(raw), 200)}
	}
	if s.scnt == "" {
		return errors.New("bootstrap: server returned no scnt header; the portal layout changed")
	}
	return nil
}

// EmailFree asks the create form's own preflight whether an address is usable.
func (s *Session) EmailFree(ctx context.Context, email string) (valid, used bool, err error) {
	raw, err := s.do(ctx, "preflight", http.MethodPost, "/account/validation/appleid",
		map[string]string{"emailAddress": email}, false)
	if err != nil {
		return false, false, err
	}
	var verdict struct {
		Valid *bool `json:"valid"`
		Used  *bool `json:"used"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return false, false, fmt.Errorf("preflight: response is not JSON: %s", clip(string(raw), 200))
	}
	if verdict.Valid == nil {
		return false, false, fmt.Errorf("preflight: no verdict: %s", clip(string(raw), 200))
	}
	free := *verdict.Valid && (verdict.Used == nil || !*verdict.Used)
	if verdict.Used != nil {
		used = *verdict.Used
	}
	return free, used, nil
}

// CheckPassword runs the widget's inline password policy check (204 = fine).
func (s *Session) CheckPassword(ctx context.Context, email, password, first, last string) error {
	_, err := s.do(ctx, "password", http.MethodPost, "/account/validate/password", map[string]any{
		"password":    password,
		"accountName": email,
		"updating":    false,
		"firstName":   first,
		"lastName":    last,
	}, false)
	return err
}

// RequestCaptcha fetches one image challenge.
func (s *Session) RequestCaptcha(ctx context.Context) (*captcha, error) {
	raw, err := s.do(ctx, "captcha", http.MethodPost, "/captcha", map[string]string{"type": "IMAGE"}, false)
	if err != nil {
		return nil, err
	}
	var c captcha
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("captcha: response is not JSON: %s", clip(string(raw), 200))
	}
	if err := c.decodeImage(); err != nil {
		return nil, err
	}
	if c.Token == "" {
		return nil, errors.New("captcha: response carries no token")
	}
	return &c, nil
}

// Account is the identity the create call submits.
type Account struct {
	Email       string
	Password    string
	FirstName   string
	LastName    string
	Birthday    string // yyyy-mm-dd
	CountryISO3 string // USA
	Language    string // en_US
}

// Phone is the number the account is created with (SMS verification target).
type Phone struct {
	Number       string // as displayed, e.g. (415) 555-0134
	CountryCode2 string // US
	DialCode     string // 1
}

// createBody is the exact shape the widget builds in `Hi(...)`.
func createBody(a Account, p Phone, c *captcha, answer string) map[string]any {
	language := a.Language
	if language == "" {
		language = "en_US"
	}
	country := a.CountryISO3
	if country == "" {
		country = "USA"
	}
	return map[string]any{
		"phoneNumberVerification": map[string]any{
			"phoneNumber": map[string]any{
				"id":              1,
				"number":          p.Number,
				"countryCode":     p.CountryCode2,
				"countryDialCode": p.DialCode,
				"nonFTEU":         true,
			},
			"mode": "sms",
		},
		"account": map[string]any{
			"name":     a.Email,
			"password": a.Password,
			"person": map[string]any{
				"name":           map[string]string{"firstName": a.FirstName, "lastName": a.LastName},
				"birthday":       a.Birthday,
				"primaryAddress": map[string]string{"country": country},
			},
			"preferences": map[string]any{
				"preferredLanguage":    language,
				"marketingPreferences": map[string]bool{"appleNews": false, "appleUpdates": true, "iTunesUpdates": true},
			},
			"verificationInfo": map[string]string{"id": "", "answer": ""},
		},
		"captcha":              map[string]any{"id": c.ID, "token": c.Token, "answer": strings.ToUpper(answer)},
		"privacyPolicyChecked": false,
	}
}

// Create validates the submitted form. Acceptance does not prove that email,
// phone verification or final account creation succeeded.
func (s *Session) Create(ctx context.Context, a Account, p Phone, c *captcha, answer string) ([]byte, error) {
	return s.do(ctx, "create", http.MethodPost, "/account/validate", createBody(a, p, c, answer), true)
}

// SendEmailCode uses the primary email flow. Rejections must not trigger a
// second send on the unrelated contactEmail branch.
func (s *Session) SendEmailCode(ctx context.Context, email string) ([]byte, error) {
	body := map[string]any{"name": email}
	return s.do(ctx, "email-send", http.MethodPost, "/account/verification", body, false)
}

// SubmitEmailCode answers that step.
func (s *Session) SubmitEmailCode(ctx context.Context, email, code string) ([]byte, error) {
	body := map[string]any{
		"name":             email,
		"verificationInfo": map[string]string{"id": "", "answer": code},
	}
	return s.do(ctx, "email-submit", http.MethodPut, "/account/verification", body, false)
}

// phoneVerification is the shared wrapper both phone steps submit.
func phoneVerification(p Phone, code string) map[string]any {
	number := map[string]any{
		"id":              1,
		"number":          p.Number,
		"countryCode":     p.CountryCode2,
		"countryDialCode": p.DialCode,
		"nonFTEU":         true,
	}
	if code != "" {
		number["securityCode"] = map[string]string{"code": code}
	}
	return map[string]any{"phoneNumberVerification": map[string]any{"phoneNumber": number, "mode": "sms"}}
}

// SendPhoneCode triggers the SMS. hashcash is required on this path.
func (s *Session) SendPhoneCode(ctx context.Context, p Phone) ([]byte, error) {
	return s.do(ctx, "phone-send", http.MethodPost, "/account/verification/phone", phoneVerification(p, ""), true)
}

// SubmitPhoneCode answers the SMS step. hashcash is required here too.
func (s *Session) SubmitPhoneCode(ctx context.Context, p Phone, code string) ([]byte, error) {
	return s.do(ctx, "phone-submit", http.MethodPut, "/account/verification/phone", phoneVerification(p, code), true)
}

// GeneratedPassword returns a password that satisfies Apple's four-class rule
// (docs/REGISTRAR-CONTRACT.md A-1: a missing special character silently
// disables the Continue button).
func GeneratedPassword(length int) string {
	if length < 12 {
		length = 12
	}
	const (
		upper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower   = "abcdefghijkmnopqrstuvwxyz"
		digits  = "23456789"
		special = "!@#$%^&*-_=+?"
	)
	index := func(n int) int {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		if err != nil {
			panic("secure randomness unavailable")
		}
		return int(v.Int64())
	}
	out := make([]byte, 0, length)
	out = append(out, upper[index(len(upper))], lower[index(len(lower))],
		digits[index(len(digits))], special[index(len(special))])
	all := upper + lower + digits + special
	for len(out) < length {
		out = append(out, all[index(len(all))])
	}
	for i := len(out) - 1; i > 0; i-- {
		j := index(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func clip(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// localDialCode keeps the create body honest when the caller only knows the
// ISO2 code: Apple compares the dial code against the country server side.
func localDialCode(iso2 string) string {
	switch strings.ToUpper(iso2) {
	case "US", "CA":
		return "1"
	case "GB":
		return "44"
	case "AU":
		return "61"
	case "DE":
		return "49"
	case "FR":
		return "33"
	case "SG":
		return "65"
	case "HK":
		return "852"
	case "JP":
		return "81"
	case "CN":
		return "86"
	case "ID":
		return "62"
	case "VN":
		return "84"
	default:
		return ""
	}
}
