package engine

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

// helloRecorder captures the TLS ClientHello of every connection, which is the
// only honest way to show that the profile reached the socket.
type helloRecorder struct {
	mu       sync.Mutex
	seen     []tls.ClientHelloInfo
	requests []string
}

func (h *helloRecorder) record(chi *tls.ClientHelloInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, *chi)
}

func (h *helloRecorder) snapshot() []tls.ClientHelloInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]tls.ClientHelloInfo, len(h.seen))
	copy(out, h.seen)
	return out
}

func startTLSServer(t *testing.T) (*httptest.Server, *helloRecorder) {
	t.Helper()
	rec := &helloRecorder{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.UserAgent()+"|"+r.URL.Path)
		rec.mu.Unlock()
		fmt.Fprint(w, `{"ok":true}`)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		rec.record(chi)
		return nil, nil
	}}
	srv.StartTLS()
	return srv, rec
}

func profileFor(t *testing.T, kind fingerprint.Kind) fingerprint.Profile {
	t.Helper()
	p, err := fingerprint.Select(fingerprint.Options{
		Kind:     kind,
		Locale:   "en-US",
		Timezone: "America/New_York",
		Region:   fingerprint.Region{Country: "US", Source: "verified-egress"},
		Engine:   Capabilities(),
		Seed:     7,
	})
	if err != nil {
		t.Fatalf("select %s: %v", kind, err)
	}
	return p
}

func TestCapabilitiesCoverFourFamilies(t *testing.T) {
	cap := Capabilities()
	if cap.Name != "utls" {
		t.Fatalf("engine name = %q", cap.Name)
	}
	for _, hello := range []string{"HelloChrome_Auto", "HelloFirefox_Auto", "HelloSafari_Auto", "HelloIOS_Auto"} {
		if !cap.Supports(hello) {
			t.Fatalf("capabilities do not list %s", hello)
		}
	}
	for kind, want := range map[fingerprint.Kind]int{
		fingerprint.KindChrome: 133, fingerprint.KindFirefox: 120,
		fingerprint.KindMacSafari: 16, fingerprint.KindIOSSafari: 14,
	} {
		if cap.MaxBrowser[kind] != want {
			t.Fatalf("%s cap = %d, want %d", kind, cap.MaxBrowser[kind], want)
		}
	}
}

func TestEnginePutsTheProfileOnTheWire(t *testing.T) {
	srv, rec := startTLSServer(t)
	defer srv.Close()

	seenALPN := map[fingerprint.Kind]string{}
	seenCiphers := map[fingerprint.Kind]string{}

	for _, kind := range fingerprint.AllKinds {
		p := profileFor(t, kind)
		transport, err := New(Options{Profile: p, Insecure: true, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatalf("%s: new transport: %v", kind, err)
		}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/probe", nil)
		req.Header.Set("User-Agent", p.UserAgent)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: request: %v", kind, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "ok") {
			t.Fatalf("%s: unexpected body %s", kind, body)
		}
		heights := rec.snapshot()
		last := heights[len(heights)-1]
		seenALPN[kind] = strings.Join(last.SupportedProtos, ",")
		parts := make([]string, 0, len(last.CipherSuites))
		for _, cs := range last.CipherSuites {
			parts = append(parts, fmt.Sprint(cs))
		}
		seenCiphers[kind] = strings.Join(parts, ",")
		transport.CloseIdleConnections()
	}

	for kind, alpn := range seenALPN {
		if !strings.Contains(alpn, "http/1.1") {
			t.Fatalf("%s: ALPN %q does not offer http/1.1", kind, alpn)
		}
	}
	if seenCiphers[fingerprint.KindChrome] == seenCiphers[fingerprint.KindFirefox] {
		t.Fatal("chrome and firefox produced the same cipher offer; the hello did not change")
	}
	if seenCiphers[fingerprint.KindMacSafari] == seenCiphers[fingerprint.KindChrome] {
		t.Fatal("mac_safari and chrome produced the same cipher offer")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	joined := strings.Join(rec.requests, " ")
	for _, kind := range fingerprint.AllKinds {
		p := profileFor(t, kind)
		if !strings.Contains(joined, p.UserAgent) {
			t.Fatalf("%s: user agent never reached the server", kind)
		}
	}
}

func TestEngineRefusesUnknownHello(t *testing.T) {
	p := profileFor(t, fingerprint.KindChrome)
	p.Engine = fingerprint.EngineHello{Name: "HelloNetscape_Auto", Minimum: "1.0"}
	if _, err := New(Options{Profile: p, Insecure: true}); err == nil {
		t.Fatal("an unknown hello must be refused instead of silently falling back")
	}
}

func TestEngineRefusesTemplateNewerThanItCanEmulate(t *testing.T) {
	p := profileFor(t, fingerprint.KindChrome)
	p.Major = 154
	p.BrowserVersion = "154"
	if _, err := New(Options{Profile: p, Insecure: true}); err == nil {
		t.Fatal("chrome 154 must be refused by a build capped at 133")
	}
}

func TestEngineVerifiesCertificatesByDefault(t *testing.T) {
	srv, _ := startTLSServer(t)
	defer srv.Close()

	p := profileFor(t, fingerprint.KindChrome)
	transport, err := New(Options{Profile: p, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("%v", err)
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	if _, err := client.Get(srv.URL + "/probe"); err == nil {
		t.Fatal("a self-signed certificate must fail with verification enabled")
	}
}

func TestEngineRefusesHTTPProxy(t *testing.T) {
	p := profileFor(t, fingerprint.KindChrome)
	transport, err := New(Options{Profile: p, ProxyURL: "http://127.0.0.1:1", Insecure: true})
	if err != nil {
		t.Fatalf("%v", err)
	}
	_, err = transport.dial(context.Background(), "tcp", "example.invalid:443")
	if err == nil || !strings.Contains(err.Error(), "socks5") {
		t.Fatalf("http proxies must be refused with a clear message, got %v", err)
	}
}

func TestDescribeReportsAppliedEngine(t *testing.T) {
	p := profileFor(t, fingerprint.KindFirefox)
	transport, err := New(Options{Profile: p, Insecure: true})
	if err != nil {
		t.Fatalf("%v", err)
	}
	d := transport.Describe()
	if d["applied"] != true || d["hello"] != "HelloFirefox_Auto" || d["http"] != "1.1" {
		t.Fatalf("describe = %+v", d)
	}
	if d["h2Disabled"] != true {
		t.Fatal("h2 must be reported as disabled, not implied")
	}
}
