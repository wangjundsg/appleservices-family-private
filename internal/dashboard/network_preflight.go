package dashboard

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Laky-64/appleservices/aps"
	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/internal/control"
	"github.com/Laky-64/appleservices/nativeadi"
	"golang.org/x/net/http/httpproxy"
)

// NetworkCheck contains only allowlisted diagnostics, never transport errors,
// proxy URLs, response bodies or account data.
type NetworkCheck struct {
	OK         bool   `json:"ok"`
	Stage      string `json:"stage"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func probeHTTP(ctx context.Context, stage, endpoint string, transport *http.Transport) NetworkCheck {
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var phase atomic.Value
	phase.Store("connect")
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { phase.Store("tls") },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				phase.Store("http")
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodHead, endpoint, nil)
	if err != nil {
		return NetworkCheck{Stage: stage + "-config"}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return NetworkCheck{Stage: stage + "-" + phase.Load().(string)}
	}
	response.Body.Close()
	// A 403/404/405 proves transport reachability only, never login permission.
	ok := response.StatusCode < 500 && response.StatusCode != http.StatusProxyAuthRequired && response.StatusCode != http.StatusTooManyRequests
	return NetworkCheck{OK: ok, Stage: stage + "-http", HTTPStatus: response.StatusCode}
}

func nativeRouteTransport() (*http.Transport, error) {
	env, err := nativeadi.Environment()
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[strings.ToUpper(key)] = value
	}
	config := httpproxy.Config{HTTPProxy: values["HTTP_PROXY"], HTTPSProxy: values["HTTPS_PROXY"], NoProxy: values["NO_PROXY"]}
	if config.HTTPSProxy == "" {
		config.HTTPSProxy = values["ALL_PROXY"]
	}
	selectProxy := config.ProxyFunc()
	// Apple service certificates require the repository's pinned Apple roots.
	// Retain that trust policy, but remove the GSA-only SOCKS dial override.
	transport := gsa.AppleRootTransport().(*http.Transport)
	transport.DialContext = http.DefaultTransport.(*http.Transport).DialContext
	transport.Proxy = func(r *http.Request) (*url.URL, error) { return selectProxy(r.URL) }
	return transport, nil
}

// CheckActivationNetwork is credential-free. Native-route probes check the helper's
// selected environment using Go TLS; the real native initialization still runs
// in Client.Start before BeginLogin. No profile is opened or changed here.
func CheckActivationNetwork(ctx context.Context, library, python string) []NetworkCheck {
	var checks []NetworkCheck
	add := func(check NetworkCheck) bool { checks = append(checks, check); return check.OK }
	native, err := nativeRouteTransport()
	if err != nil {
		return []NetworkCheck{{Stage: "native-proxy"}}
	}
	if !add(probeHTTP(ctx, "native-route", "https://gsa.apple.com/", native)) {
		return checks
	}
	if err := nativeadi.ValidateLibrary(library); err != nil {
		add(NetworkCheck{Stage: "native-library"})
		return checks
	}
	if err := nativeadi.CheckRuntime(python); err != nil {
		add(NetworkCheck{Stage: "native-runtime"})
		return checks
	}
	add(NetworkCheck{OK: true, Stage: "native-environment"})
	for _, target := range []struct{ stage, endpoint string }{
		{"gsa-route", "https://gsa.apple.com/"},
		{"verification-route", "https://gsa.apple.com/auth"},
		{"nac-worker-route", "https://static.ess.apple.com/identity/validation/cert-1.0.plist"},
		{"nac-session-route", "https://identity.ess.apple.com/"},
	} {
		if !add(probeHTTP(ctx, target.stage, target.endpoint, gsa.AppleRootTransport().(*http.Transport))) {
			return checks
		}
	}
	phase, err := aps.Preflight(ctx, false)
	if !add(NetworkCheck{OK: err == nil, Stage: "aps-route-" + phase}) {
		return checks
	}
	return checks
}

func (s *Server) activationNetworkPreflight(ctx context.Context, dir string) control.Result {
	for _, check := range CheckActivationNetwork(ctx, s.config.Library, s.config.Python) {
		if !check.OK {
			return control.Result{Stage: check.Stage, Message: fmt.Sprintf("连接预检失败（%s）；尚未启动登录或发送验证码。请检查该阶段的网络路由或原生运行环境。", check.Stage)}
		}
	}
	if !macPushStateReady(dir) {
		phase, err := aps.Preflight(ctx, true)
		if err != nil {
			return control.Result{Stage: "mac-aps-direct-" + phase, Message: "Mac APS 初始化直连预检失败；尚未创建推送身份、登录或发送验证码。"}
		}
	}
	return control.Result{OK: true}
}

func networkConfiguration() map[string]string {
	native := "inherited HTTP(S)_PROXY / NO_PROXY"
	if strings.TrimSpace(os.Getenv("APPLESERVICES_NATIVE_HTTP_PROXY")) != "" {
		native = "APPLESERVICES_NATIVE_HTTP_PROXY (helper only; NO_PROXY removed)"
	}
	gsaRoute := "HTTP(S)_PROXY / NO_PROXY"
	if strings.TrimSpace(os.Getenv("APPLESERVICES_SOCKS_PROXY")) != "" {
		gsaRoute = "APPLESERVICES_SOCKS_PROXY"
	}
	return map[string]string{
		"source": os.Getenv("APPLESERVICES_NETWORK_CONFIG_SOURCE"),
		"sha256": os.Getenv("APPLESERVICES_NETWORK_CONFIG_SHA256"),
		"native": native, "gsa": gsaRoute, "aps": "HTTP(S)_PROXY / NO_PROXY",
		"nac_worker": gsaRoute, "nac_dashboard": "direct (filteredActivationEnv)", "mac_aps_bootstrap": "direct (runMacPushProvisioner)",
	}
}
