package gsa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Laky-64/http"
	"howett.net/plist"
)

// SecondFactorError exposes only actionable status fields, never Apple's raw
// message, phone number, tokens, or security code.
type SecondFactorError struct {
	Operation             string
	HTTPStatus            int
	Code                  int64
	TooManyCodesSent      bool
	TooManyCodesValidated bool
	SecurityCodeLocked    bool
	NoTrustedDevices      bool
}

func (e *SecondFactorError) Error() string {
	message := fmt.Sprintf("gsa: %s: status %d", e.Operation, e.HTTPStatus)
	if e.Code != 0 {
		message += fmt.Sprintf("; ec=%d", e.Code)
	}
	if e.TooManyCodesSent {
		message += "; tooManyCodesSent=true"
	}
	if e.TooManyCodesValidated {
		message += "; tooManyCodesValidated=true"
	}
	if e.SecurityCodeLocked {
		message += "; securityCodeLocked=true"
	}
	if e.NoTrustedDevices {
		message += "; noTrustedDevices=true"
	}
	return message
}

func secondFactorResponseError(operation string, status int, body []byte) error {
	var details struct {
		NoTrustedDevices bool `json:"noTrustedDevices"`
		SecurityCode     struct {
			TooManyCodesSent      bool `json:"tooManyCodesSent"`
			TooManyCodesValidated bool `json:"tooManyCodesValidated"`
			SecurityCodeLocked    bool `json:"securityCodeLocked"`
		} `json:"securityCode"`
		ServiceErrors []struct {
			Code json.RawMessage `json:"code"`
		} `json:"serviceErrors"`
	}
	if match := bootArgsScriptRe.FindSubmatch(body); match != nil {
		body = match[1]
	}
	if json.Unmarshal(body, &details) != nil {
		return &SecondFactorError{Operation: operation, HTTPStatus: status}
	}
	result := &SecondFactorError{Operation: operation, HTTPStatus: status,
		TooManyCodesSent:      details.SecurityCode.TooManyCodesSent,
		TooManyCodesValidated: details.SecurityCode.TooManyCodesValidated,
		SecurityCodeLocked:    details.SecurityCode.SecurityCodeLocked,
		NoTrustedDevices:      details.NoTrustedDevices}
	if len(details.ServiceErrors) > 0 {
		result.Code, _ = strconv.ParseInt(strings.Trim(string(details.ServiceErrors[0].Code), "\""), 10, 64)
	}
	return result
}

func traceTwoFactorResponse(stage string, status int, headers map[string][]string, body []byte) error {
	directory := os.Getenv("APPLESERVICES_2FA_DIAGNOSTIC_DIR")
	if directory == "" {
		return nil
	}
	encoded, err := json.Marshal(map[string]any{"stage": stage, "status": status, "headers": headers, "body": body})
	if err != nil {
		return errors.New("gsa: cannot encode private second-factor response")
	}
	path := filepath.Join(directory, fmt.Sprintf("%s-%d.json", stage, time.Now().UnixNano()))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("gsa: cannot create private second-factor response")
	}
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("gsa: cannot save private second-factor response")
	}
	fmt.Printf("2FA diagnostic: stage=%s; HTTP=%d; response-bytes=%d\n", stage, status, len(body))
	return nil
}

// Keep the connection comparison isolated to second-factor requests; SRP still
// uses the authentication transport and this client's cookies and identity.
func (c *Client) twoFactorTransport() nethttp.RoundTripper {
	if os.Getenv("APPLESERVICES_2FA_CONNECTION_PROFILE") == "pooled" {
		if transport, ok := c.transport.(*authenticationTransport); ok {
			return transport.shared
		}
	}
	return c.transport
}

func (c *Client) twoFactorHeaders(dsid, idmsToken string) (map[string]string, error) {
	anisetteHeaders, err := c.anisetteHeaders()
	if err != nil {
		return nil, err
	}
	identityToken := base64.StdEncoding.EncodeToString([]byte(dsid + ":" + idmsToken))
	// Keep the Xcode compatibility normalization, without treating HTTP 423
	// as proof of a particular bad header or of an account cooldown.
	headers := map[string]string{
		"Accept-Language":        "en-us",
		"X-Apple-Identity-Token": identityToken,
	}
	for k, v := range anisetteHeaders {
		headers[k] = v
	}
	// The Rust reference's get_extra_headers uses a browser UA for 2FA,
	// distinct from its akd SRP UA. Keep this comparison explicit.
	profile := os.Getenv("APPLESERVICES_2FA_PROFILE")
	if profile == "browser" || profile == "authkit" {
		headers["User-Agent"] = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko)"
		headers["Sec-Fetch-Site"], headers["Sec-Fetch-Mode"], headers["Sec-Fetch-Dest"] = "same-origin", "cors", "empty"
	}
	if profile == "authkit" {
		if !c.nativeDevice {
			return nil, errors.New("gsa: AuthKit comparison requires explicit native Mac authentication")
		}
		// Match the reference AppleIDSettings second-factor context while
		// retaining this provider's model, OS, device ID and anisette pair.
		info := headers["X-Mme-Client-Info"]
		if index := strings.LastIndex(info, "<"); index > 0 {
			headers["X-Mme-Client-Info"] = info[:index] + "<com.apple.AuthKit/1 (com.apple.systempreferences.AppleIDSettings/1)>"
		}
		for key, value := range map[string]string{
			"X-Apple-AK-Context-Type": "icloud", "X-Apple-Client-App-Name": "AppleIDSettings",
			"X-Apple-I-Client-Bundle-Id":  "com.apple.systempreferences.AppleIDSettings",
			"X-Apple-Requested-Partition": "0", "X-Apple-I-Device-Configuration-Mode": "0",
			"X-Apple-I-DeviceUserMode": "0", "X-Apple-Security-Upgrade-Context": "com.apple.authkit.generic",
			"X-Apple-I-CDP-Status": "false", "X-Apple-I-CDP-Circle-Status": "false",
			"X-Apple-I-OT-Status": "false", "X-Apple-I-ICSCREC": "true", "X-Apple-I-PRK-Gen": "true",
			"X-Apple-I-Locale": "en_US", "X-Apple-I-TimeZone": "UTC", "X-Apple-I-TimeZone-Offset": "0",
			"X-MMe-Country": "US", "Accept-Language": "en-US,en;q=0.9",
			"X-Apple-I-CFU-State": base64.StdEncoding.EncodeToString([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<array/>\n</plist>\n")),
		} {
			headers[key] = value
		}
	}
	return headers, nil
}

func (c *Client) RequestTrustedDeviceCode(dsid, idmsToken string) error {
	headers, err := c.twoFactorHeaders(dsid, idmsToken)
	if err != nil {
		return err
	}
	headers["Content-Type"] = "text/x-xml-plist"
	headers["Accept"] = "text/x-xml-plist"

	result, err := http.ExecuteRequest(gsaBaseURL+"/auth/verify/trusteddevice",
		http.Method("GET"),
		http.Headers(headers),
		http.Transport(c.twoFactorTransport()),
		http.CookieJar(c.cookieJar),
		http.Timeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("gsa: request trusted-device code: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return fmt.Errorf("gsa: request trusted-device code: status %d", result.StatusCode)
	}
	return nil
}

func (c *Client) SubmitTrustedDeviceCode(dsid, idmsToken, code string) error {
	headers, err := c.twoFactorHeaders(dsid, idmsToken)
	if err != nil {
		return err
	}
	headers["security-code"] = code

	result, err := http.ExecuteRequest(gsaBaseURL+"/grandslam/GsService2/validate",
		http.Method("GET"),
		http.Headers(headers),
		http.Transport(c.twoFactorTransport()),
		http.CookieJar(c.cookieJar),
		http.Timeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("gsa: submit trusted-device code: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return secondFactorResponseError("submit trusted-device code", result.StatusCode, result.Body)
	}
	if len(bytes.TrimSpace(result.Body)) > 0 {
		var response map[string]any
		if _, err := plist.Unmarshal(result.Body, &response); err != nil {
			return fmt.Errorf("gsa: decode trusted-device validation response: %w", err)
		}
		if inner, ok := response["Response"].(map[string]any); ok {
			response = inner
		}
		if _, err := checkStatus(response, "validate", ""); err != nil {
			return err
		}
	}
	c.collectVerifiedTokens(result.Headers)
	return nil
}

type phoneNumberID struct {
	ID int `json:"id"`
}

type securityCodeBody struct {
	Code string `json:"code"`
}

type smsRequestBody struct {
	PhoneNumber phoneNumberID `json:"phoneNumber"`
	Mode        string        `json:"mode"`
}

type smsSubmitBody struct {
	PhoneNumber  phoneNumberID    `json:"phoneNumber"`
	Mode         string           `json:"mode"`
	SecurityCode securityCodeBody `json:"securityCode"`
}

var bootArgsScriptRe = regexp.MustCompile(`(?is)<script\b[^>]*\bclass\s*=\s*["'][^"']*\bboot_args\b[^"']*["'][^>]*>(.*?)</script\s*>`)

type bootArgs struct {
	TrustedPhoneNumbers []phoneNumberID `json:"trustedPhoneNumbers"`
	Direct              struct {
		PhoneNumberVerification struct {
			TrustedPhoneNumbers []phoneNumberID `json:"trustedPhoneNumbers"`
			TrustedPhoneNumber  struct {
				ID int `json:"id"`
			} `json:"trustedPhoneNumber"`
		} `json:"phoneNumberVerification"`
	} `json:"direct"`
}

func (c *Client) fetchTrustedPhoneID(dsid, idmsToken string) (int, error) {
	headers, err := c.twoFactorHeaders(dsid, idmsToken)
	if err != nil {
		return 0, err
	}
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "application/json"
	headers["Connection"] = "keep-alive"

	result, err := http.ExecuteRequest(gsaBaseURL+"/auth",
		http.Method("GET"),
		http.Headers(headers),
		http.Transport(c.twoFactorTransport()),
		http.CookieJar(c.cookieJar),
		http.Timeout(10*time.Second),
	)
	if err != nil && (result == nil || result.StatusCode == 200 || result.StatusCode == 201) {
		return 0, fmt.Errorf("gsa: fetch boot_args page: %w", err)
	}
	if result == nil {
		return 0, errors.New("gsa: fetch boot_args page: no response")
	}
	if err := traceTwoFactorResponse("phone-metadata", result.StatusCode, result.Headers, result.Body); err != nil {
		return 0, err
	}
	if result.StatusCode != 200 && result.StatusCode != 201 {
		return 0, secondFactorResponseError("fetch boot_args page", result.StatusCode, result.Body)
	}
	c.smsCodeAlreadySent = result.StatusCode == 201

	return parseTrustedPhoneID(result.Body)
}

func parseTrustedPhoneID(body []byte) (int, error) {
	var args bootArgs
	if err := json.Unmarshal(body, &args); err != nil {
		match := bootArgsScriptRe.FindSubmatch(body)
		if match == nil {
			return 0, errors.New("gsa: auth response is neither phone JSON nor an HTML boot_args page")
		}
		if err := json.Unmarshal(match[1], &args); err != nil {
			return 0, fmt.Errorf("gsa: decode boot_args: %w", err)
		}
	}
	id := args.Direct.PhoneNumberVerification.TrustedPhoneNumber.ID
	if id > 0 {
		return id, nil
	}
	for _, phone := range args.TrustedPhoneNumbers {
		if phone.ID > 0 {
			return phone.ID, nil
		}
	}
	for _, phone := range args.Direct.PhoneNumberVerification.TrustedPhoneNumbers {
		if phone.ID > 0 {
			return phone.ID, nil
		}
	}
	return 0, errors.New("gsa: auth response missing trusted phone number id")
}

func (c *Client) RequestSMSCode(dsid, idmsToken string) error {
	phoneID, err := c.fetchTrustedPhoneID(dsid, idmsToken)
	if err != nil {
		return err
	}
	if c.smsCodeAlreadySent {
		c.smsPhoneID = phoneID
		return nil
	}
	return c.requestSMSCodeForPhone(dsid, idmsToken, phoneID)
}

// RequestSMSCodeForPhone is an explicit diagnostic for a selected phone ID.
// It does not guess or enumerate phone IDs when metadata fetching fails.
func (c *Client) RequestSMSCodeForPhone(dsid, idmsToken string, phoneID int) error {
	if phoneID <= 0 {
		return errors.New("gsa: positive trusted phone id required")
	}
	return c.requestSMSCodeForPhone(dsid, idmsToken, phoneID)
}

func (c *Client) requestSMSCodeForPhone(dsid, idmsToken string, phoneID int) error {
	headers, err := c.twoFactorHeaders(dsid, idmsToken)
	if err != nil {
		return err
	}
	headers["Content-Type"] = "application/json"

	body, err := json.Marshal(smsRequestBody{PhoneNumber: phoneNumberID{ID: phoneID}, Mode: "sms"})
	if err != nil {
		return fmt.Errorf("gsa: encode sms request: %w", err)
	}
	headers["Accept"] = "application/json"

	result, err := http.ExecuteRequest(gsaBaseURL+"/auth/verify/phone",
		http.Method("PUT"),
		http.Headers(headers),
		http.Body(body),
		http.Transport(c.twoFactorTransport()),
		http.CookieJar(c.cookieJar),
		http.Timeout(10*time.Second),
	)
	if result != nil {
		if traceErr := traceTwoFactorResponse("sms-request", result.StatusCode, result.Headers, result.Body); traceErr != nil {
			return traceErr
		}
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			return secondFactorResponseError("request sms code", result.StatusCode, result.Body)
		}
	}
	if err != nil {
		return fmt.Errorf("gsa: request sms code: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return fmt.Errorf("gsa: request sms code: status %d", result.StatusCode)
	}
	c.smsPhoneID = phoneID
	return nil
}

func (c *Client) SubmitSMSCode(dsid, idmsToken, code string) error {
	phoneID := c.smsPhoneID
	if phoneID == 0 {
		var err error
		phoneID, err = c.fetchTrustedPhoneID(dsid, idmsToken)
		if err != nil {
			return err
		}
	}

	headers, err := c.twoFactorHeaders(dsid, idmsToken)
	if err != nil {
		return err
	}
	headers["Content-Type"] = "application/json"

	body, err := json.Marshal(smsSubmitBody{
		PhoneNumber:  phoneNumberID{ID: phoneID},
		Mode:         "sms",
		SecurityCode: securityCodeBody{Code: code},
	})
	if err != nil {
		return fmt.Errorf("gsa: encode sms submit: %w", err)
	}
	headers["Accept"] = "application/json"

	result, err := http.ExecuteRequest(gsaBaseURL+"/auth/verify/phone/securitycode",
		http.Method("POST"),
		http.Headers(headers),
		http.Body(body),
		http.Transport(c.twoFactorTransport()),
		http.CookieJar(c.cookieJar),
		http.Timeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("gsa: submit sms code: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return secondFactorResponseError("submit sms code", result.StatusCode, result.Body)
	}
	c.collectVerifiedTokens(result.Headers)
	return nil
}
