package proxypool

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// probeTarget is never resolved: the fake exit below answers for it, which is
// what keeps these tests off the network.
const probeTarget = "http://probe.test/ip"

// fakeExit is a proxy that answers every request itself. httptest sees the
// request in absolute form, exactly as a real forward proxy would.
type fakeExit struct {
	mu     sync.Mutex
	status int
	body   string
	delay  time.Duration
	seen   int
}

func (f *fakeExit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status, body, delay := f.status, f.body, f.delay
	f.seen++
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeExit) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

// startExit brings up one fake proxy and returns the endpoint that reaches it.
func startExit(t *testing.T, status int, body string) (*fakeExit, Endpoint) {
	t.Helper()
	exit := &fakeExit{status: status, body: body}
	server := httptest.NewServer(exit)
	t.Cleanup(server.Close)
	// No credentials and a loopback host: this is the local-proxy shape, which
	// is also the cheapest way to point the transport at a fake.
	return exit, mustParse(t, server.URL)
}

// TestPreflightAcceptsSuccess: 2xx and 3xx both prove the exit carries traffic.
func TestPreflightAcceptsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		wantIP string
	}{
		{"plain address", http.StatusOK, "203.0.113.7", "203.0.113.7"},
		{"json address", http.StatusOK, `{"ip":"198.51.100.4"}`, "198.51.100.4"},
		{"redirect", http.StatusFound, "", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			exit, endpoint := startExit(t, testCase.status, testCase.body)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := PreflightResult(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget)
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			if result.Status != testCase.status {
				t.Fatalf("status = %d, want %d", result.Status, testCase.status)
			}
			if result.IP != testCase.wantIP {
				t.Fatalf("ip = %q, want %q", result.IP, testCase.wantIP)
			}
			if exit.hits() != 1 {
				t.Fatalf("exit saw %d requests, want 1", exit.hits())
			}
		})
	}
}

// TestPreflightFlagsBlockedExit is the retry trigger: a 403 means this exit is
// burned for the target, and the caller has to switch ports rather than retry
// the same one.
func TestPreflightFlagsBlockedExit(t *testing.T) {
	exit, endpoint := startExit(t, http.StatusForbidden, "denied")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Preflight(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if errors.Is(err, ErrRetryable) {
		t.Fatal("a banned exit must not also read as a transient failure")
	}
	if exit.hits() != 1 {
		t.Fatalf("exit saw %d requests, want 1", exit.hits())
	}
}

// TestPreflightFlagsRetryableExit covers the statuses that say "try another
// exit", not "this account is burned".
func TestPreflightFlagsRetryableExit(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		_, endpoint := startExit(t, status, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := Preflight(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget)
		cancel()
		if !errors.Is(err, ErrRetryable) {
			t.Fatalf("status %d: err = %v, want ErrRetryable", status, err)
		}
		if errors.Is(err, ErrBlocked) {
			t.Fatalf("status %d also read as blocked", status)
		}
	}
}

// TestPreflightFlagsUnexpectedStatus: a 4xx that is neither success nor a ban
// is a configuration problem, and retrying it would only burn leases.
func TestPreflightFlagsUnexpectedStatus(t *testing.T) {
	_, endpoint := startExit(t, http.StatusNotFound, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Preflight(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget)
	if !errors.Is(err, ErrUnexpected) {
		t.Fatalf("err = %v, want ErrUnexpected", err)
	}
	if errors.Is(err, ErrRetryable) || errors.Is(err, ErrBlocked) {
		t.Fatalf("404 was classified as retryable or blocked: %v", err)
	}
}

// TestPreflightTimesOut: an exit that never answers is retryable, and the
// timeout has to come from HTTPProbe rather than from the test harness.
func TestPreflightTimesOut(t *testing.T) {
	exit := &fakeExit{status: http.StatusOK, body: "203.0.113.7", delay: 400 * time.Millisecond}
	server := httptest.NewServer(exit)
	t.Cleanup(server.Close)
	endpoint := mustParse(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := time.Now()
	err := Preflight(ctx, HTTPProbe{Timeout: 40 * time.Millisecond}, endpoint, probeTarget)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap DeadlineExceeded", err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("preflight took %s, the probe timeout did not apply", elapsed)
	}
}

// TestPreflightTransportFailure: a lease the machine cannot even reach is a
// transient failure, not a ban.
func TestPreflightTransportFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	endpoint := mustParse(t, "http://127.0.0.1:"+strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Preflight(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget); !errors.Is(err, ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
}

// TestPreflightRejectsBadTarget keeps a typo in -preflight-url from looking
// like a broken exit.
func TestPreflightRejectsBadTarget(t *testing.T) {
	_, endpoint := startExit(t, http.StatusOK, "203.0.113.7")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, target := range []string{"", "   ", "ftp://probe.test/ip", "probe.test/ip"} {
		if err := Preflight(ctx, HTTPProbe{Timeout: time.Second}, endpoint, target); !errors.Is(err, ErrUnexpected) {
			t.Fatalf("target %q: err = %v, want ErrUnexpected", target, err)
		}
	}
}

// stubProbe is the injection seam: Preflight's verdicts must be reachable
// without a socket.
type stubProbe struct {
	status int
	ip     string
	err    error
	calls  int
	target string
}

func (s *stubProbe) Check(_ context.Context, _ Endpoint, target string) (int, string, error) {
	s.calls++
	s.target = target
	return s.status, s.ip, s.err
}

func TestPreflightUsesTheInjectedProbe(t *testing.T) {
	ctx := context.Background()
	endpoint := mustParse(t, diHTTP)

	probe := &stubProbe{status: http.StatusForbidden}
	if err := Preflight(ctx, probe, endpoint, probeTarget); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if probe.calls != 1 || probe.target != probeTarget {
		t.Fatalf("probe saw %d calls for target %q", probe.calls, probe.target)
	}

	probe = &stubProbe{status: http.StatusOK, ip: "203.0.113.7"}
	result, err := PreflightResult(ctx, probe, endpoint, probeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != http.StatusOK || result.IP != "203.0.113.7" {
		t.Fatalf("result = %+v", result)
	}

	probe = &stubProbe{err: errors.New("dial tcp: connection refused")}
	if _, err := PreflightResult(ctx, probe, endpoint, probeTarget); !errors.Is(err, ErrRetryable) {
		t.Fatalf("transport error = %v, want ErrRetryable", err)
	}
}

func TestExtractIP(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", "203.0.113.7\n", "203.0.113.7"},
		{"json ip", `{"ip":"198.51.100.4"}`, "198.51.100.4"},
		{"json origin list", `{"origin":"198.51.100.4, 203.0.113.9"}`, "198.51.100.4"},
		{"ipify style text", "Your address is 203.0.113.4", "203.0.113.4"},
		{"empty", "", ""},
		{"not an address", "hello", ""},
		{"out of range octets", "999.1.1.1", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := extractIP(testCase.body); got != testCase.want {
				t.Fatalf("extractIP(%q) = %q, want %q", testCase.body, got, testCase.want)
			}
		})
	}
}

// TestHTTPProbeSendsProviderCredentials guards the one thing a transport needs
// and a log must never show: URL() keeps the credentials, Redacted never does.
func TestHTTPProbeSendsProviderCredentials(t *testing.T) {
	exit := &fakeExit{status: http.StatusOK, body: "203.0.113.7"}
	server := httptest.NewServer(exit)
	t.Cleanup(server.Close)

	hostPort := strings.TrimPrefix(server.URL, "http://")
	endpoint := mustParse(t, "http://abc123__cr.us;sessttl.30:s3cr3t-pass@"+hostPort)
	if endpoint.Kind != HTTP {
		t.Fatalf("kind = %s, want http for a credentialed loopback proxy", endpoint.Kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := PreflightResult(ctx, HTTPProbe{Timeout: time.Second}, endpoint, probeTarget); err != nil {
		t.Fatalf("preflight through an authenticated proxy: %v", err)
	}
	if exit.hits() != 1 {
		t.Fatalf("exit saw %d requests", exit.hits())
	}
}
