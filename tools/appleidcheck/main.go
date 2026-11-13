// Command appleidcheck probes Apple's Apple Account creation preflight.
//
// It fetches https://appleid.apple.com/account, extracts the per-session
// anti-CSRF material the create form uses (`scnt`, `apikey`, `sessionid`), and
// asks Apple whether a candidate address is usable as a new Apple Account
// username. Apple answers with `valid`, `used`, `appleOwnedDomain` and
// `isRecycledDomain`; the last two are the domain-reputation signals that
// decide whether an email plan is worth building on.
//
// This tool creates nothing, submits no password and no code. It is a
// read-only preflight: one page load plus one validation POST per address.
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const accountPage = "https://appleid.apple.com/account"
const validateURL = "https://appleid.apple.com/account/validation/appleid"
const portalBootstrapURL = "https://account.apple.com/bootstrap/portal"

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// bootstrap values are pulled out of the create page. Apple has shipped both
// quoted-JSON and single-quoted JS forms, so both are accepted.
var bootstrapPatterns = map[string]*regexp.Regexp{
	"scnt":      regexp.MustCompile(`(?i)\bscnt\b['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	"apikey":    regexp.MustCompile(`(?i)\bauth?apikey\b['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
	"sessionid": regexp.MustCompile(`(?i)\bsession[_-]?id\b['"]?\s*[:=]\s*['"]([^'"]+)['"]`),
}

type bootstrap struct {
	scnt, apikey, sessionID, widgetKey string
}

// portalBootstrap is served by account.apple.com and names the AuthKit widget
// the create form runs inside. Its serviceKey is the widget key every create
// call must carry.
type portalBootstrap struct {
	APIURL     string `json:"apiUrl"`
	ServiceKey string `json:"serviceKey"`
	ServiceURL string `json:"serviceUrl"`
	Env        string `json:"environment"`
}

type verdict struct {
	Address          string `json:"address"`
	HTTPStatus       int    `json:"httpStatus"`
	Valid            *bool  `json:"valid,omitempty"`
	Used             *bool  `json:"used,omitempty"`
	AppleOwnedDomain *bool  `json:"appleOwnedDomain,omitempty"`
	RecycledDomain   *bool  `json:"isRecycledDomain,omitempty"`
	Error            string `json:"error,omitempty"`
	Raw              string `json:"raw,omitempty"`
}

func main() {
	emails := flag.String("emails", "", "comma separated addresses to check")
	domain := flag.String("domain", "", "generate addresses on this domain instead of -emails")
	count := flag.Int("count", 3, "how many addresses to generate for -domain")
	prefix := flag.String("prefix", "", "fixed local part for -domain (default random)")
	proxy := flag.String("proxy", "", "explicit HTTP proxy URL; default honours HTTP(S)_PROXY")
	locale := flag.String("locale", "en-US", "browser locale reported in X-Apple-I-FD-Client-Info")
	tz := flag.String("tz", "America/New_York", "browser timezone reported in X-Apple-I-FD-Client-Info")
	asJSON := flag.Bool("json", false, "print one JSON object per address")
	dump := flag.String("dump", "", "write the fetched account page to this path for inspection")
	timeout := flag.Duration("timeout", 30*time.Second, "per request timeout")
	flag.Parse()

	list, err := addresses(*emails, *domain, *prefix, *count)
	if err != nil {
		fmt.Fprintln(os.Stderr, "appleidcheck:", err)
		os.Exit(2)
	}

	client, err := newClient(*proxy, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "appleidcheck:", err)
		os.Exit(2)
	}

	boot, err := loadBootstrap(client, *locale, *dump)
	if err != nil {
		fmt.Fprintln(os.Stderr, "appleidcheck:", err)
		os.Exit(1)
	}
	if !*asJSON {
		fmt.Printf("bootstrap ok: scnt=%s apikey=%s sessionid=%s\n",
			mask(boot.scnt), mask(boot.apikey), mask(boot.sessionID))
	}

	failed := 0
	for _, address := range list {
		v := check(client, boot, address, *locale, *tz)
		if v.Error != "" || v.Valid == nil {
			failed++
		}
		if *asJSON {
			line, _ := json.Marshal(v)
			fmt.Println(string(line))
			continue
		}
		fmt.Println(format(v))
	}
	if failed > 0 && failed == len(list) {
		os.Exit(1)
	}
}

func addresses(raw, domain, prefix string, count int) ([]string, error) {
	var list []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			list = append(list, part)
		}
	}
	if strings.TrimSpace(domain) != "" {
		domain = strings.TrimPrefix(strings.TrimSpace(domain), "@")
		if count < 1 || count > 50 {
			return nil, errors.New("-count must be between 1 and 50")
		}
		for i := 0; i < count; i++ {
			local := prefix
			if local == "" {
				local = randomLocal()
			}
			if i > 0 {
				local = fmt.Sprintf("%s%d", local, i)
			}
			list = append(list, local+"@"+domain)
		}
	}
	if len(list) == 0 {
		return nil, errors.New("nothing to check: pass -emails or -domain")
	}
	return list, nil
}

func randomLocal() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	r := mathrand.New(mathrand.NewSource(time.Now().UnixNano()))
	var b strings.Builder
	b.WriteString("preflight.")
	for i := 0; i < 10; i++ {
		b.WriteByte(alphabet[r.Intn(len(alphabet))])
	}
	return b.String()
}

func newClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid -proxy: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Jar: jar, Timeout: timeout}, nil
}

func loadBootstrap(client *http.Client, locale, dump string) (bootstrap, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, accountPage, nil)
	if err != nil {
		return bootstrap{}, err
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", locale+","+locale[:2]+";q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return bootstrap{}, fmt.Errorf("get account page: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return bootstrap{}, fmt.Errorf("read account page: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return bootstrap{}, fmt.Errorf("account page returned HTTP %d (%d bytes)", resp.StatusCode, len(body))
	}
	if dump != "" {
		if err := os.WriteFile(dump, body, 0o600); err != nil {
			return bootstrap{}, fmt.Errorf("write account page dump: %w", err)
		}
	}

	var boot bootstrap
	for name, pattern := range bootstrapPatterns {
		match := pattern.FindSubmatch(body)
		if len(match) != 2 {
			continue
		}
		switch name {
		case "scnt":
			boot.scnt = string(match[1])
		case "apikey":
			boot.apikey = string(match[1])
		case "sessionid":
			boot.sessionID = string(match[1])
		}
	}
	// Only scnt is mandatory: the current account portal publishes it in
	// app_config and pushes X-Apple-Api-Key later, after /account/manageData/get.
	// apikey/sessionid are optional and sent only when the page happened to
	// expose them.
	if boot.scnt == "" {
		return bootstrap{}, fmt.Errorf("account page exposes no scnt (%d bytes); the portal layout changed", len(body))
	}

	// The create form is the AuthKit widget; every create call carries its
	// serviceKey as X-Apple-Widget-Key. It is published by the portal bootstrap.
	portalReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, portalBootstrapURL, nil)
	portalReq.Header.Set("User-Agent", defaultUA)
	portalReq.Header.Set("Accept", "application/json")
	portalResp, err := client.Do(portalReq)
	if err != nil {
		return bootstrap{}, fmt.Errorf("get portal bootstrap: %w", err)
	}
	portalBody, _ := io.ReadAll(io.LimitReader(portalResp.Body, 1<<20))
	portalResp.Body.Close()
	if portalResp.StatusCode != http.StatusOK {
		return bootstrap{}, fmt.Errorf("portal bootstrap returned HTTP %d", portalResp.StatusCode)
	}
	var portal portalBootstrap
	if err := json.Unmarshal(portalBody, &portal); err != nil {
		return bootstrap{}, fmt.Errorf("portal bootstrap is not JSON: %w", err)
	}
	boot.widgetKey = portal.ServiceKey
	if boot.widgetKey == "" {
		return bootstrap{}, errors.New("portal bootstrap carries no serviceKey")
	}

	// The widget mints its own session id; any 128-byte hex value is accepted
	// as long as it stays constant for the flow.
	if boot.sessionID == "" {
		raw := make([]byte, 128)
		if _, err := crand.Read(raw); err != nil {
			return bootstrap{}, err
		}
		boot.sessionID = strings.ToUpper(hex.EncodeToString(raw))
	}
	return boot, nil
}

func check(client *http.Client, boot bootstrap, address, locale, tz string) verdict {
	v := verdict{Address: address}
	payload, _ := json.Marshal(map[string]string{"emailAddress": address})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, validateURL, bytes.NewReader(payload))
	if err != nil {
		v.Error = err.Error()
		return v
	}
	// Header set copied from a captured Chrome run of the create widget.
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", locale)
	req.Header.Set("Origin", "https://appleid.apple.com")
	req.Header.Set("Referer", "https://appleid.apple.com/")
	req.Header.Set("X-Apple-I-Request-Context", "account")
	req.Header.Set("X-Apple-Widget-Key", boot.widgetKey)
	req.Header.Set("X-Apple-ID-Session-Id", boot.sessionID)
	req.Header.Set("X-Apple-I-Timezone", tz)
	req.Header.Set("scnt", boot.scnt)
	if boot.apikey != "" {
		req.Header.Set("X-Apple-Api-Key", boot.apikey)
	}
	req.Header.Set("X-Apple-I-FD-Client-Info", fdClientInfo(defaultUA, locale, "GMT-05:00"))

	resp, err := client.Do(req)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	v.HTTPStatus = resp.StatusCode
	v.Raw = trim(string(body), 400)

	if resp.StatusCode != http.StatusOK {
		v.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return v
	}
	var decoded struct {
		Valid            *bool  `json:"valid"`
		Used             *bool  `json:"used"`
		AppleOwnedDomain *bool  `json:"appleOwnedDomain"`
		RecycledDomain   *bool  `json:"isRecycledDomain"`
		Error            string `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		v.Error = "response is not JSON"
		return v
	}
	v.Valid, v.Used = decoded.Valid, decoded.Used
	v.AppleOwnedDomain, v.RecycledDomain = decoded.AppleOwnedDomain, decoded.RecycledDomain
	if decoded.Error != "" {
		v.Error = decoded.Error
	}
	return v
}

// fdClientInfo mirrors what the browser sends: the UA, locale, timezone, a
// version tag and a device fingerprint blob. Apple does not document it.
func fdClientInfo(ua, locale, tz string) string {
	const fingerprint = "kWa4j1e3NlY5BSo9z4ofjb75PaK4Vpjt1szHVXU3vtnTDovyhbQ3zIRbPhj03x.dXnYbFdH6PEwHXXTSHCSPmtd0wVYPIG_qvoPfybYb5EvYTrYesR2hQnH6hAOfUftmF5wMQE5nXxQD6CKaMhQWx2KlTTpZHgfLMC7AeLd7FmrpwoNN5uQ4s5uQ1szHVzZrefOJQuyPB94UXuGlfUm4ly_03y8rnawSdrxQDq8yatDpQT18u0I4b6DWHZDoUs_43wuZPup_nH2t05oaYAhrcpMxE6DBUr5xj6Kks6PraZEPLnLzMnZVCsiqTCb4uqRDPfBjPr2u5fXwxf7_OLgiPFMqbN0TUMpwoNRe98vPSb_GGEOpBSKxUC56MnGWpwoNSUC53ZXnN87gq1a293lio8dOHr_i.uJtHoqvynx9MsFyxYMH0XKJ7lrHay.9aB.KB4D93tG2hixAwlMsITxYMJ5uFBHeNNW5BNlYicklY5BqNAE.lTjV.5Ec"
	return fmt.Sprintf(`{"U":"%s","L":"%s","Z":"%s","V":"1.1","F":"%s"}`, ua, locale, tz, fingerprint)
}

func format(v verdict) string {
	verdictText := "no answer"
	switch {
	case v.Valid == nil:
		verdictText = "no answer"
	case *v.Valid && v.Used != nil && *v.Used:
		verdictText = "TAKEN"
	case *v.Valid:
		verdictText = "FREE"
	default:
		verdictText = "REJECTED"
	}
	flags := "[]"
	if v.AppleOwnedDomain != nil || v.RecycledDomain != nil {
		var parts []string
		if v.AppleOwnedDomain != nil && *v.AppleOwnedDomain {
			parts = append(parts, "apple-owned")
		}
		if v.RecycledDomain != nil && *v.RecycledDomain {
			parts = append(parts, "recycled-domain")
		}
		flags = "[" + strings.Join(parts, ",") + "]"
	}
	line := fmt.Sprintf("%-40s %-10s %s", v.Address, verdictText, flags)
	if v.Error != "" {
		line += " err=" + v.Error
	}
	return line
}

func mask(value string) string {
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func trim(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
