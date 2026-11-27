package gsa

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGSAConnectDeadlineLeavesTimeForRecovery(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	fixture := original.(*http.Transport).Clone()
	fixture.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Error("connect can consume entire login deadline")
		}
		return nil, io.EOF
	}
	http.DefaultTransport = fixture
	transport := newAuthenticationTransport()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = transport.gsa.DialContext(ctx, "tcp", "gsa.apple.com:443")
	if ctx.Err() != nil {
		t.Fatal("connection attempt cancelled original login")
	}
}

func TestGSARecoveryUsesSystemDNSBeforeExternalResolver(t *testing.T) {
	lookup := func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip4" || host != "gsa.apple.com" {
			t.Fatal("unexpected lookup")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("17.1.2.3")}, nil
	}
	addresses, err := resolveGSAFallbackWithResolver(context.Background(), nil, lookup)
	if err != nil || len(addresses) != 1 || addresses[0].String() != "17.1.2.3" {
		t.Fatal("system DNS recovery failed")
	}
}

func TestGSAFallbackOnlyBeforeRequest(t *testing.T) {
	for _, mode := range []string{"tls-eof", "connect-eof", "response-eof", "http-503", "bad-certificate", "fallback-bad-certificate"} {
		t.Run(mode, func(t *testing.T) {
			var received, lookups, dials atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				if r.Host != "gsa.apple.com" {
					t.Errorf("changed HTTP host: %s", r.Host)
				}
				if mode == "response-eof" {
					c, _, _ := w.(http.Hijacker).Hijack()
					c.Close()
					return
				}
				if mode == "http-503" {
					w.WriteHeader(503)
					return
				}
				data, _ := io.ReadAll(r.Body)
				if string(data) != "fixture-proof" {
					t.Error("lost request body")
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			transport := newAuthenticationTransport()
			transport.gsa.Proxy = nil
			pool := x509.NewCertPool()
			if mode != "bad-certificate" && mode != "fallback-bad-certificate" {
				pool.AddCert(server.Certificate())
			}
			transport.gsa.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: server.Certificate().DNSNames[0]}
			transport.resolveFallback = func(context.Context) ([]netip.Addr, error) {
				lookups.Add(1)
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			transport.gsa.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				n := dials.Add(1)
				if n == 1 && (mode == "connect-eof" || mode == "fallback-bad-certificate") {
					return nil, io.EOF
				}
				if n == 1 && mode == "tls-eof" {
					a, b := net.Pipe()
					go func() { defer b.Close(); buf := make([]byte, 4096); _, _ = b.Read(buf) }()
					return a, nil
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			req, _ := http.NewRequest(http.MethodPost, "https://gsa.apple.com/grandslam/GsService2", strings.NewReader("fixture-proof"))
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			switch mode {
			case "tls-eof", "connect-eof":
				if err != nil || received.Load() != 1 || lookups.Load() != 1 || dials.Load() != 2 {
					t.Fatalf("err=%v requests=%d lookups=%d dials=%d", err, received.Load(), lookups.Load(), dials.Load())
				}
			case "fallback-bad-certificate":
				var certificateError *tls.CertificateVerificationError
				if !errors.As(err, &certificateError) || received.Load() != 0 || lookups.Load() != 1 || dials.Load() != 2 {
					t.Fatalf("fallback skipped TLS validation: err=%v requests=%d", err, received.Load())
				}
			case "http-503":
				if err != nil || resp.StatusCode != 503 || received.Load() != 1 || lookups.Load() != 0 {
					t.Fatal("HTTP refusal was retried")
				}
			default:
				if err == nil || lookups.Load() != 0 || dials.Load() != 1 {
					t.Fatalf("unsafe retry err=%v lookups=%d dials=%d", err, lookups.Load(), dials.Load())
				}
			}
		})
	}
}

func TestGSAFallbackPreservesProxySelection(t *testing.T) {
	transport := newAuthenticationTransport()
	var lookup, connect atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connect.Add(1)
		if r.Method != http.MethodConnect {
			t.Error("proxy not used")
		}
		if connect.Load() == 2 && r.Host != "17.1.2.3:443" {
			t.Errorf("fallback CONNECT target=%s", r.Host)
		}
		c, buf, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		_ = buf.Flush()
		data := make([]byte, 4096)
		_, _ = buf.Read(data)
	}))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)
	transport.gsa.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Hostname() != "gsa.apple.com" {
			t.Error("re-evaluated proxy for IP instead of original domain")
		}
		return proxyURL, nil
	}
	transport.resolveFallback = func(context.Context) ([]netip.Addr, error) {
		lookup.Add(1)
		return []netip.Addr{netip.MustParseAddr("17.1.2.3")}, nil
	}
	req, _ := http.NewRequest(http.MethodPost, "https://gsa.apple.com/grandslam/GsService2", strings.NewReader("fixture-proof"))
	_, err := transport.RoundTrip(req)
	if err == nil || lookup.Load() != 1 || connect.Load() != 2 {
		t.Fatalf("err=%v lookup=%d connects=%d", err, lookup.Load(), connect.Load())
	}
}

func TestGSAFallbackStopsOnCanceledContext(t *testing.T) {
	transport := newAuthenticationTransport()
	transport.gsa.Proxy = nil
	transport.resolveFallback = func(context.Context) ([]netip.Addr, error) { t.Fatal("resolved after cancellation"); return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://gsa.apple.com/grandslam/GsService2", strings.NewReader("fixture-proof"))
	_, err := transport.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGSAFallbackBoundedAndRequiresReplayableBody(t *testing.T) {
	for _, replayable := range []bool{true, false} {
		transport := newAuthenticationTransport()
		transport.gsa.Proxy = nil
		dials, lookups := 0, 0
		transport.gsa.DialContext = func(context.Context, string, string) (net.Conn, error) { dials++; return nil, io.EOF }
		transport.resolveFallback = func(context.Context) ([]netip.Addr, error) {
			lookups++
			return []netip.Addr{netip.MustParseAddr("17.1.2.3"), netip.MustParseAddr("17.1.2.4"), netip.MustParseAddr("17.1.2.5")}, nil
		}
		req, _ := http.NewRequest(http.MethodPost, "https://gsa.apple.com/grandslam/GsService2", strings.NewReader("fixture-proof"))
		if !replayable {
			req.GetBody = nil
		}
		_, err := transport.RoundTrip(req)
		if err == nil || (replayable && (dials != 3 || lookups != 1)) || (!replayable && (dials != 1 || lookups != 0)) {
			t.Fatalf("replayable=%t dials=%d lookups=%d err=%v", replayable, dials, lookups, err)
		}
	}
}

func TestGSAProtocolRequestCanRecoverConnection(t *testing.T) {
	received := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Response</key><dict><key>Status</key><dict><key>ec</key><integer>0</integer></dict></dict></dict></plist>`))
	}))
	defer server.Close()
	client := NewClient(headerProvider{})
	transport := client.transport.(*authenticationTransport)
	transport.gsa.Proxy = nil
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.gsa.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: server.Certificate().DNSNames[0]}
	dials := 0
	transport.gsa.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return nil, io.EOF
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	transport.resolveFallback = func(context.Context) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	_, err := client.gsaRoundTrip(map[string]any{"o": "init"}, []byte("fixture-init"), nil)
	if err != nil || received != 1 || dials != 2 {
		t.Fatalf("real request builder recovery: err=%v received=%d dials=%d", err, received, dials)
	}
}

func TestGSAFallbackDNSResponses(t *testing.T) {
	for _, tc := range []struct {
		body  string
		count int
	}{
		{`{"Status":0,"Answer":[{"type":5,"data":"alias.apple.com"},{"type":1,"data":"17.1.2.3"},{"type":1,"data":"17.1.2.3"},{"type":1,"data":"17.1.2.4"},{"type":1,"data":"17.1.2.5"}]}`, 2},
		{`{"Status":0,"Answer":[{"type":1,"data":"127.0.0.1"},{"type":1,"data":"10.0.0.1"},{"type":1,"data":"198.18.0.53"},{"type":1,"data":"169.254.1.1"},{"type":1,"data":"::1"}]}`, 0},
		{`{"Status":3,"Answer":[{"type":1,"data":"17.1.2.3"}]}`, 0},
		{`{"Answer":[{"type":1,"data":"17.1.2.3"}]}`, 0},
		{`null`, 0}, {`invalid`, 0},
	} {
		addresses, err := parseGSAFallbackDNS(strings.NewReader(tc.body))
		if len(addresses) != tc.count || (tc.count == 0) != (err != nil) {
			t.Fatalf("count=%d err=%v", len(addresses), err)
		}
	}
}
