package gsa

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http/cookiejar"
	"strings"
	"time"

	nethttp "net/http"

	"github.com/Laky-64/http"

	"github.com/Laky-64/appleservices/internal/httpx"
	"github.com/Laky-64/appleservices/internal/srp"
	"github.com/Laky-64/appleservices/internal/uuid"
	"howett.net/plist"
)

const anisetteMaxAge = 60 * time.Second

var ErrAccountRepairRequired = errors.New("gsa_account_repair_required")

func continuationRequires2FA(au string) (bool, error) {
	switch au {
	case "":
		return false, nil
	case "secondaryAuth", "trustedDeviceSecondaryAuth":
		return true, nil
	case "repair":
		return false, ErrAccountRepairRequired
	default:
		return false, errors.New("gsa: unsupported authentication continuation")
	}
}

type Client struct {
	anisette              AnisetteProvider
	transport             nethttp.RoundTripper
	cookieJar             *cookiejar.Jar
	cachedAnisetteHeaders map[string]string
	anisetteFetchedAt     time.Time
	smsPhoneID            int
	smsCodeAlreadySent    bool
	loginPayload          map[string]any
	verifiedTokens        map[string]string
	responseHeaders       map[string][]string
	pushToken             []byte
	nativeDevice          bool
	requestID             string
}

func (c *Client) EnableNativeDeviceAuthentication() { c.nativeDevice = true }

func (c *Client) SetPushToken(token []byte) error {
	if len(token) != 0 && len(token) != 32 {
		return errors.New("gsa: APS token must be 32 bytes")
	}
	c.pushToken = append([]byte(nil), token...)
	return nil
}

func (c *Client) anisetteHeaders() (map[string]string, error) {
	if c.cachedAnisetteHeaders != nil && time.Since(c.anisetteFetchedAt) < anisetteMaxAge {
		return c.cachedAnisetteHeaders, nil
	}
	return c.refreshAnisetteHeaders()
}

func (c *Client) refreshAnisetteHeaders() (map[string]string, error) {
	h, err := c.anisette.Headers()
	if err != nil {
		return nil, fmt.Errorf("gsa: anisette headers: %w", err)
	}
	c.cachedAnisetteHeaders = httpx.NormalizeAnisetteHeaders(h)
	c.anisetteFetchedAt = time.Now()
	return c.cachedAnisetteHeaders, nil
}

func (c *Client) AnisetteHeaders() (map[string]string, error) {
	return c.anisetteHeaders()
}

const gsaBaseURL = "https://gsa.apple.com"

func NewClient(anisette AnisetteProvider) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		anisette:  anisette,
		transport: newAuthenticationTransport(),
		cookieJar: jar,
	}
}

type LoginResult struct {
	NeedsTwoFactor  bool
	TwoFactorMethod string
	SessionPayload  map[string]any
}

func caseInsensitiveLookup(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// gsaRequestAttempts bounds how often one GrandSlam call is resent. The edge in
// front of gsa.apple.com drops a request it cannot route and answers with an
// empty 200, a bare 204/403/404, or a 5xx — none of which carries an
// application answer. Only a plist with a Response dict proves the request
// arrived, so anything else is resent rather than reported as a rejection.
const gsaRequestAttempts = 40

// gsaRetryDelay paces those resends. The edge's behaviour changes on the scale
// of minutes, so the attempts stay close together while the login is still
// worth completing.
const gsaRetryDelay = 400 * time.Millisecond

// retryableGSAStatus reports whether a status means the request never reached
// the application. The package's http helper shadows net/http, so the codes are
// written out.
func retryableGSAStatus(status int) bool {
	switch status {
	case 204, 403, 404, 429:
		return true
	}
	return status/100 == 5
}

func (c *Client) doRequest(request map[string]any) (map[string]any, error) {
	body := map[string]any{
		"Header":  map[string]any{"Version": "1.0.1"},
		"Request": request,
	}
	var buf bytes.Buffer
	if err := plist.NewEncoder(&buf).Encode(body); err != nil {
		return nil, fmt.Errorf("gsa: encode request: %w", err)
	}

	anisetteHeaders, err := c.anisetteHeaders()
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Content-Type": "text/x-xml-plist",
		"Accept":       "*/*",
		"User-Agent":   "akd/1.0 CFNetwork/978.0.7 Darwin/18.7.0",
	}
	if ua := caseInsensitiveLookup(anisetteHeaders, "User-Agent"); ua != "" {
		headers["User-Agent"] = ua
	}
	if ci := caseInsensitiveLookup(anisetteHeaders, "X-Mme-Client-Info"); ci != "" {
		headers["X-Mme-Client-Info"] = ci
	}
	if c.nativeDevice {
		headers["X-Apple-AK-Context-Type"] = "icloud"
		headers["X-Apple-Client-App-Name"] = "AppleIDSettings"
		headers["X-Apple-I-Client-Bundle-Id"] = "com.apple.systempreferences.AppleIDSettings"
		headers["Accept-Language"] = "en-US,en;q=0.9"
		if strings.Contains(headers["X-Mme-Client-Info"], "<macOS;13.1;") {
			headers["User-Agent"] = "akd/1.0 CFNetwork/1399 Darwin/22.1.0"
		}
	}

	var lastErr error
	for attempt := 1; attempt <= gsaRequestAttempts; attempt++ {
		resp, err := c.gsaRoundTrip(request, buf.Bytes(), headers)
		if err == nil {
			return resp, nil
		}
		var dropped *droppedRequestError
		if !errors.As(err, &dropped) || attempt == gsaRequestAttempts {
			return nil, err
		}
		lastErr = err
		time.Sleep(gsaRetryDelay)
	}
	return nil, lastErr
}

// droppedRequestError marks a response that proves the request never reached
// the application, so the caller may resend it unchanged.
type droppedRequestError struct {
	status int
	reason string
}

func (e *droppedRequestError) Error() string {
	return "gsa: request did not reach the application (" + e.reason + ")"
}

// gsaRoundTrip performs one GrandSlam call. It reports a droppedRequestError
// when the edge answered without forwarding the request.
func (c *Client) gsaRoundTrip(request map[string]any, body []byte, headers map[string]string) (map[string]any, error) {
	result, err := http.ExecuteRequest(gsaBaseURL+"/grandslam/GsService2",
		http.Method("POST"),
		http.Body(body),
		http.Headers(headers),
		http.Transport(c.transport),
		http.CookieJar(c.cookieJar),
		http.Timeout(30*time.Second),
	)
	op, _ := request["o"].(string)
	if result != nil && result.StatusCode != 200 {
		if retryableGSAStatus(result.StatusCode) {
			return nil, &droppedRequestError{status: result.StatusCode, reason: fmt.Sprintf("HTTP %d", result.StatusCode)}
		}
		return nil, &AuthenticationError{Operation: op, HTTPStatus: result.StatusCode}
	}
	if err != nil {
		return nil, fmt.Errorf("gsa: request failed: %w", err)
	}
	if result == nil {
		return nil, &droppedRequestError{status: 0, reason: "no response"}
	}
	if len(bytes.TrimSpace(result.Body)) == 0 {
		return nil, &droppedRequestError{status: 200, reason: "empty 200"}
	}
	c.responseHeaders = cloneHeaderValues(result.Headers)

	var decoded map[string]any
	if _, err := plist.Unmarshal(result.Body, &decoded); err != nil {
		// A 200 that does not decode is an edge error page, not an answer.
		return nil, &droppedRequestError{status: 200, reason: "undecodable 200"}
	}
	respMap, ok := decoded["Response"].(map[string]any)
	if !ok {
		return nil, errors.New("gsa: response missing Response dict")
	}
	return respMap, nil
}

func cloneHeaderValues(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	default:
		return 0, false
	}
}

func (c *Client) buildCPD() (map[string]any, error) {
	h, err := c.anisetteHeaders()
	if err != nil {
		return nil, err
	}
	cpd := map[string]any{
		"bootstrap": true,
		"icscrec":   true,
		"pbe":       false,
		"prkgen":    true,
		"svct":      "iCloud",
	}
	if len(c.pushToken) == 32 {
		cpd["ptkn"] = strings.ToUpper(fmt.Sprintf("%x", c.pushToken))
	}
	if c.nativeDevice {
		cpd["svct"] = "icloud"
		cpd["capp"] = "AppleIDSettings"
		cpd["cbid"] = "com.apple.systempreferences.AppleIDSettings"
		cpd["loc"], cpd["cou"] = "en_US", "US"
		cpd["X-Apple-I-Request-UUID"] = c.requestID
		cpd["X-Apple-I-Device-Configuration-Mode"] = "0"
		cpd["X-Apple-Requested-Partition"] = "0"
		cpd["X-Apple-Security-Upgrade-Context"] = "com.apple.authkit.generic"
		cpd["at"], cpd["ckgen"], cpd["fcd"] = 0, true, true
	}
	for k, v := range h {
		if !strings.EqualFold(k, "User-Agent") {
			cpd[k] = v
		}
	}
	return cpd, nil
}

func derivePasswordKey(password string, protocol string, salt []byte, iterations int) ([]byte, error) {
	if protocol != "s2k" && protocol != "s2k_fo" {
		return nil, fmt.Errorf("gsa: unsupported SRP protocol %q", protocol)
	}
	p1 := sha256.Sum256([]byte(password))
	var p2 []byte
	if protocol == "s2k_fo" {
		p2 = []byte(fmt.Sprintf("%x", p1))
	} else {
		p2 = p1[:]
	}
	return pbkdf2.Key(sha256.New, string(p2), salt, iterations, 32)
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("gsa: spd ciphertext length invalid for PKCS7")
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > blockSize || padLen > len(data) {
		return nil, errors.New("gsa: spd invalid PKCS7 padding length")
	}
	for _, b := range data[len(data)-padLen:] {
		if int(b) != padLen {
			return nil, errors.New("gsa: spd invalid PKCS7 padding bytes")
		}
	}
	return data[:len(data)-padLen], nil
}

func decryptSPD(sessionKey, spd []byte) (map[string]any, error) {
	key := hmacSHA256(sessionKey, []byte("extra data key:"))
	iv := hmacSHA256(sessionKey, []byte("extra data iv:"))[:aes.BlockSize]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("gsa: spd aes cipher: %w", err)
	}
	if len(spd)%aes.BlockSize != 0 {
		return nil, errors.New("gsa: spd ciphertext not a multiple of the AES block size")
	}
	plain := make([]byte, len(spd))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, spd)

	unpadded, err := pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return nil, err
	}

	var decoded map[string]any
	if _, err := plist.Unmarshal(unpadded, &decoded); err != nil {
		return nil, fmt.Errorf("gsa: spd plist decode: %w", err)
	}
	return decoded, nil
}

func (c *Client) Login(username, password string) (*LoginResult, error) {
	c.requestID = strings.ToUpper(uuid.New())
	c.smsPhoneID = 0
	c.smsCodeAlreadySent = false
	c.verifiedTokens = nil
	c.loginPayload = nil
	params := srp.NewParams()
	client := srp.NewClient(params, username)

	if _, err := c.refreshAnisetteHeaders(); err != nil {
		return nil, err
	}

	initCPD, err := c.buildCPD()
	if err != nil {
		return nil, err
	}
	initResp, err := c.doRequest(map[string]any{
		"cpd": initCPD,
		"A2k": client.PublicA().Bytes(),
		"ps":  []string{"s2k", "s2k_fo"},
		"u":   username,
		"o":   "init",
	})
	if err != nil {
		return nil, fmt.Errorf("gsa: init request: %w", err)
	}
	if _, err := checkStatus(initResp, "init", ""); err != nil {
		return nil, err
	}

	protocol, _ := initResp["sp"].(string)
	salt, _ := initResp["s"].([]byte)
	iterations, ok := asInt64(initResp["i"])
	if !ok {
		return nil, errors.New("gsa: could not parse ec/i as integer")
	}
	challengeID, _ := initResp["c"].(string)
	bBytes, _ := initResp["B"].([]byte)
	if salt == nil || bBytes == nil || challengeID == "" {
		return nil, errors.New("gsa: init response missing required fields")
	}
	B := new(big.Int).SetBytes(bBytes)

	passwordKey, err := derivePasswordKey(password, protocol, salt, int(iterations))
	if err != nil {
		return nil, fmt.Errorf("gsa: derive password key: %w", err)
	}
	x := params.DeriveX(salt, passwordKey)

	m1, sessionKey, err := client.ProcessChallenge(x, salt, B)
	if err != nil {
		return nil, fmt.Errorf("gsa: process challenge: %w", err)
	}

	completeCPD, err := c.buildCPD()
	if err != nil {
		return nil, err
	}
	completeResp, err := c.doRequest(map[string]any{
		"cpd": completeCPD,
		"c":   challengeID,
		"M1":  m1,
		"u":   username,
		"o":   "complete",
	})
	if err != nil {
		return nil, fmt.Errorf("gsa: complete request: %w", err)
	}
	status, err := checkStatus(completeResp, "complete", protocol)
	if err != nil {
		return nil, err
	}

	m2, _ := completeResp["M2"].([]byte)
	if m2 == nil {
		return nil, errors.New("gsa: complete response missing M2")
	}
	if err := client.VerifyServer(m2); err != nil {
		return nil, fmt.Errorf("gsa: server proof invalid: %w", err)
	}

	var sessionPayload map[string]any
	if spd, _ := completeResp["spd"].([]byte); len(spd) > 0 {
		sessionPayload, err = decryptSPD(sessionKey, spd)
		if err != nil {
			return nil, fmt.Errorf("gsa: decrypt spd: %w", err)
		}
	}
	// Trusted devices can complete without a new 2FA prompt. Apple may still
	// return the service-scoped commerce tokens in the GrandSlam response
	// headers, so retain them instead of reducing the session to the PET only.
	if sessionPayload != nil && len(c.responseHeaders) > 0 {
		c.collectVerifiedTokens(c.responseHeaders)
		sessionPayload = c.mergeVerifiedTokens(sessionPayload)
	}

	au, _ := status["au"].(string)
	needsTwoFactor, err := continuationRequires2FA(au)
	if err != nil {
		return nil, err
	}
	c.loginPayload = sessionPayload

	return &LoginResult{NeedsTwoFactor: needsTwoFactor, TwoFactorMethod: au, SessionPayload: sessionPayload}, nil
}
