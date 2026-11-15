package engine

// R5 regression: the adapter must not become a direct plaintext client when a
// proxy is configured, and plain http is only allowed for loopback fixtures.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

func TestPlainHTTPDoesNotBypassProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()

	transport, err := New(Options{
		Profile:  profileFor(t, fingerprint.KindChrome),
		ProxyURL: "socks5://127.0.0.1:1",
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()

	resp, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("plain HTTP reached the target despite an unreachable required proxy: %d", resp.StatusCode)
	}
}

func TestLoopbackPlainHTTPIsExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()

	// Opted in: the fixture may be reached in plaintext.
	open, err := New(Options{
		Profile:           profileFor(t, fingerprint.KindChrome),
		Timeout:           time.Second,
		AllowLoopbackHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: open, Timeout: time.Second}).Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback fixture should be reachable when explicitly allowed: %v", err)
	}
	resp.Body.Close()

	// Not opted in: refused even though it is loopback.
	closed, err := New(Options{Profile: profileFor(t, fingerprint.KindChrome), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&http.Client{Transport: closed, Timeout: time.Second}).Get(srv.URL); err == nil {
		t.Fatal("plain http to loopback must require an explicit opt-in")
	}
}

// A loopback fixture may redirect outward; that hop must be refused before the
// connection is made, otherwise the transport would dial an external origin with
// certificate verification still disabled.
func TestInsecureDoesNotFollowExternalRedirect(t *testing.T) {
	var outside atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://outside.invalid/final", http.StatusFound)
			return
		}
		if r.Host == "outside.invalid" {
			outside.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tr, err := New(Options{Profile: profileFor(t, fingerprint.KindChrome), Insecure: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()

	// Offline routing only: keep URL and Host intact, map the dial to the fixture.
	dial := tr.rt.DialTLSContext
	tr.rt.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dial(ctx, network, strings.TrimPrefix(srv.URL, "https://"))
	}
	client := &http.Client{Transport: tr, CheckRedirect: CheckRedirect, Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/start")
	if resp != nil {
		resp.Body.Close()
	}
	if got := outside.Load(); got != 0 {
		t.Fatalf("insecure transport sent %d request(s) to an external origin", got)
	}
	if err == nil {
		t.Fatal("an external redirect must fail while verification is disabled")
	}
}
