package dashboard

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightRecoversOneConnectFailureWithoutSendingCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("probe sent credentials or changed method")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	dial := (&net.Dialer{}).DialContext
	dials := 0
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return nil, &net.DNSError{IsTimeout: true, Err: "fixture-private-detail"}
		}
		return dial(ctx, network, address)
	}
	result := probeHTTP(context.Background(), "fixture", server.URL, transport)
	if !result.OK || dials != 2 || result.Attempts != 2 {
		t.Fatalf("did not recover bounded preflight: %+v dials=%d", result, dials)
	}
}

func TestPreflightNeverRetriesAnHTTPResponseOrUntrustedCertificate(t *testing.T) {
	for _, mode := range []string{"503", "response-eof", "certificate"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "response-eof" {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			transport := server.Client().Transport.(*http.Transport).Clone()
			if mode == "certificate" {
				transport = &http.Transport{}
			}
			result := probeHTTP(context.Background(), "fixture", server.URL, transport)
			if result.OK || result.Attempts != 1 || calls > 1 {
				t.Fatalf("unsafe retry: %+v calls=%d", result, calls)
			}
		})
	}
}

func TestPreflightConnectRetryIsBoundedAndRedacted(t *testing.T) {
	dials := 0
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { dials++; return nil, io.EOF }}
	result := probeHTTP(context.Background(), "fixture", "https://example.test", transport)
	if result.OK || dials != 2 || result.Attempts != 2 || result.Failure != "connection_closed" {
		t.Fatalf("unexpected bounded failure: %+v dials=%d", result, dials)
	}
}

func TestPreflightHTTPChecksTLSAndDoesNotTreat403AsLogin(t *testing.T) {
	for _, status := range []int{200, 403, 407, 503} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "HEAD" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("not an unauthenticated probe")
			}
			w.WriteHeader(status)
		}))
		result := probeHTTP(context.Background(), "fixture", server.URL, server.Client().Transport.(*http.Transport))
		if result.OK != (status == 200 || status == 403) || result.HTTPStatus != status {
			t.Fatalf("unexpected probe result: %+v", result)
		}
		untrusted := probeHTTP(context.Background(), "fixture", server.URL, &http.Transport{})
		if untrusted.OK || untrusted.Stage != "fixture-tls" {
			t.Fatalf("untrusted TLS accepted: %+v", untrusted)
		}
		server.Close()
	}
}

func TestNativePreflightUsesExplicitRouteDespiteNoProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv("APPLESERVICES_NATIVE_HTTP_PROXY", "http://private-marker:private-password@"+address)
	t.Setenv("NO_PROXY", "*")
	s := registerTestServer(t)
	s.config.Worker = filepath.Join(t.TempDir(), "unused.exe")
	if err := os.WriteFile(s.config.Worker, []byte("unused"), 0600); err != nil {
		t.Fatal(err)
	}
	s.provisionMacPush = func(context.Context, string) error { t.Fatal("provisioning ran after network failure"); return nil }
	dir := t.TempDir()
	result := s.startIn(context.Background(), dir, credentials{AppleID: "owner@example.test", Password: "private-password"}, nil)
	if result.OK || result.Stage != "native-route-connect" || strings.Contains(result.Message, "private-") {
		t.Fatalf("unsafe or missing preflight: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "daemon.out.log")); !os.IsNotExist(err) {
		t.Fatal("worker launch reached")
	}
}

func TestVerificationPreflightDiagnosticKeepsStageWithoutPrivateError(t *testing.T) {
	for _, phase := range []string{"connect", "tls", "http"} {
		path := filepath.Join(t.TempDir(), "error.log")
		if err := os.WriteFile(path, []byte("gsa: verification_preflight_"+phase+" private-marker"), 0600); err != nil {
			t.Fatal(err)
		}
		message := lastProcessError(path)
		if !strings.Contains(message, "GSA/"+phase) || strings.Contains(message, "private-marker") {
			t.Fatal("lost stage or leaked stderr")
		}
	}
}
