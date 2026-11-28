package activationenv

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestIPv4UsesConfiguredDialerWithoutChangingTLSOrProxy(t *testing.T) {
	for _, enabled := range []string{"", "1"} {
		t.Run(enabled, func(t *testing.T) {
			t.Setenv("APPLESERVICES_ACTIVATION_IPV4", enabled)
			want := errors.New("fixture")
			called := ""
			transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DialContext: func(_ context.Context, network, _ string) (net.Conn, error) { called = network; return nil, want }}
			ConfigureTransport(transport)
			_, err := transport.DialContext(context.Background(), "tcp", "example.test:443")
			if called != Network() || err != want || transport.Proxy == nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
				t.Fatal("route or verification changed unexpectedly")
			}
		})
	}
}

func TestIPv4CannotDialAnIPv6Literal(t *testing.T) {
	t.Setenv("APPLESERVICES_ACTIVATION_IPV4", "1")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	ConfigureTransport(transport)
	if connection, err := transport.DialContext(context.Background(), "tcp", "[::1]:1"); err == nil {
		connection.Close()
		t.Fatal("IPv6 connection allowed")
	}
}
