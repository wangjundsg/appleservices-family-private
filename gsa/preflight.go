package gsa

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// RoutePreflightError never includes proxy credentials or server response data.
type RoutePreflightError struct{ Stage string }

func (e *RoutePreflightError) Error() string { return "gsa: verification_preflight_" + e.Stage }

func (c *Client) preflightSecondFactor() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodHead, gsaBaseURL+"/", nil)
	// Use this client's actual route/profile, without its cookies or auth headers.
	client := &http.Client{Transport: c.twoFactorTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return &RoutePreflightError{Stage: phase.Load().(string)}
	}
	response.Body.Close()
	if response.StatusCode >= 500 || response.StatusCode == 407 || response.StatusCode == 429 {
		return &RoutePreflightError{Stage: "http"}
	}
	return nil
}
