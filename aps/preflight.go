package aps

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

	"github.com/Laky-64/appleservices/gsa"
)

// Preflight uses the worker's courier dialer without sending any APS auth frame.
func Preflight(ctx context.Context, direct bool) (string, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM([]byte(gsa.AppleRootCAPEM))
	dial := dialCourier
	if direct {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		}
	}
	return preflight(ctx, dial, &tls.Config{RootCAs: pool, ServerName: "courier.push.apple.com", NextProtos: []string{"apns-security-v3"}, MinVersion: tls.VersionTLS12})
}

func preflight(ctx context.Context, dial func(context.Context, string) (net.Conn, error), config *tls.Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := dial(ctx, "1-courier.push.apple.com:5223")
	if err != nil {
		return "connect", err
	}
	defer conn.Close()
	secure := tls.Client(conn, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		return "tls", err
	}
	return "tls", nil
}
