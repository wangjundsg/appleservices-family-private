package dashboard

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
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
	Attempts   int    `json:"attempts,omitempty"`
	Failure    string `json:"failure,omitempty"`
	retryable  bool
}

func probeHTTP(ctx context.Context, stage, endpoint string, transport *http.Transport) NetworkCheck {
	defer transport.CloseIdleConnections()
	var check NetworkCheck
	for attempt := 1; attempt <= 2; attempt++ {
		check = probeHTTPOnce(ctx, stage, endpoint, transport)
		check.Attempts = attempt
		if !check.retryable || ctx.Err() != nil {
			break
		}
	}
	return check
}

func probeHTTPOnce(ctx context.Context, stage, endpoint string, transport *http.Transport) NetworkCheck {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var phase atomic.Value
	phase.Store("connect")
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn:           func(httptrace.GotConnInfo) { connected.Store(true) },
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
		failure, retryable := preflightFailure(err)
		return NetworkCheck{Stage: stage + "-" + phase.Load().(string), Failure: failure, retryable: !connected.Load() && retryable}
	}
	response.Body.Close()
	// A 403/404/405 proves transport reachability only, never login permission.
	ok := response.StatusCode < 500 && response.StatusCode != http.StatusProxyAuthRequired && response.StatusCode != http.StatusTooManyRequests
	return NetworkCheck{OK: ok, Stage: stage + "-http", HTTPStatus: response.StatusCode}
}

func preflightFailure(err error) (string, bool) {
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "certificate", false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", true
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout", true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection_closed", true
	}
	if errors.As(err, &network) {
		return "connect", true
	}
	return "transport", false
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

// Public HEAD responses cannot decide whether native provisioning or an
// authenticated activation succeeds. Start checks local prerequisites only.
func (s *Server) activationRuntimeCheck() control.Result {
	if err := nativeadi.ValidateLibrary(s.config.Library); err != nil {
		return control.Result{Stage: "native-library", Message: "原生组件文件不可用或校验失败；尚未启动登录。"}
	}
	if err := nativeadi.CheckRuntime(s.config.Python); err != nil {
		return control.Result{Stage: "native-runtime", Message: "本机 Python 或编译环境不可用；尚未启动登录。"}
	}
	return control.Result{OK: true}
}

func networkConfiguration() map[string]string {
	return map[string]string{
		"source": os.Getenv("APPLESERVICES_NETWORK_CONFIG_SOURCE"),
		"sha256": os.Getenv("APPLESERVICES_NETWORK_CONFIG_SHA256"),
		"native": "direct", "gsa": "direct", "aps": "direct",
		"nac_worker": "direct", "nac_dashboard": "direct", "mac_aps_bootstrap": "direct",
	}
}
