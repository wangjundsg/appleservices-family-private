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

// The credentials every test template carries. Any output line containing either
// of them is a leak, so the tests assert on both rather than trusting the
// formatting.
const (
	testPassword = "pw-secret-1234"
	testUser     = "acct__cr.us;sessttl.30"
)

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

// leaseExit is one fake exit on a DataImpulse lease port.
type leaseExit struct {
	port int
	exit *fakeExit
}

// startLeaseExits brings up n fake exits on ports inside 10000..20000. The
// endpoints this command builds are validated against §2's lease range, so a
// fake on an ephemeral port would be rejected before any probe ran; the
// listeners still use whatever free lease ports this machine has.
func startLeaseExits(t *testing.T, n int) []leaseExit {
	t.Helper()
	exits := make([]leaseExit, 0, n)
	for port := proxypool.DataImpulseLeaseStart; port < proxypool.DataImpulseLeaseStart+64 && len(exits) < n; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		exit := &fakeExit{status: http.StatusOK, body: "203.0.113.1"}
		server := httptest.NewUnstartedServer(exit)
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		exits = append(exits, leaseExit{port: port, exit: exit})
	}
	if len(exits) < n {
		t.Skipf("only %d free lease port(s) on this machine, want %d", len(exits), n)
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

// runCLI drives the command with an isolated port table, so no test can read or
// write the working directory's cache.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWithState(t, filepath.Join(t.TempDir(), "state"), args...)
}

func runCLIWithState(t *testing.T, stateDir string, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"-state", stateDir}, args...)
	var stdout, stderr strings.Builder
	code := run(full, func(string) string { return "" }, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// template is the shape an operator pastes in: the DataImpulse login (with its
// ';') and a password, on the lease port the fake exit listens on.
func template(host string, port int) string {
	return fmt.Sprintf("http://%s:%s@%s", testUser, testPassword, net.JoinHostPort(host, strconv.Itoa(port)))
}

// assertNoCredentials is contract §7: no log line carries the password or the
// login, whatever the run did.
func assertNoCredentials(t *testing.T, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		for _, secret := range []string{testPassword, testUser} {
			if strings.Contains(out, secret) {
				t.Fatalf("output carries the credential %q: %q", secret, out)
			}
		}
	}
}

// TestDryRunTouchesNothing is the promise of -dry-run: it answers "which exit
// would this account get?" without a lease, a lock, a socket or a write, which
// is what makes it safe to run before an account directory exists.
func TestDryRunTouchesNothing(t *testing.T) {
	var probeHits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probeHits.Add(1)
	}))
	defer probe.Close()

	dir := filepath.Join(t.TempDir(), "acct-01")
	stateDir := filepath.Join(t.TempDir(), "state")
	leasePort := proxypool.DataImpulseLeaseStart
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", leasePort),
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
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("dry run created the port table directory %s", stateDir)
	}
	for _, want := range []string{
		"dry-run=true",
		"ref=di:us:" + strconv.Itoa(leasePort),
		"lease_port=" + strconv.Itoa(leasePort),
		"leased=false",
		"redacted=http://acc***@127.0.0.1:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	assertNoCredentials(t, stdout, stderr)
}

// TestPreflightRetriesAfterForbidden is the retry contract end to end: the first
// lease answers 403, the run releases that port, walks to the next candidate,
// binds the one that worked and records the address it measured.
func TestPreflightRetriesAfterForbidden(t *testing.T) {
	exits := startLeaseExits(t, 2)
	first, second := exits[0], exits[1]
	first.exit.set(http.StatusForbidden, "denied")
	second.exit.set(http.StatusOK, "203.0.113.9")

	stateDir := filepath.Join(t.TempDir(), "state")
	dir := accountDir(t)
	ports := strconv.Itoa(first.port) + "," + strconv.Itoa(second.port)
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", first.port),
		"-country", "US",
		"-ports", ports,
		"-preflight-url", "http://probe.test/ip",
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "notice=blocked") {
		t.Fatalf("no notice about the banned lease: %q", stderr)
	}
	if first.exit.hits() != 1 {
		t.Fatalf("the banned lease was tried %d times, want once", first.exit.hits())
	}
	if second.exit.hits() != 1 {
		t.Fatalf("the second lease was tried %d times, want once", second.exit.hits())
	}
	wantRef := "di:us:" + strconv.Itoa(second.port)
	for _, want := range []string{
		"ref=" + wantRef,
		"status=200",
		"ip=203.0.113.9",
		"attempts=2",
		"lease_port=" + strconv.Itoa(second.port),
		"recorded=true",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	assertNoCredentials(t, stdout, stderr)

	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != wantRef {
		t.Fatalf("expected.egress_ref = %q, want %q", bound.Expected.EgressRef, wantRef)
	}
	if bound.Actual.EgressIP != "203.0.113.9" {
		t.Fatalf("actual.egress_ip = %q", bound.Actual.EgressIP)
	}
	// The banned port went back into the table: another account may take it.
	if port, leased := proxypool.Peek(stateDir, "acct-02", []int{first.port, second.port}); port != first.port || leased {
		t.Fatalf("the banned port was not released: Peek = %d, leased = %t", port, leased)
	}
}

// TestRetryChangesThePortNeverTheRoute is contract §7 with a stopwatch on it: the
// retry walks to another port, and every attempt leaves through a proxy. A
// direct connection would reach the probe target, which counts its own hits.
func TestRetryChangesThePortNeverTheRoute(t *testing.T) {
	exits := startLeaseExits(t, 2)
	for _, e := range exits {
		e.exit.set(http.StatusForbidden, "denied")
	}

	var directHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		_, _ = io.WriteString(w, "203.0.113.1")
	}))
	defer target.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", accountDir(t),
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", exits[0].port),
		"-ports", strconv.Itoa(exits[0].port)+","+strconv.Itoa(exits[1].port),
		"-preflight-url", target.URL+"/ip",
		"-account", "acct-01",
		"-attempts", "2",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "error=blocked") {
		t.Fatalf("stderr = %q", stderr)
	}
	total := exits[0].exit.hits() + exits[1].exit.hits()
	if total != 2 {
		t.Fatalf("the two attempts made %d proxied request(s), want 2", total)
	}
	if directHits.Load() != 0 {
		t.Fatalf("the run reached the target directly %d time(s): a retry may change the port, never the route", directHits.Load())
	}
	assertNoCredentials(t, stdout, stderr)
}

// TestBindWithoutPreflight: -preflight-url is what turns the probe on, so a run
// without it has to bind straight away, touch no socket, and leave Actual empty
// instead of inventing a measurement.
func TestBindWithoutPreflight(t *testing.T) {
	exits := startLeaseExits(t, 1)
	only := exits[0]
	dir := accountDir(t)
	code, stdout, stderr := runCLI(t,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", only.port),
		"-country", "US",
		"-ports", strconv.Itoa(only.port),
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if only.exit.hits() != 0 {
		t.Fatalf("the run probed %d time(s) without -preflight-url", only.exit.hits())
	}
	for _, want := range []string{"status=0 ip= ", "recorded=false", "lease_port=" + strconv.Itoa(only.port)} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != "di:us:"+strconv.Itoa(only.port) {
		t.Fatalf("expected.egress_ref = %q", bound.Expected.EgressRef)
	}
	if bound.Actual.EgressIP != "" {
		t.Fatalf("actual.egress_ip = %q, no probe ever ran", bound.Actual.EgressIP)
	}
	assertNoCredentials(t, stdout, stderr)
}

// TestAllBlockedFailsStructured: when every lease is banned the run has to say so
// in a form a script can read, keep the credentials out of it, and leave no route
// and no lease behind.
func TestAllBlockedFailsStructured(t *testing.T) {
	exits := startLeaseExits(t, 1)
	only := exits[0]
	only.exit.set(http.StatusForbidden, "denied")

	stateDir := filepath.Join(t.TempDir(), "state")
	dir := accountDir(t)
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", only.port),
		"-country", "US",
		"-ports", strconv.Itoa(only.port),
		"-preflight-url", "http://probe.test/ip",
		"-account", "acct-01",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "error=blocked") {
		t.Fatalf("stderr = %q", stderr)
	}
	assertNoCredentials(t, stdout, stderr)
	// Nothing was bound: a failed run must not leave a route behind.
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != "" {
		t.Fatalf("a blocked run still bound %q", bound.Expected.EgressRef)
	}
	// ... and nothing was leased either: the burned port is free again.
	if port, leased := proxypool.Peek(stateDir, "acct-02", []int{only.port}); port != only.port || leased {
		t.Fatalf("the burned port is still leased: Peek = %d, leased = %t", port, leased)
	}
}

// TestMissingProfilePrintsUnboundRef is run-20261113 §4.1: the documented
// pipeline order selects the exit before profilegen has ever run, so a missing
// profile.json must not kill the run. The pick is printed with bound=false, the
// hint says why, and no lease is spent on a probe that has nowhere to land.
func TestMissingProfilePrintsUnboundRef(t *testing.T) {
	var probeHits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probeHits.Add(1)
	}))
	defer probe.Close()

	dir := filepath.Join(t.TempDir(), "acct-01")
	stateDir := filepath.Join(t.TempDir(), "state")
	leasePort := proxypool.DataImpulseLeaseStart
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", leasePort),
		"-country", "US",
		"-preflight-url", probe.URL,
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitOK, stderr)
	}
	for _, want := range []string{"ref=di:us:", "bound=false", "lease_port=" + strconv.Itoa(leasePort)} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if !strings.Contains(stderr, "notice=unbound") || !strings.Contains(stderr, "未绑定（缺少 profile.json）") {
		t.Fatalf("stderr = %q", stderr)
	}
	if probeHits.Load() != 0 {
		t.Fatalf("an unbound run probed the network %d time(s)", probeHits.Load())
	}
	assertNoCredentials(t, stdout, stderr)
	// No profile was written by this run: the pick is reported, not recorded.
	if _, err := os.Stat(profile.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("unbound run created %s", profile.Path(dir))
	}
	// The pick is still a lease: the port the account will be bound to is held.
	if port, leased := proxypool.Peek(stateDir, "acct-01", []int{leasePort}); port != leasePort || !leased {
		t.Fatalf("the unbound pick was not leased: Peek = %d, leased = %t", port, leased)
	}
}

// TestUnavailableIsReportedNotRetried is run-20261113 §4.5: a 503 from the
// target's edge is not a 403 ban, so walking to the next port would burn a lease
// for nothing. The run reports status=unavailable and stops after one probe.
func TestUnavailableIsReportedNotRetried(t *testing.T) {
	exits := startLeaseExits(t, 2)
	for _, e := range exits {
		e.exit.set(http.StatusServiceUnavailable, "Server: Apple")
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	dir := accountDir(t)
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", exits[0].port),
		"-country", "US",
		"-ports", strconv.Itoa(exits[0].port)+","+strconv.Itoa(exits[1].port),
		"-preflight-url", "http://probe.test/ip",
		"-account", "acct-01",
	)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d (stderr = %q)", code, exitFailure, stderr)
	}
	if !strings.Contains(stderr, "error=unavailable") {
		t.Fatalf("stderr = %q", stderr)
	}
	total := exits[0].exit.hits() + exits[1].exit.hits()
	if total != 1 {
		t.Fatalf("the run probed %d time(s), want exactly one on an edge refusal", total)
	}
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Expected.EgressRef != "" {
		t.Fatalf("a 503 run still bound %q", bound.Expected.EgressRef)
	}
	if port, leased := proxypool.Peek(stateDir, "acct-02", []int{exits[0].port}); port != exits[0].port || leased {
		t.Fatalf("the refused port is still leased: Peek = %d, leased = %t", port, leased)
	}
	assertNoCredentials(t, stdout, stderr)
}

// TestAccountKeepsItsPortAcrossRuns is §4's stickiness at the command level: two
// runs of one account report the same lease port, a second account gets another.
func TestAccountKeepsItsPortAcrossRuns(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	account := accountDir(t)
	args := func(accountID string) []string {
		return []string{
			"-dir", account,
			"-provider", "dataimpulse",
			"-url", template("127.0.0.1", proxypool.DataImpulseLeaseStart),
			"-country", "US",
			"-account", accountID,
		}
	}
	firstPort := ""
	for run := 0; run < 2; run++ {
		code, stdout, stderr := runCLIWithState(t, stateDir, args("acct-01")...)
		if code != exitOK {
			t.Fatalf("run %d: exit = %d, stderr = %q", run, code, stderr)
		}
		want := "lease_port=" + strconv.Itoa(proxypool.DataImpulseLeaseStart)
		if !strings.Contains(stdout, want) {
			t.Fatalf("run %d: stdout = %q, want %q", run, stdout, want)
		}
		if run == 0 {
			firstPort = want
		} else if !strings.Contains(stdout, firstPort) {
			t.Fatalf("the account moved off its port: %q", stdout)
		}
	}
	code, stdout, stderr := runCLIWithState(t, stateDir, args("acct-02")...)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "lease_port="+strconv.Itoa(proxypool.DataImpulseLeaseStart+1)) {
		t.Fatalf("the second account did not get the next port: %q", stdout)
	}
}

// TestReleaseFlagIsTheWayOut is the manual half of the lease: -release works
// without an account directory, frees the port for the next account and is
// idempotent.
func TestReleaseFlagIsTheWayOut(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	dir := accountDir(t)
	leasePort := proxypool.DataImpulseLeaseStart
	code, stdout, stderr := runCLIWithState(t, stateDir,
		"-dir", dir,
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", leasePort),
		"-country", "US",
		"-account", "acct-01",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "lease_port="+strconv.Itoa(leasePort)) {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, stderr = runCLIWithState(t, stateDir, "-release", strconv.Itoa(leasePort))
	if code != exitOK {
		t.Fatalf("release exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "released="+strconv.Itoa(leasePort)) {
		t.Fatalf("stdout = %q", stdout)
	}
	if port, leased := proxypool.Peek(stateDir, "acct-02", []int{leasePort}); port != leasePort || leased {
		t.Fatalf("the released port is still leased: Peek = %d, leased = %t", port, leased)
	}

	// Releasing again is not an error: the flag doubles as cleanup.
	if code, _, stderr := runCLIWithState(t, stateDir, "-release", strconv.Itoa(leasePort)); code != exitOK {
		t.Fatalf("second release exit = %d, stderr = %q", code, stderr)
	}
	// A port outside 1..65535 is a typo, not a lease.
	if code, _, stderr := runCLIWithState(t, stateDir, "-release", "70000"); code != exitFailure || !strings.Contains(stderr, "error=release") {
		t.Fatalf("release 70000: exit = %d, stderr = %q", code, stderr)
	}
}

// TestLogsStayRedacted is the §7 assertion the command owes its operators: a
// successful run prints the ref and the masked endpoint, and neither of those
// carries the password or the login.
func TestLogsStayRedacted(t *testing.T) {
	exits := startLeaseExits(t, 1)
	only := exits[0]
	code, stdout, stderr := runCLI(t,
		"-dir", accountDir(t),
		"-provider", "dataimpulse",
		"-url", template("127.0.0.1", only.port),
		"-ports", strconv.Itoa(only.port),
		"-account", "acct-01",
		"-preflight-url", "http://probe.test/ip",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "redacted=http://acc***@127.0.0.1:"+strconv.Itoa(only.port)) {
		t.Fatalf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, testUser) || strings.Contains(stderr, testUser) {
		t.Fatalf("the login reached a log line: %q %q", stdout, stderr)
	}
	assertNoCredentials(t, stdout, stderr)
}

// TestLocalProviderDefaultsToClash pins the third provider: with no template the
// command has to fall back to the machine's own proxy, say it is not sticky, and
// allocate nothing.
func TestLocalProviderDefaultsToClash(t *testing.T) {
	code, stdout, stderr := runCLI(t,
		"-dir", t.TempDir(),
		"-provider", "local",
		"-dry-run",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"ref=local:7897", "kind=local", "sticky=false", "lease_port=0", "redacted=http://127.0.0.1:7897"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// Test711KeepsItsSession is §4 for the other provider: 711's candidate is the
// session inside the username, so no lease port is allocated and the template's
// session survives untouched.
func Test711KeepsItsSession(t *testing.T) {
	const template711 = "socks5h://user1-zone-custom-region-US-session-12345678-sessTime-30-sessAuto-0:pw-711@global.rotgb.trustsource.live:10000"
	code, stdout, stderr := runCLI(t,
		"-dir", t.TempDir(),
		"-provider", "711",
		"-url", template711,
		"-country", "GB",
		"-dry-run",
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"ref=711:gb:12345678", "lease_port=0", "redacted=socks5h://use***@global.rotgb.trustsource.live:10000"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout+stderr, "pw-711") {
		t.Fatalf("the 711 password reached a log line: %q %q", stdout, stderr)
	}
}

// TestTemplateFromEnvironment: the template is usually a secret the shell keeps,
// so the environment variable has to work as well as the flag.
func TestTemplateFromEnvironment(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"-dir", t.TempDir(), "-state", filepath.Join(t.TempDir(), "state"), "-dry-run"},
		func(key string) string {
			if key == "PROXY_TEMPLATE" {
				return template("127.0.0.1", proxypool.DataImpulseLeaseStart)
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
	assertNoCredentials(t, stdout.String(), stderr.String())
}

// TestUsageErrors covers the configurations that must be refused before a lease
// or a profile is touched, including the §2 shapes Parse now enforces.
func TestUsageErrors(t *testing.T) {
	sevenEleven := "socks5h://user1-zone-custom-region-US-session-12345678:pw@global.rotgb.trustsource.live:10000"
	rotation := "http://acct__cr.us;sessttl.30:" + testPassword + "@127.0.0.1:8000"
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"missing dir", []string{"-provider", "local", "-dry-run"}},
		{"unknown provider", []string{"-dir", "x", "-provider", "whatever", "-dry-run"}},
		{"provider without template", []string{"-dir", "x", "-provider", "dataimpulse", "-dry-run"}},
		{"provider disagrees with template", []string{"-dir", "x", "-provider", "dataimpulse", "-url", sevenEleven, "-dry-run"}},
		{"local provider with a credentialed url", []string{"-dir", "x", "-provider", "local", "-url", template("127.0.0.1", proxypool.DataImpulseLeaseStart), "-dry-run"}},
		{"unparseable template", []string{"-dir", "x", "-provider", "dataimpulse", "-url", "not a url", "-dry-run"}},
		{"bad port spec", []string{"-dir", "x", "-provider", "dataimpulse", "-url", template("127.0.0.1", proxypool.DataImpulseLeaseStart), "-ports", "10002-10000", "-dry-run"}},
		{"stray argument", []string{"-dir", "x", "-provider", "local", "-dry-run", "extra"}},
		// §2: a rotation port is not a lease, and a 711 session has to be eight
		// digits. Both are configuration errors, not something to "repair".
		{"dataimpulse rotation port", []string{"-dir", "x", "-provider", "dataimpulse", "-url", rotation, "-dry-run"}},
		{"711 session too short", []string{"-dir", "x", "-provider", "711", "-url", "socks5h://u-zone-custom-region-US-session-1234567:pw@global.rotgb.trustsource.live:10000", "-dry-run"}},
		{"711 lower case region", []string{"-dir", "x", "-provider", "711", "-url", "socks5h://u-zone-custom-region-us-session-12345678:pw@global.rotgb.trustsource.live:10000", "-dry-run"}},
		{"zero ttl", []string{"-dir", "x", "-provider", "dataimpulse", "-url", template("127.0.0.1", proxypool.DataImpulseLeaseStart), "-ttl", "0s", "-dry-run"}},
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
			assertNoCredentials(t, stdout, stderr)
			if strings.Contains(stdout+stderr, ":pw@") || strings.Contains(stdout+stderr, "pw-711") {
				t.Fatalf("usage error leaked the template credentials: %q %q", stdout, stderr)
			}
		})
	}
}
