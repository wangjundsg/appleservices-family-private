package appleweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Only public terms metadata is returned; credentials stay in the pending session.
type ICloudTerms struct {
	Version string `json:"version"`
	Title   string `json:"title"`
	Content string `json:"content,omitempty"`
	URL     string `json:"url"`
	AppleID string `json:"apple_id"`
	Host    string `json:"host"`
}

func (c *AppleAuthClient) fetchICloudTerms(ctx context.Context, s *appleAuthSession) (ICloudTerms, error) {
	if s.isAppleAccountManage() || !s.TermsRequired {
		return ICloudTerms{}, errCode("icloud_terms_not_required", "此登录会话不需要确认 iCloud 条款", false)
	}
	var reply struct {
		Terms map[string]json.RawMessage `json:"iCloudTerms"`
	}
	u, _ := url.Parse(s.Endpoints.Setup + "/getTerms")
	q := u.Query()
	q.Set("clientBuildNumber", "2622Build20")
	q.Set("clientMasteringNumber", "2622Build20")
	q.Set("clientId", s.FrameID)
	u.RawQuery = q.Encode()
	status, _, err := c.do(ctx, s, http.MethodGet, u.String(), s.commonHeaders(nil), map[string]string{"locale": firstNonEmpty(s.TermsLocale, "zh_CN")}, &reply, false)
	if err != nil {
		return ICloudTerms{}, errCode("icloud_terms_fetch_failed", fmt.Sprintf("获取 Apple 条款失败（HTTP %d），可重试读取；尚未提交同意", status), true)
	}
	var version json.Number
	raw := strings.TrimSpace(string(reply.Terms["version"]))
	if len(raw) > 0 && raw[0] == '"' {
		_ = json.Unmarshal(reply.Terms["version"], &raw)
	}
	if json.Unmarshal([]byte(raw), &version) != nil || !validTermsVersion(raw) {
		return ICloudTerms{}, errCode("icloud_terms_bad_response", "Apple 未返回有效条款版本，无法提交同意", true)
	}
	terms := ICloudTerms{Version: raw, Title: "iCloud 服务条款", AppleID: s.AppleID, Host: s.Endpoints.Host, URL: "https://www.apple.com/legal/internet-services/icloud/"}
	for _, key := range []string{"text", "content", "terms", "html"} {
		var value string
		if json.Unmarshal(reply.Terms[key], &value) == nil && strings.TrimSpace(value) != "" {
			terms.Content = value
			break
		}
	}
	for _, key := range []string{"url", "termsUrl"} {
		var value string
		if json.Unmarshal(reply.Terms[key], &value) == nil {
			u, e := url.Parse(value)
			if e == nil && u.Scheme == "https" && u.User == nil && (u.Host == "www.apple.com" || u.Host == "www.apple.com.cn") {
				terms.URL = u.String()
				break
			}
		}
	}
	s.Terms = &terms
	return terms, nil
}

func validTermsVersion(version string) bool {
	if len(version) == 0 || len(version) > 16 {
		return false
	}
	for _, ch := range version {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return version != "0"
}

func (c *AppleAuthClient) acceptICloudTerms(ctx context.Context, s *appleAuthSession, version string, accepted bool) (ICloudSession, error) {
	if !accepted {
		return ICloudSession{}, errCode("icloud_terms_consent_required", "请先阅读条款，并明确点击同意后继续", false)
	}
	if s.isAppleAccountManage() || (!s.TermsRequired && !s.TermsAccepted) || s.Terms == nil || version != s.Terms.Version || !validTermsVersion(version) {
		return ICloudSession{}, errCode("icloud_terms_version_mismatch", "条款版本或登录会话已变化，请重新读取条款后确认", false)
	}
	if !s.TermsAccepted {
		// Refresh immediately before accepting: never silently agree to a newer version.
		terms, err := c.fetchICloudTerms(ctx, s)
		if err != nil {
			return ICloudSession{}, err
		}
		if terms.Version != version {
			return ICloudSession{}, errCode("icloud_terms_version_mismatch", "Apple 条款已更新，请重新阅读新版本后确认", false)
		}
		u, _ := url.Parse(s.Endpoints.Setup + "/repairDone")
		q := u.Query()
		q.Set("clientId", s.FrameID)
		q.Set("clientBuildNumber", "2622Build20")
		q.Set("clientMasteringNumber", "2622Build20")
		u.RawQuery = q.Encode()
		var reply struct {
			Success *bool `json:"success"`
		}
		status, _, err := c.do(ctx, s, http.MethodGet, u.String(), s.commonHeaders(nil), map[string]any{"acceptedICloudTerms": json.Number(version)}, &reply, false)
		if err != nil {
			return ICloudSession{}, errCode("icloud_terms_accept_failed", fmt.Sprintf("提交 Apple 条款确认失败（HTTP %d），登录尚未完成", status), true)
		}
		if reply.Success != nil && !*reply.Success {
			return ICloudSession{}, errCode("icloud_terms_accept_failed", "Apple 未接受本次条款确认，登录尚未完成", true)
		}
		s.TermsAccepted = true
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := c.authWithTokenAndValidate(ctx, s)
		if err == nil {
			return result, nil
		}
		if code, _, _ := ErrorDetails(err); code == "icloud_terms_update_needed" {
			s.TermsAccepted = false
			s.Terms = nil
			return ICloudSession{}, err
		}
		if redirect, ok := err.(appleDomainRedirectError); ok && s.switchHost(redirect.Host) {
			continue
		}
		return ICloudSession{}, err
	}
	return ICloudSession{}, errCode("apple_domain_switch_failed", "确认条款后的登录域切换未完成，请重新发起登录", true)
}
