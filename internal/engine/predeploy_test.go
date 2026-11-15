package engine

// R5 regression: the adapter must not become a direct plaintext client when a
// proxy is configured, and plain http is only allowed for loopback fixtures.

import (
	"net/http"
	"net/http/httptest"
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
