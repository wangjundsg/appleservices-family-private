package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestIPv4PreservesConfiguredDialerAndTLS(t *testing.T) {
	want := errors.New("configured dialer")
	called := false
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			called = true
			if network != "tcp4" || address != "example.test:443" {
				t.Fatalf("unexpected dial: %s %s", network, address)
			}
			return nil, want
		}}
	useIPv4(transport)
	_, err := transport.DialContext(context.Background(), "tcp", "example.test:443")
	if !called || err != want || transport.Proxy == nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("IPv4 selection changed configured routing or TLS verification")
	}
}
