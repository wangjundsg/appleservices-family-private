package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Laky-64/appleservices/internal/profile"
	"github.com/Laky-64/appleservices/internal/proxypool"
)

// The password every test template carries. Any output line containing it is a
// leak, so each test asserts on it rather than trusting the formatting.
const testPassword = "pw-secret-1234"

// fakeExit answers every request itself, the way a forward proxy does. The
// status can be changed between attempts, which is how the blocked-port test
// arranges one ban and one success on the same run.
type fakeExit struct {
	mu     sync.Mutex
	status int
	body   string
	seen   atomic.Int32
}

func (f *fakeExit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.seen.Add(1)
	f.mu.Lock()
	status, body := f.status, f.body
	f.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeExit) set(status int, body string) {
	f.mu.Lock()
	f.status, f.body = status, body
	f.mu.Unlock()
}

func (f *fakeExit) hits() int { return int(f.seen.Load()) }

// startExits brings up n fake exits on whatever loopback ports the kernel
// hands out, so a test never depends on a specific port being free.
func startExits(t *testing.T, n int) map[int]*fakeExit {
	t.Helper()
	exits := make(map[int]*fakeExit, n)
	for i := 0; i < n; i++ {
		exit := &fakeExit{status: http.StatusOK, body: "203.0.113.1"}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		server := httptest.NewUnstartedServer(exit)
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		exits[listener.Addr().(*net.TCPAddr).Port] = exit
	}
	return exits
}

// accountDir writes a frozen profile so the real (non-dry-run) paths have a
// file to bind into.
func accountDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	frozen, err := profile.Generate("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Save(dir, frozen); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(args, func(string) string { return "" }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func template(host string, port int) string {
	return fmt.Sprintf("http://acct__cr.us;sessttl.30:%s@%s", testPassword, net.JoinHostPort(host, strconv.Itoa(port)))
}

// TestDryRunTouchesNothing is the promise of -dry-run: it answers "which exit
// would this account get?" without a lease, without a socket and without a
// write, which is what makes it safe to run before an account directory exists.
func TestDryRunTouchesNothing(t *testing.T) {
	var probeHits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probeHits.Add(1)
	}))
	defer probe.Close()

	dir := filepath.Join(t.TempDir(), "acct-01")
	code, stdout, stderr := runCLI(t,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", 9),
		"-country", "US",
		"-preflight-url", probe.URL,
		"-dry-run",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if probeHits.Load() != 0 {
		t.Fatalf("dry run probed the network %d time(s)", probeHits.Load())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry run created %s", dir)
	}
	if !strings.Contains(stdout, "dry-run=true") || !strings.Contains(stdout, "ref=di:us:") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "redacted=http://acc***@127.0.0.1:") {
		t.Fatalf("stdout has no redacted endpoint: %q", stdout)
	}
	if strings.Contains(stdout+stderr, testPassword) {
		t.Fatalf("dry run printed the password: %q %q", stdout, stderr)
	}
}

// TestPreflightRetriesAfterForbidden is the retry contract end to end: the first
// lease answers 403, the run walks to the next port, binds the one that worked
// and records the address it measured.
func TestPreflightRetriesAfterForbidden(t *testing.T) {
	exits := startExits(t, 2)
	ports := make([]int, 0, len(exits))
	for port := range exits {
		ports = append(ports, port)
	}

	hostPort := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))
	base, err := proxypool.Parse(template("127.0.0.1", ports[0]))
	if err != nil {
		t.Fatal(err)
	}
	// The pool the command builds has to be the pool the test reasons about, or
	// the expectation below would be about a different account.
	pool := proxypool.Pool{Country: "US", Sticky: true, Ports: ports, Base: base}
	first, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.PickOffset("acct-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == second.Port {
		t.Fatalf("both picks landed on %d", first.Port)
	}
	exits[first.Port].set(http.StatusForbidden, "denied")
	exits[second.Port].set(http.StatusOK, "203.0.113.9")

	dir := accountDir(t)
	code, stdout, stderr := runCLI(t,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", ports[0]),
		"-country", "US",
		"-ports", strconv.Itoa(ports[0])+","+strconv.Itoa(ports[1]),
		"-preflight-url", "http://probe.test/ip",
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "notice=blocked") {
		t.Fatalf("no notice about the banned lease: %q", stderr)
	}
	if exits[first.Port].hits() != 1 {
		t.Fatalf("the banned lease was tried %d times, want once", exits[first.Port].hits())
	}
	for _, want := range []string{
		"ref=" + second.Ref(),
		"status=200",
		"ip=203.0.113.9",
		"attempts=2",
		"recorded=true",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout+stderr, testPassword) {
		t.Fatalf("run printed the password: %q %q", stdout, stderr)
	}

	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != second.Ref() {
		t.Fatalf("expected.egress_ref = %q, want %q", bound.Expected.EgressRef, second.Ref())
	}
	if bound.Actual.EgressIP != "203.0.113.9" {
		t.Fatalf("actual.egress_ip = %q", bound.Actual.EgressIP)
	}
	_ = hostPort
}

// TestBindWithoutPreflight: -preflight-url is what turns the probe on, so a run
// without it has to bind straight away, touch no socket, and leave Actual empty
// instead of inventing a measurement.
func TestBindWithoutPreflight(t *testing.T) {
	exits := startExits(t, 1)
	var only int
	for port := range exits {
		only = port
	}
	dir := accountDir(t)
	code, stdout, stderr := runCLI(t,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", only),
		"-country", "US",
		"-ports", strconv.Itoa(only),
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if exits[only].hits() != 0 {
		t.Fatalf("the run probed %d time(s) without -preflight-url", exits[only].hits())
	}
	if !strings.Contains(stdout, "status=0 ip= ") || !strings.Contains(stdout, "recorded=false") {
		t.Fatalf("stdout = %q", stdout)
	}
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != "di:us:"+strconv.Itoa(only) {
		t.Fatalf("expected.egress_ref = %q", bound.Expected.EgressRef)
	}
	if bound.Actual.EgressIP != "" {
		t.Fatalf("actual.egress_ip = %q, no probe ever ran", bound.Actual.EgressIP)
	}
}

// TestAllBlockedFailsStructured: when every lease is banned the run has to say
// so in a form a script can read, and still keep the credentials out of it.
func TestAllBlockedFailsStructured(t *testing.T) {
	exits := startExits(t, 1)
	var only int
	for port := range exits {
		only = port
	}
	exits[only].set(http.StatusForbidden, "denied")

	dir := accountDir(t)
	code, stdout, stderr := runCLI(t,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", only),
		"-country", "US",
		"-ports", strconv.Itoa(only),
		"-preflight-url", "http://probe.test/ip",
		"-account", "acct-01",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "error=blocked") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stdout+stderr, testPassword) {
		t.Fatalf("failure printed the password: %q %q", stdout, stderr)
	}
	// Nothing was bound: a failed run must not leave a route behind.
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != "" {
		t.Fatalf("a blocked run still bound %q", bound.Expected.EgressRef)
	}
}

// TestMissingProfileFailsBeforeProbing: a directory that cannot hold the
// answer is not worth spending a lease on.
func TestMissingProfileFailsBeforeProbing(t *testing.T) {
	var probeHits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probeHits.Add(1)
	}))
	defer probe.Close()

	code, _, stderr := runCLI(t,
		"-dir", t.TempDir(),
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", 9),
		"-preflight-url", probe.URL,
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "error=config") {
		t.Fatalf("stderr = %q", stderr)
	}
	if probeHits.Load() != 0 {
		t.Fatalf("a doomed run probed the network %d time(s)", probeHits.Load())
	}
}

// TestLocalProviderDefaultsToClash pins the third provider: with no template the
// command has to fall back to the machine's own proxy and say it is not sticky.
func TestLocalProviderDefaultsToClash(t *testing.T) {
	code, stdout, stderr := runCLI(t,
		"-dir", t.TempDir(),
		"-provider", "local",
		"-dry-run",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"ref=local:7897", "kind=local", "sticky=false", "redacted=http://127.0.0.1:7897"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// TestTemplateFromEnvironment: the template is usually a secret the shell keeps,
// so the environment variable has to work as well as the flag.
func TestTemplateFromEnvironment(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"-dir", t.TempDir(), "-dry-run"},
		func(key string) string {
			if key == "PROXY_TEMPLATE" {
				return template("127.0.0.1", 10000)
			}
			return ""
		},
		&stdout, &stderr,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ref=di:us:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), testPassword) {
		t.Fatal("the environment template leaked its password")
	}
}

// TestUsageErrors covers the configurations that must be refused before any
// lease or profile is touched.
func TestUsageErrors(t *testing.T) {
	sevenEleven := "socks5h://user1-zone-custom-region-US-session-sess-123:pw@global.rotgb.trustsource.live:10000"
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"missing dir", []string{"-provider", "local", "-dry-run"}},
		{"unknown provider", []string{"-dir", "x", "-provider", "whatever", "-dry-run"}},
		{"provider without template", []string{"-dir", "x", "-provider", "dataimpulse", "-dry-run"}},
		{"provider disagrees with template", []string{"-dir", "x", "-provider", "dataimpulse", "-url", sevenEleven, "-dry-run"}},
		{"local provider with a credentialed url", []string{"-dir", "x", "-provider", "local", "-url", template("127.0.0.1", 9), "-dry-run"}},
		{"unparseable template", []string{"-dir", "x", "-provider", "dataimpulse", "-url", "not a url", "-dry-run"}},
		{"bad port spec", []string{"-dir", "x", "-provider", "dataimpulse", "-url", template("127.0.0.1", 9), "-ports", "10002-10000", "-dry-run"}},
		{"stray argument", []string{"-dir", "x", "-provider", "local", "-dry-run", "extra"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, testCase.args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "error=usage") {
				t.Fatalf("stderr = %q", stderr)
			}
			if strings.Contains(stdout+stderr, testPassword) || strings.Contains(stdout+stderr, ":pw@") {
				t.Fatalf("usage error leaked the template credentials: %q %q", stdout, stderr)
			}
		})
	}
}
