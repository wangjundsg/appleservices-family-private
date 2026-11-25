package gsa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"
)

var deviceServiceURL = "https://gsas.apple.com/grandslam/GsService2/postdata"
var deviceListURL = "https://gsa.apple.com/appleid/account/manage/security/devices/summary"
var deviceTeardownURL = "https://gsas.apple.com/grandslam/GsService2/teardown"

type DeviceRequestError struct {
	HTTPStatus    int
	Code          int64
	ServerMessage string
}

func (e *DeviceRequestError) Error() string {
	return fmt.Sprintf("gsa: account-device request status %d (ec=%d)", e.HTTPStatus, e.Code)
}

// AccountDevice describes the account's registered devices. An entry alone is
// not proof that Apple accepts it as a verification-code receiver.
type AccountDevice struct {
	ID                        string `json:"id"`
	Name                      string `json:"name"`
	ModelName                 string `json:"modelName"`
	CurrentDevice             bool   `json:"currentDevice"`
	SerialNumber              string `json:"serialNumber"`
	IsTrustedDevice           *bool  `json:"isTrustedDevice"`
	SupportsVerificationCodes *bool  `json:"supportsVerificationCodes"`
	DeviceDetailURI           string `json:"deviceDetailUri"`
	DeviceDetailHTTPMethod    string `json:"deviceDetailHttpMethod"`
}

func payloadToken(payload map[string]any, service string) string {
	tokens, _ := payload["t"].(map[string]any)
	entry, _ := tokens[service].(map[string]any)
	token, _ := entry["token"].(string)
	return token
}

// DeviceRegistration is the account-side liveness announcement. PushToken must
// come from a successful APS connection; an arbitrary token cannot receive codes.
type DeviceRegistration struct {
	Name      string
	PushToken []byte
}

func (c *Client) accountRequest(method, endpoint string, headers map[string]string, body []byte) ([]byte, error) {
	return c.accountRequestContext(context.Background(), method, endpoint, headers, body)
}

func (c *Client) accountRequestContext(ctx context.Context, method, endpoint string, headers map[string]string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Transport: c.transport, Jar: c.cookieJar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, deviceTransportFailure(err)
	}
	defer resp.Body.Close()
	data, readErr := readAccountResponse(resp)
	if directory := os.Getenv("APPLESERVICES_ACCOUNT_DEVICE_DIAGNOSTIC_DIR"); directory != "" && method == http.MethodGet && endpoint == deviceListURL {
		path := filepath.Join(directory, fmt.Sprintf("device-summary-%d.json", time.Now().UnixNano()))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, errors.New("gsa: cannot create private account-device diagnostic")
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, errors.New("gsa: cannot save private account-device diagnostic")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &DeviceRequestError{HTTPStatus: resp.StatusCode}
		var decoded map[string]any
		if _, err := plist.Unmarshal(data, &decoded); err == nil {
			if inner, ok := decoded["Response"].(map[string]any); ok {
				decoded = inner
			}
			if inner, ok := decoded["Status"].(map[string]any); ok {
				decoded = inner
			}
			if ec, ok := asInt64(decoded["ec"]); ok {
				failure.Code = ec
			}
			failure.ServerMessage, _ = decoded["em"].(string)
		}
		return nil, failure
	}
	if readErr != nil {
		return nil, deviceTransportFailure(readErr)
	}
	return data, nil
}

func (c *Client) authenticatedDeviceHeaders(payload map[string]any) (map[string]string, error) {
	adsid, _ := payload["adsid"].(string)
	if adsid == "" {
		return nil, errors.New("gsa: authenticated account ID missing")
	}
	headers, err := c.anisetteHeaders()
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(headers)+5)
	for key, value := range headers {
		result[key] = value
	}
	// Match the macOS 13.1 identity supplied by the current anisette provider.
	// A native akd request must not claim the server's legacy CFNetwork/808 agent.
	if info := caseInsensitiveLookup(result, "X-Mme-Client-Info"); strings.Contains(info, "<macOS;13.1;") {
		result["User-Agent"] = "akd/1.0 CFNetwork/1399 Darwin/22.1.0"
	}
	result["Content-Type"] = "text/x-xml-plist"
	result["Accept"] = "*/*"
	result["Accept-Language"] = "en-US,en;q=0.9"
	return result, nil
}

func (c *Client) RegisterDevice(payload map[string]any, registration DeviceRegistration) error {
	if strings.TrimSpace(registration.Name) == "" {
		return errors.New("gsa: device name is required")
	}
	if len(registration.PushToken) != 32 {
		return errors.New("gsa: APS device token must be 32 bytes")
	}
	heartbeat := payloadToken(payload, "com.apple.gs.idms.hb")
	if heartbeat == "" {
		return errors.New("gsa: registration requires an authenticated heartbeat token")
	}
	headers, err := c.authenticatedDeviceHeaders(payload)
	if err != nil {
		return err
	}
	adsid := payload["adsid"].(string)
	headers["X-Apple-HB-Token"] = base64.StdEncoding.EncodeToString([]byte(adsid + ":" + heartbeat))
	headers["X-Apple-I-UrlSwitch-Info"] = base64.StdEncoding.EncodeToString([]byte(adsid + ":postdata"))
	headers["X-Apple-I-Service-Type"] = "itunesstore"
	headers["X-Apple-I-Device-Configuration-Mode"] = "0"
	headers["X-Apple-I-DeviceUserMode"] = "0"
	headers["X-Apple-Requested-Partition"] = "0"
	headers["X-Apple-I-TimeZone-Offset"] = "0"
	headers["X-Apple-I-Device-Type"] = "1"
	// These are this client's capabilities, not the account's cloud settings.
	headers["X-Apple-I-CDP-Status"] = "false"
	headers["X-Apple-I-OT-Status"] = "false"
	request := map[string]any{
		"dn": registration.Name, "event": "liveness", "loc": "en_US",
		"services": []string{"icloud"}, "cfuids": []string{},
		"cdpStatus": false, "circleStatus": false, "otStatus": false,
		"rep": 1, "ut": 1, "signinPartition": 1,
	}
	request["ptkn"] = strings.ToUpper(hex.EncodeToString(registration.PushToken))
	body, err := plist.Marshal(map[string]any{"Header": map[string]any{"Version": "1.0.1"}, "Request": request}, plist.XMLFormat)
	if err != nil {
		return err
	}
	data, err := c.accountRequest(http.MethodPost, deviceServiceURL, headers, body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var response map[string]any
	if _, err := plist.Unmarshal(data, &response); err != nil {
		return errors.New("gsa: registration returned an unrecognized response")
	}
	if inner, ok := response["Response"].(map[string]any); ok {
		response = inner
	}
	if message, ok := response["localizedError"].(string); ok && message != "" {
		return errors.New("gsa: registration returned a localized error")
	}
	if status, ok := response["Status"].(map[string]any); ok {
		if ec, valid := asInt64(status["ec"]); valid && ec != 0 {
			return fmt.Errorf("gsa: registration rejected with ec=%d", ec)
		}
	}
	for _, key := range []string{"ec", "status"} {
		if ec, valid := asInt64(response[key]); valid && ec != 0 {
			return fmt.Errorf("gsa: registration rejected with %s=%d", key, ec)
		}
	}
	return nil
}

func (c *Client) AccountDevices(payload map[string]any) ([]AccountDevice, error) {
	return c.AccountDevicesContext(context.Background(), payload)
}

func (c *Client) AccountDevicesContext(ctx context.Context, payload map[string]any) ([]AccountDevice, error) {
	if err := ctx.Err(); err != nil {
		return nil, deviceTransportFailure(err)
	}
	headers, err := c.authenticatedDeviceHeaders(payload)
	if err != nil {
		return nil, err
	}
	token, _ := payload["GsIdmsToken"].(string)
	if token == "" {
		token = payloadToken(payload, "com.apple.gs.appleid.auth")
	}
	if token == "" {
		return nil, errors.New("gsa: device list requires an account identity token")
	}
	headers["X-Apple-Identity-Token"] = base64.StdEncoding.EncodeToString([]byte(payload["adsid"].(string) + ":" + token))
	headers["Accept"] = "application/json"
	data, err := c.accountRequestContext(ctx, http.MethodGet, deviceListURL, headers, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		ManageDevices *struct {
			Devices []AccountDevice `json:"devices"`
		} `json:"manageDevices"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, errors.New("gsa: device list returned invalid JSON")
	}
	if response.ManageDevices == nil {
		return nil, errors.New("gsa: device list response missing manageDevices")
	}
	return response.ManageDevices.Devices, nil
}

// AccountDeviceDetails follows only Apple's GET detail link for the current
// device returned by AccountDevices. It cannot remove or register a device.
func (c *Client) AccountDeviceDetails(payload map[string]any, device AccountDevice) (map[string]any, error) {
	endpoint, err := url.Parse(device.DeviceDetailURI)
	if err != nil || !device.CurrentDevice || device.ID == "" || device.DeviceDetailHTTPMethod != http.MethodGet || endpoint.Scheme != "https" || endpoint.Host != "gsa.apple.com" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/appleid/account/manage/security/devices/"+device.ID {
		return nil, errors.New("gsa: unsupported current-device detail link")
	}
	headers, err := c.authenticatedDeviceHeaders(payload)
	if err != nil {
		return nil, err
	}
	token, _ := payload["GsIdmsToken"].(string)
	if token == "" {
		token = payloadToken(payload, "com.apple.gs.appleid.auth")
	}
	if token == "" {
		return nil, errors.New("gsa: device detail requires an account identity token")
	}
	headers["X-Apple-Identity-Token"] = base64.StdEncoding.EncodeToString([]byte(payload["adsid"].(string) + ":" + token))
	headers["Accept"] = "application/json"
	data, err := c.accountRequest(http.MethodGet, endpoint.String(), headers, nil)
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if json.Unmarshal(data, &response) != nil {
		return nil, errors.New("gsa: device detail returned invalid JSON")
	}
	return response, nil
}

func (c *Client) RespondToSignIn(payload map[string]any, transaction string, allow bool) error {
	if transaction == "" || len(transaction) > 512 {
		return errors.New("gsa: invalid sign-in transaction")
	}
	heartbeat := payloadToken(payload, "com.apple.gs.idms.hb")
	if heartbeat == "" {
		return errors.New("gsa: sign-in response requires a heartbeat token")
	}
	headers, err := c.authenticatedDeviceHeaders(payload)
	if err != nil {
		return err
	}
	adsid := payload["adsid"].(string)
	headers["X-Apple-HB-Token"] = base64.StdEncoding.EncodeToString([]byte(adsid + ":" + heartbeat))
	headers["X-Apple-I-UrlSwitch-Info"] = base64.StdEncoding.EncodeToString([]byte(adsid + ":teardown"))
	headers["X-Apple-I-Device-Configuration-Mode"] = "0"
	headers["X-Apple-I-DeviceUserMode"] = "0"
	headers["X-Apple-Requested-Partition"] = "0"
	headers["X-Apple-I-TimeZone-Offset"] = "0"
	// AuthCodeActivity in OpenBubbles uses these server button identifiers.
	action := "albtn"
	if allow {
		action = "defbtn"
	}
	body, err := plist.Marshal(map[string]any{"Header": map[string]any{}, "Request": map[string]any{
		"action": action, "cmd": 100, "txnid": transaction, "prkgen": false,
	}}, plist.XMLFormat)
	if err != nil {
		return err
	}
	data, err := c.accountRequest(http.MethodPost, deviceTeardownURL, headers, body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var response map[string]any
	if _, err := plist.Unmarshal(data, &response); err != nil {
		return errors.New("gsa: sign-in response returned invalid plist")
	}
	if inner, ok := response["Response"].(map[string]any); ok {
		response = inner
	}
	if _, present := response["Status"]; present {
		_, err := checkStatus(response, "teardown", "")
		return err
	}
	if ec, ok := asInt64(response["ec"]); ok && ec != 0 {
		return &AuthenticationError{Operation: "teardown", Code: ec}
	}
	return nil
}
