package activationenv

import (
	"context"
	"net"
	"net/http"
	"os"
)

// Scoped to activation children; browser and system routing are unchanged.
func Network() string {
	if os.Getenv("APPLESERVICES_ACTIVATION_IPV4") == "1" {
		return "tcp4"
	}
	return "tcp"
}

func ConfigureTransport(transport *http.Transport) {
	if Network() != "tcp4" {
		return
	}
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network == "tcp" {
			network = "tcp4"
		}
		return dial(ctx, network, address)
	}
}

func HTTPTransport() http.RoundTripper {
	if Network() != "tcp4" {
		return http.DefaultTransport
	}
	if original, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := original.Clone()
		ConfigureTransport(transport)
		return transport
	}
	return http.DefaultTransport
}
