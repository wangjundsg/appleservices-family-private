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

func TestCourierTLSConnectionRecovery(t *testing.T) {
	for _, mode := range []string{"transient", "permanent", "bad-certificate", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("must not send application data during TLS recovery")
			}))
			defer server.Close()
			pool := x509.NewCertPool()
			if mode != "bad-certificate" {
				pool.AddCert(server.Certificate())
			}
			cfg := &tls.Config{RootCAs: pool, ServerName: server.Certificate().DNSNames[0]}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			dials := 0
			conn, err := dialCourierTLS(ctx, cfg, func(ctx context.Context, address string) (net.Conn, error) {
				dials++
				if address != "1-courier.push.apple.com:5223" {
					t.Fatal("changed courier")
				}
				if mode == "permanent" {
					return nil, io.EOF
				}
				if mode == "transient" && dials == 1 {
					a, b := net.Pipe()
					go func() { defer b.Close(); _, _ = b.Read(make([]byte, 4096)) }()
					return a, nil
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			})
			if conn != nil {
				conn.Close()
			}
			switch mode {
			case "transient":
				if err != nil || dials != 2 {
					t.Fatalf("err=%v dials=%d", err, dials)
				}
			case "permanent":
				if err == nil || dials != 3 {
					t.Fatalf("err=%v dials=%d", err, dials)
				}
			case "bad-certificate":
				var invalid *tls.CertificateVerificationError
				if !errors.As(err, &invalid) || dials != 1 {
					t.Fatalf("err=%v dials=%d", err, dials)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || dials != 0 {
					t.Fatalf("err=%v dials=%d", err, dials)
				}
			}
		})
	}
}
