package aps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreflightDoesNotSendPushAuthentication(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected application data") }))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		if address != "1-courier.push.apple.com:5223" {
			t.Fatal("changed courier endpoint")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	phase, err := preflight(context.Background(), dial, &tls.Config{RootCAs: pool, ServerName: "example.com"})
	if err != nil || phase != "tls" {
		t.Fatalf("probe failed: %s %v", phase, err)
	}
	phase, err = preflight(context.Background(), func(context.Context, string) (net.Conn, error) { return nil, io.EOF }, &tls.Config{})
	if !errors.Is(err, io.EOF) || phase != "connect" {
		t.Fatal("lost connect stage")
	}
}
