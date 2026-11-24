package gsa

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"sync/atomic"
)

// The GSA service can reject the second request on a reused connection with 429.
// Keep cookies, proxy configuration, and certificate verification; retire only
// connections used by gsa.apple.com after each response.
type authenticationTransport struct {
	resolveFallback func(context.Context) ([]netip.Addr, error)
	shared          *http.Transport
	gsa             *http.Transport
}

func newAuthenticationTransport() *authenticationTransport {
	shared := AppleRootTransport().(*http.Transport)
	unpooled := shared.Clone()
	unpooled.DisableKeepAlives = true
	// An HTTP/2 connection can carry multiple authentication requests. Pin GSA
	// to HTTP/1 so Connection: close also closes the TLS/proxy tunnel each time.
	unpooled.Protocols = new(http.Protocols)
	unpooled.Protocols.SetHTTP1(true)
	return &authenticationTransport{shared: shared, gsa: unpooled, resolveFallback: func(ctx context.Context) ([]netip.Addr, error) { return resolveGSAFallback(ctx, shared) }}
}

func (t *authenticationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() != "gsa.apple.com" {
		return t.shared.RoundTrip(request)
	}
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	copy := request.Clone(httptrace.WithClientTrace(request.Context(), trace))
	copy.Close = true
	response, err := t.gsa.RoundTrip(copy)
	// Once a connection was handed to HTTP, a proof or OTP may have been sent.
	// Only recover connection setup failures; never replay an ambiguous request.
	if err == nil || connected.Load() || request.Context().Err() != nil || request.URL.Scheme != "https" || !gsaConnectionFailure(err) || t.resolveFallback == nil || (request.Body != nil && request.Body != http.NoBody && request.GetBody == nil) {
		return response, err
	}
	addresses, resolveErr := t.resolveFallback(request.Context())
	if resolveErr != nil || len(addresses) == 0 {
		return response, err
	}
	fallback := t.gsa.Clone()
	defer fallback.CloseIdleConnections()
	// Keep the original proxy/NO_PROXY decision even though CONNECT uses an IP.
	if t.gsa.Proxy != nil {
		selected, proxyErr := t.gsa.Proxy(request)
		if proxyErr != nil {
			return nil, proxyErr
		}
		fallback.Proxy = func(*http.Request) (*url.URL, error) { return selected, nil }
	}
	fallback.TLSClientConfig = t.gsa.TLSClientConfig.Clone()
	if fallback.TLSClientConfig.ServerName == "" {
		fallback.TLSClientConfig.ServerName = request.URL.Hostname()
	}
	port := request.URL.Port()
	if port == "" {
		port = "443"
	}
	for i, address := range addresses {
		if i >= 2 {
			break
		}
		connected.Store(false)
		retry := request.Clone(httptrace.WithClientTrace(request.Context(), trace))
		retry.Close = true
		retry.Host = request.Host
		if retry.Host == "" {
			retry.Host = request.URL.Host
		}
		retry.URL.Host = net.JoinHostPort(address.String(), port)
		if request.GetBody != nil {
			retry.Body, err = request.GetBody()
			if err != nil {
				return nil, err
			}
		}
		response, err = fallback.RoundTrip(retry)
		if err == nil || connected.Load() || request.Context().Err() != nil || !gsaConnectionFailure(err) {
			return response, err
		}
	}
	return response, err
}

func gsaConnectionFailure(err error) bool {
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}
