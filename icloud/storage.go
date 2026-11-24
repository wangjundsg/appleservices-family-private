package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"howett.net/plist"
)

// StorageInfo intentionally contains no endpoint URLs or authentication tokens.
type StorageInfo struct {
	TotalBytes            uint64 `json:"total_bytes"`
	UsedBytes             uint64 `json:"used_bytes"`
	AvailableBytes        uint64 `json:"available_bytes"`
	PaidBytes             uint64 `json:"paid_bytes"`
	PlanEndpointAvailable bool   `json:"plan_endpoint_available"`
}

// StorageError exposes only the stage and status, never a credential-bearing URL.
type StorageError struct {
	Stage      string
	HTTPStatus int
	AppleCode  string
	Hint       string
}

var accountSettingsURL = "https://setup.icloud.com/setup/get_account_settings"

func HasStorageInfoEndpoint(sd map[string]any) bool {
	_, ok := quotaConfig(sd)["storageInfoURL"].(string)
	return ok
}

func HasStoragePlanEndpoint(sd map[string]any) bool {
	_, ok := quotaConfig(sd)["storagePlanURL"].(string)
	return ok
}

// HasQuotaUpdateEndpoint reports the native Windows iCloud upgrade route.
// It is supplied by account settings and is distinct from the Store's
// presentOffers endpoint.
func HasQuotaUpdateEndpoint(sd map[string]any) bool {
	_, ok := quotaConfig(sd)["quotaUpdateURL"].(string)
	return ok
}

// ResolveQuotaUpdateEndpoint mirrors AOSKit's quota upgrade URL expansion.
// The Windows client replaces these three server supplied placeholders; the
// literal UDID value is part of that native route and is not the SAP GUID.
func ResolveQuotaUpdateEndpoint(sd map[string]any, appleID, prsID string) (string, error) {
	template, _ := quotaConfig(sd)["quotaUpdateURL"].(string)
	if template == "" || !appleEndpoint(template) {
		return "", &StorageError{Stage: "configuration"}
	}
	escape := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	resolved := strings.NewReplacer("$APPLE_ID$", escape(appleID), "$DS_PRS_ID$", escape(prsID), "$UDID$", "123").Replace(template)
	if strings.Contains(resolved, "$") || !appleEndpoint(resolved) {
		return "", &StorageError{Stage: "endpoint"}
	}
	return resolved, nil
}

// FetchAccountSettings retrieves Apple's authenticated service map. It is a
// read-only fallback when loginDelegates returns tokens without quota data.
func FetchAccountSettings(ctx context.Context, dsid, adsid, token string, headers map[string]string) (map[string]any, error) {
	if dsid == "" || adsid == "" || token == "" {
		return nil, &StorageError{Stage: "authentication"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, accountSettingsURL, nil)
	if err != nil {
		return nil, &StorageError{Stage: "request"}
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(dsid+":"+token)))
	request.Header.Set("X-Apple-ADSID", adsid)
	request.Header.Set("Cookie", "termsAccepted=true")
	request.Header.Set("X-MMe-Country", "US")
	request.Header.Set("X-MMe-Language", "en")
	request.Header.Set("Accept", "application/x-plist")
	request.Header.Set("User-Agent", "com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, &StorageError{Stage: "settings-transport"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		_ = response.Body.Close()
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, accountSettingsURL, nil)
		if err != nil {
			return nil, &StorageError{Stage: "settings-request"}
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(dsid+":"+token)))
		request.Header.Set("X-Apple-ADSID", adsid)
		request.Header.Set("Cookie", "termsAccepted=true")
		request.Header.Set("X-MMe-Country", "US")
		request.Header.Set("X-MMe-Language", "en")
		request.Header.Set("Accept", "application/x-plist")
		request.Header.Set("User-Agent", "com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0")
		response, err = client.Do(request)
		if err != nil {
			return nil, &StorageError{Stage: "settings-transport"}
		}
		defer response.Body.Close()
	}
	if response.StatusCode != http.StatusOK {
		return nil, &StorageError{Stage: "settings", HTTPStatus: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return nil, &StorageError{Stage: "settings-response", HTTPStatus: response.StatusCode}
	}
	var settings map[string]any
	if _, err := plist.Unmarshal(body, &settings); err != nil {
		return nil, &StorageError{Stage: "settings-decode", HTTPStatus: response.StatusCode}
	}
	return settings, nil
}

func (e *StorageError) Error() string {
	if storeFailurePattern.MatchString(e.AppleCode) {
		return fmt.Sprintf("icloud: storage %s failed (HTTP %d; Apple code %s)", e.Stage, e.HTTPStatus, e.AppleCode)
	}
	return fmt.Sprintf("icloud: storage %s failed (HTTP %d)", e.Stage, e.HTTPStatus)
}

func dictionary(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func quotaConfig(sd map[string]any) map[string]any {
	if q := dictionary(sd, "com.apple.Dataclass.Quota"); q != nil {
		return q
	}
	if q := dictionary(dictionary(sd, "mme-info"), "quota"); q != nil {
		return q
	}
	if q := dictionary(dictionary(sd, "config"), "com.apple.Dataclass.Quota"); q != nil {
		return q
	}
	// Apple has returned the quota block at different nesting levels in
	// loginDelegates responses. Walk only maps and stop at the first map that
	// contains a supported endpoint template.
	var walk func(map[string]any, int) map[string]any
	walk = func(current map[string]any, depth int) map[string]any {
		if depth > 8 {
			return nil
		}
		if _, ok := current["storageInfoURL"].(string); ok {
			return current
		}
		if _, ok := current["storagePlanURL"].(string); ok {
			return current
		}
		if _, ok := current["quotaUpdateURL"].(string); ok {
			return current
		}
		for _, value := range current {
			if child, ok := value.(map[string]any); ok {
				if found := walk(child, depth+1); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return walk(sd, 0)
}

// ResolveStorageEndpoint uses service data from the authenticated delegate.
// It is not an arbitrary URL fetcher and rejects non-Apple destinations.
func ResolveStorageEndpoint(sd map[string]any, key, appleID, dsid, token string) (string, error) {
	if key != "storageInfoURL" && key != "storagePlanURL" {
		return "", &StorageError{Stage: "configuration"}
	}
	config := quotaConfig(sd)
	template, _ := config[key].(string)
	if template == "" && key == "storageInfoURL" {
		// Older Windows/iCloud responses omit the quota map entirely. This is
		// Apple's documented default quota service shape; the strict host check
		// below still applies before any token is substituted.
		template = "https://p01-quota.icloud.com/quotaservice/external/ios/$dsid$/storageUsageInfo"
	}
	if template == "" {
		return "", &StorageError{Stage: "configuration"}
	}
	// Check the template authority before substituting any secrets.
	if !appleEndpoint(template) {
		return "", &StorageError{Stage: "endpoint"}
	}
	escape := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	resolved := strings.NewReplacer("$apple_id$", escape(appleID), "$dsid$", escape(dsid), "$MMeAuthToken$", escape(token)).Replace(template)
	// Apple ships two placeholder sets: storagePlanURL uses the lower-case
	// names above, while storageInfoURL and quotaUpdateURL arrive with
	// $APPLE_ID$/$DS_PRS_ID$/$UDID$ (the native route pins the UDID literal).
	if strings.Contains(resolved, "$") {
		resolved = strings.NewReplacer("$APPLE_ID$", escape(appleID), "$DS_PRS_ID$", escape(dsid), "$UDID$", "123").Replace(resolved)
	}
	if strings.Contains(resolved, "$") || !appleEndpoint(resolved) {
		return "", &StorageError{Stage: "endpoint"}
	}
	return resolved, nil
}

func appleEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	// The .com.cn authorities are Apple's mainland China deployment, operated
	// by GCBD. A mainland account's quota service only answers there.
	for _, suffix := range []string{"icloud.com", "apple.com", "icloud.com.cn", "apple.com.cn"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// FetchStorageInfo performs a read-only authenticated quota request. Redirects
// are rejected so authentication cannot leak to a different service.
func FetchStorageInfo(ctx context.Context, sd map[string]any, appleID, dsid, adsid, token string, headers map[string]string) (*StorageInfo, error) {
	if dsid == "" || adsid == "" || token == "" {
		return nil, &StorageError{Stage: "authentication"}
	}
	endpoint, err := ResolveStorageEndpoint(sd, "storageInfoURL", appleID, dsid, token)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &StorageError{Stage: "request"}
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(dsid+":"+token)))
	request.Header.Set("X-Apple-ADSID", adsid)
	request.Header.Set("X-MMe-Language", "en")
	request.Header.Set("Accept", "application/x-plist, application/json")
	request.Header.Set("User-Agent", "com.apple.iCloudHelper/282 CFNetwork/1408.0.4 Darwin/22.5.0")
	if udid := request.Header.Get("X-Mme-Device-Id"); udid != "" {
		request.Header.Set("X-Client-UDID", strings.ToLower(udid))
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, &StorageError{Stage: "transport"}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, &StorageError{Stage: "quota", HTTPStatus: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return nil, &StorageError{Stage: "response", HTTPStatus: 200}
	}
	info, err := parseStorageInfo(body)
	if err != nil {
		return nil, &StorageError{Stage: "decode", HTTPStatus: 200}
	}
	_, err = ResolveStorageEndpoint(sd, "storagePlanURL", appleID, dsid, token)
	info.PlanEndpointAvailable = err == nil
	return info, nil
}

func parseStorageInfo(data []byte) (*StorageInfo, error) {
	var body map[string]any
	if _, err := plist.Unmarshal(data, &body); err != nil {
		if err = json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
	}
	storage := dictionary(body, "storage_data")
	quota := dictionary(storage, "quota_info_in_bytes")
	usage := dictionary(storage, "storage_info_in_bytes")
	if quota == nil || usage == nil {
		return nil, errors.New("missing quota data")
	}
	total, okTotal := byteCount(quota["total_quota"])
	used, okUsed := byteCount(quota["total_used"])
	available, okAvailable := byteCount(quota["total_available"])
	// The Windows client tracks the paid state as quota_status.paid_quota and
	// never reads a paid byte count; that field is best-effort here too.
	paid, _ := byteCount(usage["commerce_storage_in_bytes"])
	if !okTotal || !okUsed || !okAvailable {
		return nil, errors.New("invalid quota values")
	}
	return &StorageInfo{TotalBytes: total, UsedBytes: used, AvailableBytes: available, PaidBytes: paid}, nil
}

func byteCount(value any) (uint64, bool) {
	switch v := value.(type) {
	case uint64:
		return v, true
	case int64:
		return uint64(v), v >= 0
	case float64:
		if v >= 0 && v < 1<<53 && v == float64(uint64(v)) {
			return uint64(v), true
		}
	}
	return 0, false
}
