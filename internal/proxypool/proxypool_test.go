package proxypool

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The three shapes the pipeline really pastes in. Keeping them as constants
// means every test below reads the same endpoints the operators use, not a
// simplified stand-in.
const (
	diHTTP   = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:10012"
	diSOCKS  = "socks5h://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:10012"
	diRotate = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:8000"
	trust711 = "socks5h://user1-zone-custom-region-US-session-sess-123:pw-711@global.rotgb.trustsource.live:10000"
	clash    = "http://127.0.0.1:7897"
)

func mustParse(t *testing.T, raw string) Endpoint {
	t.Helper()
	endpoint, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return endpoint
}

// TestParseDataImpulse pins the residential format: the login suffix carries
// the country, the port carries the lease, and both have to survive parsing
// because Ref is built out of them.
func TestParseDataImpulse(t *testing.T) {
	for _, raw := range []string{diHTTP, diSOCKS} {
		endpoint := mustParse(t, raw)
		if endpoint.Host != "gw.dataimpulse.com" {
			t.Fatalf("%s: host = %q", raw, endpoint.Host)
		}
		if endpoint.Port != 10012 {
			t.Fatalf("%s: port = %d", raw, endpoint.Port)
		}
		if endpoint.User != "abc123__cr.us;sessttl.30" {
			t.Fatalf("%s: user = %q, the provider suffix must not be stripped", raw, endpoint.User)
		}
		if endpoint.Pass != "s3cr3t-pass" {
			t.Fatalf("%s: password not kept for transport", raw)
		}
		if !endpoint.Sticky {
			t.Fatalf("%s: lease port 10012 pins the exit, Sticky = false", raw)
		}
		if endpoint.Country != "US" {
			t.Fatalf("%s: country = %q", raw, endpoint.Country)
		}
		if ref := endpoint.Ref(); ref != "di:us:10012" {
			t.Fatalf("%s: ref = %q, want di:us:10012", raw, ref)
		}
		if endpoint.Provider() != providerDataImpulse {
			t.Fatalf("%s: provider = %q", raw, endpoint.Provider())
		}
	}
	if mustParse(t, diHTTP).Kind != HTTP {
		t.Fatal("http:// DataImpulse endpoint is not HTTP kind")
	}
	if mustParse(t, diSOCKS).Kind != SOCKS5H {
		t.Fatal("socks5h:// DataImpulse endpoint is not SOCKS5H kind")
	}
}

// TestParseDataImpulseOutsideLease keeps "sticky" honest: a port outside the
// lease range is a rotating exit, and saying otherwise would let the pipeline
// believe an account is pinned when it is not.
func TestParseDataImpulseOutsideLease(t *testing.T) {
	endpoint := mustParse(t, diRotate)
	if endpoint.Sticky {
		t.Fatalf("port %d is not a sticky lease", endpoint.Port)
	}
	if !strings.HasPrefix(endpoint.Ref(), "di:us:") {
		t.Fatalf("ref = %q", endpoint.Ref())
	}
}

// TestParse711 pins the second provider: the country lives in -region- and the
// sticky identity in -session-, so Ref must name the session rather than the
// port every 711 customer shares.
func TestParse711(t *testing.T) {
	endpoint := mustParse(t, trust711)
	if endpoint.Kind != SOCKS5H {
		t.Fatalf("kind = %s", endpoint.Kind)
	}
	if endpoint.Host != "global.rotgb.trustsource.live" || endpoint.Port != 10000 {
		t.Fatalf("host:port = %s:%d", endpoint.Host, endpoint.Port)
	}
	if endpoint.User != "user1-zone-custom-region-US-session-sess-123" {
		t.Fatalf("user = %q", endpoint.User)
	}
	if endpoint.Country != "US" {
		t.Fatalf("country = %q", endpoint.Country)
	}
	if !endpoint.Sticky {
		t.Fatal("-session- pins the exit, Sticky = false")
	}
	if ref := endpoint.Ref(); ref != "711:us:sess-123" {
		t.Fatalf("ref = %q, want 711:us:sess-123", ref)
	}
	if endpoint.Provider() != provider711 {
		t.Fatalf("provider = %q", endpoint.Provider())
	}
}

// TestParseLocal covers the Clash case: a bare loopback port with no
// credentials and no fixed exit.
func TestParseLocal(t *testing.T) {
	endpoint := mustParse(t, clash)
	if endpoint.Kind != LOCAL {
		t.Fatalf("kind = %s", endpoint.Kind)
	}
	if endpoint.User != "" || endpoint.Pass != "" {
		t.Fatalf("local endpoint gained credentials: %q / %q", endpoint.User, endpoint.Pass)
	}
	if endpoint.Sticky {
		t.Fatal("a local proxy holds no fixed exit")
	}
	if endpoint.Country != "" {
		t.Fatalf("country = %q", endpoint.Country)
	}
	if ref := endpoint.Ref(); ref != "local:7897" {
		t.Fatalf("ref = %q, want local:7897", ref)
	}
}

// TestParseRejects is the set of inputs that must not become an endpoint: each
// one would otherwise reach a transport with a field missing.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"missing port", "http://gw.dataimpulse.com", ErrNoPort},
		{"missing credentials", "http://gw.dataimpulse.com:10012", ErrNoAuth},
		{"missing password", "http://abc123__cr.us@gw.dataimpulse.com:10012", ErrNoAuth},
		{"unknown scheme", "ftp://abc:def@gw.dataimpulse.com:10012", ErrUnknownScheme},
		// socks5 without the h resolves names locally, i.e. outside the tunnel.
		{"socks5 without remote dns", "socks5://abc:def@gw.dataimpulse.com:1080", ErrUnknownScheme},
		{"port out of range", "http://abc:def@gw.dataimpulse.com:99999", ErrBadPort},
		{"non numeric port", "http://abc:def@gw.dataimpulse.com:abc", ErrInvalid},
		{"missing host", "http://:10012", ErrInvalid},
		{"empty", "   ", ErrInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint, err := Parse(testCase.raw)
			if err == nil {
				t.Fatalf("%q parsed as %+v", testCase.raw, endpoint)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("%q: err = %v, want %v", testCase.raw, err, testCase.want)
			}
			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrUnknownScheme) {
				return
			}
			// A parse failure is allowed in a log, so it must not quote the
			// credential-bearing URL back out.
			if strings.Contains(err.Error(), "def@") || strings.Contains(err.Error(), ":def") {
				t.Fatalf("%q: error leaks credentials: %v", testCase.raw, err)
			}
		})
	}
}

// TestRefAndRedactedWithholdCredentials is the rule the profile depends on:
// whatever an endpoint looks like, the two renderings that leave the process
// carry no password and no usable username.
func TestRefAndRedactedWithholdCredentials(t *testing.T) {
	for _, raw := range []string{diHTTP, diSOCKS, diRotate, trust711, clash} {
		endpoint := mustParse(t, raw)
		ref := endpoint.Ref()
		if strings.Contains(ref, "s3cr3t") || strings.Contains(ref, "pw-711") {
			t.Fatalf("%s: ref carries the password: %q", raw, ref)
		}
		if endpoint.User != "" && strings.Contains(ref, endpoint.User) {
			t.Fatalf("%s: ref carries the full username: %q", raw, ref)
		}
		redacted := endpoint.Redacted()
		if strings.Contains(redacted, "s3cr3t") || strings.Contains(redacted, "pw-711") {
			t.Fatalf("%s: redacted carries the password: %q", raw, redacted)
		}
		if endpoint.User != "" && strings.Contains(redacted, endpoint.User) {
			t.Fatalf("%s: redacted echoes the whole username: %q", raw, redacted)
		}
		if !strings.Contains(redacted, strconv.Itoa(endpoint.Port)) {
			t.Fatalf("%s: redacted lost the port: %q", raw, redacted)
		}
	}
	if got := mustParse(t, diHTTP).Redacted(); got != "http://abc***@gw.dataimpulse.com:10012" {
		t.Fatalf("redacted = %q", got)
	}
	if got := mustParse(t, clash).Redacted(); got != "http://127.0.0.1:7897" {
		t.Fatalf("local redacted = %q", got)
	}
}

// TestURLRoundTrips keeps URL() a faithful inverse of Parse: it is what the
// transport dials, so a lost suffix would silently change the exit.
func TestURLRoundTrips(t *testing.T) {
	for _, raw := range []string{diHTTP, diSOCKS, trust711, clash} {
		first := mustParse(t, raw)
		assembled := first.URL()
		if first.Pass != "" && !strings.Contains(assembled, first.Pass) {
			t.Fatalf("%s: URL() lost the password a transport needs: %q", raw, assembled)
		}
		second, err := Parse(assembled)
		if err != nil {
			t.Fatalf("%s: re-parse %q: %v", raw, assembled, err)
		}
		if first.Kind != second.Kind || first.Host != second.Host || first.Port != second.Port ||
			first.User != second.User || first.Pass != second.Pass || first.Country != second.Country {
			t.Fatalf("%s: round trip changed the endpoint:\n%+v\n%+v", raw, first, second)
		}
	}
}

// TestPickIsStable is the correctness property the whole package exists for:
// one account, one lease, on every run.
func TestPickIsStable(t *testing.T) {
	pool := Pool{Country: "US", Sticky: true, Start: 10000, End: 10020, Base: mustParse(t, diHTTP)}
	first, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref() != second.Ref() || first.URL() != second.URL() {
		t.Fatalf("same account picked two leases: %s then %s", first.Ref(), second.Ref())
	}
	if first.Port < 10000 || first.Port > 10020 {
		t.Fatalf("port %d is outside the pool", first.Port)
	}
	if !first.Sticky {
		t.Fatal("a sticky pool returned a non-sticky endpoint")
	}
	if first.Raw != "" {
		t.Fatal("a picked endpoint still claims the template's raw url")
	}
	if !strings.Contains(first.User, "__cr.us") {
		t.Fatalf("pick lost the provider suffix: %q", first.User)
	}
}

// TestPickSpreadsAccounts is the other half of the property: two accounts must
// not quietly share one exit, or Apple sees one machine creating two accounts.
func TestPickSpreadsAccounts(t *testing.T) {
	pool := Pool{Country: "US", Sticky: true, Start: 10000, End: 10020, Base: mustParse(t, diHTTP)}
	seen := map[int]string{}
	for i := 1; i <= 12; i++ {
		account := "acct-" + strconv.Itoa(i)
		endpoint, err := pool.Pick(account)
		if err != nil {
			t.Fatal(err)
		}
		if other, clash := seen[endpoint.Port]; clash {
			if account == other {
				t.Fatalf("%s landed twice on %d", account, endpoint.Port)
			}
		}
		seen[endpoint.Port] = account
	}
	// 12 draws out of 21 slots collide by birthday; anything near a single port
	// would mean the hash is not spreading at all.
	if len(seen) < 8 {
		t.Fatalf("12 accounts used only %d distinct ports", len(seen))
	}

	wider := map[int]bool{}
	for i := 1; i <= 20; i++ {
		endpoint, err := pool.Pick("acct-" + strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		wider[endpoint.Port] = true
	}
	if len(wider) < 12 {
		t.Fatalf("20 accounts used only %d distinct ports", len(wider))
	}
}

// TestPickNonStickyReturnsTemplate: a local Clash is not pinnable, so the pool
// hands back exactly the template it was given.
func TestPickNonStickyReturnsTemplate(t *testing.T) {
	base := mustParse(t, clash)
	pool := Pool{Sticky: false, Base: base}
	picked, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(picked, base) {
		t.Fatalf("non-sticky pool changed the template:\n%+v\n%+v", picked, base)
	}
}

// TestPickAppliesCountry keeps -country meaningful instead of decorative: the
// override has to reach the username the provider reads.
func TestPickAppliesCountry(t *testing.T) {
	pool := Pool{Country: "de", Sticky: true, Start: 10000, End: 10000, Base: mustParse(t, diHTTP)}
	picked, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(picked.User, "__cr.de") {
		t.Fatalf("user = %q, want the __cr.de suffix", picked.User)
	}
	if picked.Country != "DE" || !strings.HasPrefix(picked.Ref(), "di:de:") {
		t.Fatalf("country/ref = %q / %q", picked.Country, picked.Ref())
	}

	region := Pool{Country: "GB", Sticky: true, Base: mustParse(t, trust711)}
	picked, err = region.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(picked.User, "-region-GB-") {
		t.Fatalf("711 user = %q, want -region-GB-", picked.User)
	}
	if picked.Ref() != "711:gb:sess-123" {
		t.Fatalf("711 ref = %q", picked.Ref())
	}
}

// TestPickOffsetWalksThePorts is what the CLI uses after a 403: the next
// attempt has to be a different lease, and the walk has to wrap.
func TestPickOffsetWalksThePorts(t *testing.T) {
	pool := Pool{Sticky: true, Ports: []int{10002, 10000, 10001}, Base: mustParse(t, diHTTP)}
	first, err := pool.Pick("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.PickOffset("acct-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == second.Port {
		t.Fatalf("offset 1 returned the same lease %d", first.Port)
	}
	third, err := pool.PickOffset("acct-01", 2)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := pool.PickOffset("acct-01", 3)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Ref() != first.Ref() {
		t.Fatalf("offset 3 did not wrap to the first pick: %s vs %s", wrapped.Ref(), first.Ref())
	}
	ports := map[int]bool{first.Port: true, second.Port: true, third.Port: true}
	if len(ports) != 3 {
		t.Fatalf("three offsets covered %d ports, want 3", len(ports))
	}
	back, err := pool.PickOffset("acct-01", -1)
	if err != nil {
		t.Fatal(err)
	}
	if back.Ref() != third.Ref() {
		t.Fatalf("offset -1 = %s, want %s", back.Ref(), third.Ref())
	}
}

// TestPickRefusesStickyLocal: pinning the machine's own proxy is a
// configuration the pipeline cannot honour, so it must fail loudly instead of
// claiming an exit that does not exist.
func TestPickRefusesStickyLocal(t *testing.T) {
	pool := Pool{Sticky: true, Base: mustParse(t, clash)}
	if _, err := pool.Pick("acct-01"); !errors.Is(err, ErrNotSticky) {
		t.Fatalf("err = %v, want ErrNotSticky", err)
	}
}

// TestPoolCandidates covers the port sources a pool can have.
func TestPoolCandidates(t *testing.T) {
	explicit := Pool{Ports: []int{10002, 10000, 10002}}
	ports, err := explicit.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{10000, 10002}) {
		t.Fatalf("ports = %v", ports)
	}

	derived := Pool{Start: 10010, End: 10012}
	ports, err = derived.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{10010, 10011, 10012}) {
		t.Fatalf("derived ports = %v", ports)
	}

	fallback := Pool{}
	ports, err = fallback.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != DefaultEnd-DefaultStart+1 || ports[0] != DefaultStart {
		t.Fatalf("default ports = %v", ports)
	}

	if _, err := (Pool{Start: 10010, End: 10000}).Candidates(); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("inverted range: %v", err)
	}
	if _, err := (Pool{Ports: []int{0}}).Candidates(); !errors.Is(err, ErrBadPort) {
		t.Fatalf("zero port: %v", err)
	}
}

func TestParsePorts(t *testing.T) {
	ports, err := ParsePorts("10000-10002")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{10000, 10001, 10002}) {
		t.Fatalf("range = %v", ports)
	}

	ports, err = ParsePorts(" 10011 , 10000,10011 ")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{10000, 10011}) {
		t.Fatalf("list = %v", ports)
	}

	ports, err = ParsePorts("")
	if err != nil || ports != nil {
		t.Fatalf("empty spec = %v / %v", ports, err)
	}

	for _, spec := range []string{"10002-10000", "abc", "0", "70000", "10000-", "-10000"} {
		if _, err := ParsePorts(spec); err == nil {
			t.Fatalf("%q was accepted", spec)
		}
	}
}
