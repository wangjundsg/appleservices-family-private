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
// What it turned out NOT to need (measured, see docs/PURE-PROTOCOL-REGISTRAR.md):
// X-Apple-I-FD-Client-Info / fdcBrowserData, jslog/jslogs telemetry, the
// landing-page cookie chain, and X-Apple-Api-Key.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
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
	base         string
	portal       string
	userAgent    string
	locale       string
	timezone     string
	widgetKey    string
	client       *http.Client
	scnt         string
	sessionID    string
	hcBits       int
	hcChallenge  string
	fdClientInfo string
	lastRequest  string
}

// SessionOptions configures the transport and the browser identity it claims.
type SessionOptions struct {
	Base         string
	UserAgent    string
	Locale       string
	Timezone     string
	Proxy        string
	FDClientInfo string
	Timeout      time.Duration
}

func newSession(opts SessionOptions) (*Session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if strings.TrimSpace(opts.Proxy) != "" {
		parsed, err := url.Parse(strings.TrimSpace(opts.Proxy))
		if err != nil {
			return nil, fmt.Errorf("invalid proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
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
	return &Session{
		base:         strings.TrimSuffix(opts.Base, "/"),
		portal:       portalURLFor(strings.TrimSuffix(opts.Base, "/")),
		userAgent:    opts.UserAgent,
		locale:       opts.Locale,
		timezone:     opts.Timezone,
		widgetKey:    widgetKeyFallback,
		fdClientInfo: opts.FDClientInfo,
		client:       &http.Client{Transport: transport, Jar: jar, Timeout: opts.Timeout},
	}, nil
}

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
	return h, nil
}

// absorb copies the rotating server state out of every response. scnt is
// re-issued on all of them; dropping it is exactly the defect that made an
// earlier pure-HTTP attempt answer 403.
func (s *Session) absorb(resp *http.Response) {
	if v := strings.TrimSpace(resp.Header.Get("scnt")); v != "" {
		s.scnt = v
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-ID-Session-Id")); v != "" {
		s.sessionID = v
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-HC-Bits")); v != "" {
		var bits int
		if _, err := fmt.Sscanf(v, "%d", &bits); err == nil && bits > 0 {
			s.hcBits = bits
		}
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-HC-Challenge")); v != "" {
		s.hcChallenge = v
	}
}

// do performs one call, absorbs server state and returns the raw body.
func (s *Session) do(ctx context.Context, stage, method, path string, body any, hashcash bool) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(raw)
	}
	url := s.base + path
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, err
	}
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	header, err := s.headers(contentType, hashcash, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stage, err)
	}
	req.Header = header

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stage, err)
	}
	defer resp.Body.Close()
	s.absorb(resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", stage, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ProtocolError{Stage: stage, Method: method, URL: url, Status: resp.StatusCode, Body: clip(string(raw), 600)}
	}
	return raw, nil
}

// refreshWidgetKey pulls the live serviceKey. A failure is not fatal: the
// known-good constant is used and the caller keeps going.
func (s *Session) refreshWidgetKey(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.portal, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("portal bootstrap HTTP %d", resp.StatusCode)
	}
	var portal struct {
		ServiceKey string `json:"serviceKey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&portal); err != nil {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept-Language", s.locale)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", stage, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
	s.absorb(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ProtocolError{Stage: stage, Method: http.MethodGet, URL: url, Status: resp.StatusCode}
	}
	return nil
}

// Bootstrap establishes the session: cookies from the portal, then the create
// page config, whose response headers carry scnt, the session id and the
// hashcash challenge. Nothing here writes anything on Apple's side.
func (s *Session) Bootstrap(ctx context.Context, countryISO3 string) error {
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
	if err := s.getAbsolute(ctx, "landing", landing); err != nil {
		warnf("landing page: %v (continuing)", err)
	}
	if _, err := s.do(ctx, "bootstrap", http.MethodGet, "/account?countryCode="+url.QueryEscape(countryISO3), nil, false); err != nil {
		return err
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

// Create submits the form. A 200 here means Apple accepted the account and is
// about to mail/sms the first verification code; it is the only call that
// writes anything.
func (s *Session) Create(ctx context.Context, a Account, p Phone, c *captcha, answer string) ([]byte, error) {
	return s.do(ctx, "create", http.MethodPost, "/account/validate", createBody(a, p, c, answer), true)
}

// SendEmailCode triggers the "we mailed you a code" step. The widget uses
// /account/verification and the contactEmail variant for the alternate-address
// branch; the first one that answers 2xx wins.
func (s *Session) SendEmailCode(ctx context.Context, email string) ([]byte, error) {
	body := map[string]any{"name": email}
	raw, err := s.do(ctx, "email-send", http.MethodPost, "/account/verification", body, false)
	if err == nil {
		return raw, nil
	}
	if pe, ok := err.(*ProtocolError); ok && pe.Status >= 400 && pe.Status < 500 {
		if alt, altErr := s.do(ctx, "email-send", http.MethodPost, "/account/verification/contactEmail", body, false); altErr == nil {
			return alt, nil
		}
	}
	return nil, err
}

// SubmitEmailCode answers that step.
func (s *Session) SubmitEmailCode(ctx context.Context, email, code string) ([]byte, error) {
	body := map[string]any{
		"name":             email,
		"verificationInfo": map[string]string{"id": "", "answer": code},
	}
	raw, err := s.do(ctx, "email-submit", http.MethodPut, "/account/verification", body, false)
	if err == nil {
		return raw, nil
	}
	if pe, ok := err.(*ProtocolError); ok && pe.Status >= 400 && pe.Status < 500 {
		if alt, altErr := s.do(ctx, "email-submit", http.MethodPut, "/account/verification/contactEmail", body, false); altErr == nil {
			return alt, nil
		}
	}
	return nil, err
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
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(rand.Int())))
	out := make([]byte, 0, length)
	out = append(out, upper[rnd.Intn(len(upper))], lower[rnd.Intn(len(lower))],
		digits[rnd.Intn(len(digits))], special[rnd.Intn(len(special))])
	all := upper + lower + digits + special
	for len(out) < length {
		out = append(out, all[rnd.Intn(len(all))])
	}
	rnd.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
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
	default:
		return "1"
	}
}
