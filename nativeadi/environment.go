package nativeadi

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Environment scopes an explicit HTTP CONNECT proxy to the native helper only.
// A SOCKS endpoint must never be silently converted to an HTTP endpoint.
func Environment() ([]string, error) {
	env := bridgeEnvironment()
	proxy := strings.TrimSpace(os.Getenv("APPLESERVICES_NATIVE_HTTP_PROXY"))
	if proxy == "" {
		return env, nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u == nil {
		return nil, errors.New("nativeadi: native_proxy_invalid")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(proxy, "\r\n\x00{}") {
		return nil, errors.New("nativeadi: native_proxy_invalid")
	}
	result := make([]string, 0, len(env)+4)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "APPLESERVICES_SOCKS_PROXY", "APPLESERVICES_NATIVE_HTTP_PROXY":
			continue
		}
		result = append(result, entry)
	}
	return append(result, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "http_proxy="+proxy, "https_proxy="+proxy), nil
}
