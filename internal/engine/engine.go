// Package engine turns a fingerprint profile into an actual transport.
//
// Why this exists: a profile that only reaches a User-Agent header is metadata,
// not a browser. This adapter applies the *engine-level* half — the TLS
// ClientHello shape — with utls, so "the profile was applied" is a fact about
// the socket rather than a claim about a JSON import.
//
// Contract limits, stated plainly:
//   - HTTP/1.1 only. The hello advertises http/1.1 in ALPN; we do not claim h2
//     stream settings we do not send.
//   - Pinned hello profiles, no randomisation. The contract forbids migrating
//     custom TLS/HTTP2 perturbation, and a randomised hello would make the
//     "which template was used" receipt meaningless.
//   - No retry policy lives here. The caller owns intent and replay rules.
package engine

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

// hellos maps a template's engine requirement onto a *pinned* utls preset.
//
// Pinned, not Auto: an Auto alias follows the library upgrade, so a template
// frozen as "Safari 15.3" would quietly start posing as Safari 18 after a
// dependency bump. Each entry names the exact preset the template was validated
// against, and a template whose version has no pinned preset is never selected.
var hellos = map[string]utls.ClientHelloID{
	"HelloChrome_131":  utls.HelloChrome_131,
	"HelloChrome_133":  utls.HelloChrome_133,
	"HelloFirefox_120": utls.HelloFirefox_120,
	"HelloSafari_16_0": utls.HelloSafari_16_0,
	"HelloIOS_14":      utls.HelloIOS_14,
}

// maxBrowser is what this build of utls can pose as, per kind. Selection uses
// it to drop templates the engine cannot reach.
var maxBrowser = map[fingerprint.Kind]int{
	fingerprint.KindChrome:    133,
	fingerprint.KindFirefox:   120,
	fingerprint.KindMacSafari: 16,
	fingerprint.KindIOSSafari: 14,
}

// Capabilities reports what this adapter can do, for selection and receipts.
func Capabilities() fingerprint.Capability {
	names := make([]string, 0, len(hellos))
	for name := range hellos {
		names = append(names, name)
	}
	return fingerprint.Capability{
		Name:       "utls",
		Version:    moduleVersion(),
		Hellos:     names,
		MaxBrowser: maxBrowser,
	}
}

// Options configure one transport. Kind is not used directly for the hello —
// the profile's Engine.Name is — but it is kept so receipts can report the pair
// and so a mismatch is caught early.
type Options struct {
	Profile  fingerprint.Profile
	ProxyURL string
	Timeout  time.Duration
	// Insecure disables certificate verification. It exists for the offline
	// tests only; a production receipt records it as false.
	Insecure bool
}

// Transport is an http.RoundTripper that talks through the profile's hello.
type Transport struct {
	kind     fingerprint.Kind
	hello    string
	proxy    string
	timeout  time.Duration
	insecure bool
	rt       *http.Transport
}

// New builds the transport and validates that the profile's hello is reachable.
func New(opts Options) (*Transport, error) {
	hello := opts.Profile.Engine.Name
	if _, ok := hellos[hello]; !ok {
		return nil, fmt.Errorf("engine: hello %q is not supported by this build", hello)
	}
	if limit, ok := maxBrowser[opts.Profile.Kind]; ok && limit > 0 && opts.Profile.MajorHint() > limit {
		return nil, fmt.Errorf("engine: %s major %d exceeds what utls can emulate (%d)",
			opts.Profile.Kind, opts.Profile.MajorHint(), limit)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	t := &Transport{
		kind:     opts.Profile.Kind,
		hello:    hello,
		proxy:    strings.TrimSpace(opts.ProxyURL),
		timeout:  opts.Timeout,
		insecure: opts.Insecure,
	}
	t.rt = &http.Transport{
		DialTLSContext:      t.dialTLS,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   false,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: opts.Timeout,
	}
	return t, nil
}

// RoundTrip satisfies http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.rt.RoundTrip(req)
}

// CloseIdleConnections releases pooled sockets.
func (t *Transport) CloseIdleConnections() { t.rt.CloseIdleConnections() }

// Describe is the receipt line for this transport: the chosen template is
// reported by the caller, the *applied* engine is reported here.
func (t *Transport) Describe() map[string]any {
	return map[string]any{
		"name":       "utls",
		"version":    moduleVersion(),
		"hello":      t.hello,
		"kind":       string(t.kind),
		"http":       "1.1",
		"applied":    true,
		"proxySet":   t.proxy != "",
		"insecure":   t.insecure,
		"h2Disabled": true,
	}
}

func (t *Transport) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := t.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		raw.Close()
		return nil, err
	}
	cfg := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: t.insecure,
		MinVersion:         tls.VersionTLS12,
	}
	// HelloCustom is required: ApplyPreset is ignored when the client was built
	// with a named preset, which is why the previous code still offered h2.
	conn := utls.UClient(raw, cfg, utls.HelloCustom)
	// A preset carries its own ALPN extension, and it overrides Config.NextProtos.
	// Rewrite that extension so the offer is http/1.1 only: we speak HTTP/1.1 and
	// must never let the server pick h2 for us.
	spec, err := utls.UTLSIdToSpec(hellos[t.hello])
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("engine: preset %s has no usable spec: %w", t.hello, err)
	}
	replaced := false
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
			replaced = true
		}
	}
	if !replaced {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}})
	}
	if err := conn.ApplyPreset(&spec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("engine: apply preset %s: %w", t.hello, err)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("engine: handshake with %s failed: %w", t.hello, err)
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		raw.Close()
		return nil, fmt.Errorf("engine: server negotiated ALPN %q, but this adapter speaks http/1.1 only", got)
	}
	return conn, nil
}

// dial connects directly or through the configured proxy.
func (t *Transport) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.proxy == "" {
		d := &net.Dialer{Timeout: t.timeout}
		return d.DialContext(ctx, network, addr)
	}
	parsed, err := url.Parse(t.proxy)
	if err != nil {
		return nil, fmt.Errorf("engine: bad proxy url: %w", err)
	}
	switch parsed.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, &net.Dialer{Timeout: t.timeout})
		if err != nil {
			return nil, fmt.Errorf("engine: socks5 dialer: %w", err)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	case "http", "https":
		return nil, errors.New("engine: http proxies are not supported by the utls adapter; use socks5")
	default:
		return nil, fmt.Errorf("engine: unsupported proxy scheme %q", parsed.Scheme)
	}
}

// moduleVersion reports the utls version actually linked into the binary.
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/refraction-networking/utls" {
				return dep.Version
			}
		}
	}
	return "unknown"
}
