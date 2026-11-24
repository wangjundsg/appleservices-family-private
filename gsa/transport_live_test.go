package gsa

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in, credential-free smoke test. Never sends a login, proof, or SMS request.
func TestGSAConnectionRecoveryLive(t *testing.T) {
	if os.Getenv("APPLESERVICES_TEST_GSA_NETWORK") != "1" {
		t.Skip("requires opt-in live network check")
	}
	for _, injectFailure := range []bool{false, true} {
		name := "normal"
		if injectFailure {
			name = "injected-TLS-EOF"
		}
		t.Run(name, func(t *testing.T) {
			transport := newAuthenticationTransport()
			defer transport.shared.CloseIdleConnections()
			defer transport.gsa.CloseIdleConnections()
			var dials, resolutions atomic.Int32
			resolve := transport.resolveFallback
			transport.resolveFallback = func(ctx context.Context) ([]netip.Addr, error) { resolutions.Add(1); return resolve(ctx) }
			dial := transport.gsa.DialContext
			transport.gsa.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if dials.Add(1) == 1 && injectFailure {
					a, b := net.Pipe()
					go func() { defer b.Close(); buf := make([]byte, 8192); _, _ = b.Read(buf) }()
					return a, nil
				}
				return dial(ctx, network, address)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://gsa.apple.com/grandslam/GsService2", nil)
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 || response.TLS.ServerName != "gsa.apple.com" {
				t.Fatal("missing verified Apple TLS connection")
			}
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("unexpected credential-free endpoint status=%d", response.StatusCode)
			}
			if injectFailure && resolutions.Load() != 1 {
				t.Fatal("recovery resolver was not exercised")
			}
			t.Logf("HTTP=%d verified-TLS=true recovery-lookups=%d", response.StatusCode, resolutions.Load())
		})
	}
}
