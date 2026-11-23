package gsa

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"howett.net/plist"
)

func TestAppleTransportPreservesProxyAndCertificateVerification(t *testing.T) {
	transport := AppleRootTransport().(*http.Transport)
	if transport.Proxy == nil {
		t.Fatal("Apple requests bypass configured environment proxy")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("Apple certificate verification is not enabled")
	}
}

// Reproduce the reported edge behavior locally: one response per connection.
// The old pooled transport gets 429 on complete; the fixed transport succeeds
// on two connections while still carrying the cookie set by init.
func TestGSAUsesFreshConnectionButKeepsSessionCookies(t *testing.T) {
	type connectionKey struct{}
	var mu sync.Mutex
	counts := map[net.Conn]int{}
	response, err := plist.Marshal(map[string]any{"Response": map[string]any{"Status": map[string]any{"ec": int64(0)}}}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := r.Context().Value(connectionKey{}).(net.Conn)
		mu.Lock()
		counts[conn]++
		count := counts[conn]
		mu.Unlock()
		if count > 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var envelope map[string]any
		if _, err := plist.Unmarshal(data, &envelope); err != nil {
			t.Error(err)
			return
		}
		operation := envelope["Request"].(map[string]any)["o"]
		if operation == "init" {
			http.SetCookie(w, &http.Cookie{Name: "test-trust", Value: "test-cookie", Path: "/", Secure: true})
		} else {
			cookie, err := r.Cookie("test-trust")
			if err != nil || cookie.Value != "test-cookie" {
				t.Error("lost session cookie between new connections")
			}
			if r.ProtoMajor != 1 || !r.Close {
				t.Error("GSA request did not use a single-use HTTP/1 connection")
			}
		}
		w.Write(response)
	}))
	server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connectionKey{}, c)
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	configure := func(transport *http.Transport) {
		transport.Proxy = nil
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: server.Certificate().DNSNames[0]}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
	}
	for _, fixed := range []bool{false, true} {
		name := "pooled-reproduction"
		if fixed {
			name = "fixed"
		}
		t.Run(name, func(t *testing.T) {
			c := NewClient(headerProvider{})
			transport := c.transport.(*authenticationTransport)
			configure(transport.shared)
			configure(transport.gsa)
			defer transport.shared.CloseIdleConnections()
			defer transport.gsa.CloseIdleConnections()
			if !fixed {
				c.transport = transport.shared
			}
			if _, err := c.doRequest(map[string]any{"o": "init"}); err != nil {
				t.Fatal(err)
			}
			_, err := c.doRequest(map[string]any{"o": "complete"})
			if fixed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			// A pooled connection's 429 must surface without replaying the proof.
			if err == nil {
				t.Fatal("a permanently rejected pooled connection was reported as success")
			}
		})
	}
}
