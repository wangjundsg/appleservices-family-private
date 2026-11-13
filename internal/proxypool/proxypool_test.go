package proxypool

import (
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The shapes the pipeline really pastes in. Keeping them as constants means
// every test below reads the same endpoints the operators use, not a simplified
// stand-in.
const (
	diHTTP  = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:10012"
	diSOCKS = "socks5h://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:10012"
	// diLow and diHigh are the inclusive ends of the lease range (contract §2).
	diLow  = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:10000"
	diHigh = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:20000"
	// diRotate is the provider's rotating entrance. It is a perfectly good URL
	// and not a lease this package may claim, so Parse has to reject it instead
	// of calling it a non-sticky endpoint.
	diRotate = "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:8000"
	trust711 = "socks5h://user1-zone-custom-region-US-session-12345678-sessTime-30-sessAuto-0:pw-711@global.rotgb.trustsource.live:10000"
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

// TestParseDataImpulse pins the residential format: the login suffix carries the
// country, the port carries the lease, and both have to survive parsing because
// Ref is built out of them.
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

// TestParseDataImpulseLeaseBounds is contract §2: the gateway hands out
// 10000..20000 inclusive, and a port outside it is not a lease. Both ends are
// accepted, the neighbours are refused loudly.
func TestParseDataImpulseLeaseBounds(t *testing.T) {
	for _, raw := range []string{diLow, diHigh} {
		if endpoint := mustParse(t, raw); endpoint.Port != 10000 && endpoint.Port != 20000 {
			t.Fatalf("%s: port = %d", raw, endpoint.Port)
		}
	}
	for _, port := range []int{9999, 20001, 8000} {
		raw := "http://abc123__cr.us;sessttl.30:s3cr3t-pass@gw.dataimpulse.com:" + strconv.Itoa(port)
		endpoint, err := Parse(raw)
		if !errors.Is(err, ErrEndpointShape) {
			t.Fatalf("port %d: err = %v, want ErrEndpointShape", port, err)
		}
		if !strings.Contains(err.Error(), "10000-20000") {
			t.Fatalf("port %d: error does not name the lease range: %v", port, err)
		}
		if endpoint.User != "" {
			t.Fatalf("port %d: a rejected endpoint was still returned", port)
		}
	}
}

// TestValidateDataImpulse covers the username half of the same rule: the login
// suffix is not decoration, and a template missing it would dial an exit the
// provider never assigned.
func TestValidateDataImpulse(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		pass    string
		port    int
		wantErr error
	}{
		{"contract form", "abc123__cr.us;sessttl.30", "pw", 10012, nil},
		{"other country", "abc123__cr.de;sessttl.30", "pw", 10000, nil},
		{"other session time", "abc123__cr.us;sessttl.60", "pw", 20000, nil},
		{"missing session time", "abc123__cr.us", "pw", 10012, ErrEndpointShape},
		{"upper case country", "abc123__cr.US;sessttl.30", "pw", 10012, ErrEndpointShape},
		{"trailing field", "abc123__cr.us;sessttl.30;session.abcd", "pw", 10012, ErrEndpointShape},
		{"missing password", "abc123__cr.us;sessttl.30", "", 10012, ErrNoAuth},
		{"port below range", "abc123__cr.us;sessttl.30", "pw", 9999, ErrEndpointShape},
		{"port above range", "abc123__cr.us;sessttl.30", "pw", 20001, ErrEndpointShape},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := Endpoint{
				Kind: HTTP,
				Host: "gw.dataimpulse.com",
				Port: testCase.port,
				User: testCase.user,
				Pass: testCase.pass,
			}
			err := endpoint.Validate()
			if testCase.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, testCase.wantErr)
			}
		})
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
	if endpoint.User != "user1-zone-custom-region-US-session-12345678-sessTime-30-sessAuto-0" {
		t.Fatalf("user = %q", endpoint.User)
	}
	if endpoint.Country != "US" {
		t.Fatalf("country = %q", endpoint.Country)
	}
	if !endpoint.Sticky {
		t.Fatal("-session- pins the exit, Sticky = false")
	}
	if ref := endpoint.Ref(); ref != "711:us:12345678" {
		t.Fatalf("ref = %q, want 711:us:12345678", ref)
	}
	if endpoint.Provider() != provider711 {
		t.Fatalf("provider = %q", endpoint.Provider())
	}
}

// TestValidate711 is the §2 table for the second provider: upper-case region,
// eight decimal session digits, sessTime 5..180, sessAuto 0/1, port 10000.
func TestValidate711(t *testing.T) {
	const prefix = "user1-zone-custom-region-"
	cases := []struct {
		name string
		user string
		port int
		want bool // wantErr
	}{
		{"minimal", prefix + "US-session-12345678", 10000, false},
		{"contract example", prefix + "US-session-12345678-sessTime-30-sessAuto-0", 10000, false},
		{"sessTime low end", prefix + "DE-session-87654321-sessTime-5-sessAuto-1", 10000, false},
		{"sessTime high end", prefix + "GB-session-87654321-sessTime-180-sessAuto-0", 10000, false},
		{"lower case region", prefix + "us-session-12345678", 10000, true},
		{"mixed case region", prefix + "Us-session-12345678", 10000, true},
		{"region not two letters", prefix + "USA-session-12345678", 10000, true},
		{"session seven digits", prefix + "US-session-1234567", 10000, true},
		{"session nine digits", prefix + "US-session-123456789", 10000, true},
		{"session not decimal", prefix + "US-session-1234567a", 10000, true},
		{"sessTime four", prefix + "US-session-12345678-sessTime-4-sessAuto-0", 10000, true},
		{"sessTime 181", prefix + "US-session-12345678-sessTime-181-sessAuto-0", 10000, true},
		{"sessAuto two", prefix + "US-session-12345678-sessTime-30-sessAuto-2", 10000, true},
		{"sessTime without sessAuto", prefix + "US-session-12345678-sessTime-30", 10000, true},
		{"no zone marker", "user1-region-US-session-12345678", 10000, true},
		{"wrong port", prefix + "US-session-12345678", 10001, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := Endpoint{
				Kind: SOCKS5H,
				Host: "global.rotgb.trustsource.live",
				Port: testCase.port,
				User: testCase.user,
				Pass: "pw-711",
			}
			err := endpoint.Validate()
			if testCase.want {
				if !errors.Is(err, ErrEndpointShape) {
					t.Fatalf("Validate() = %v, want ErrEndpointShape", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
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

// TestValidateLocal keeps the third provider on this machine and credential-free.
func TestValidateLocal(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "LOCALHOST", "::1"} {
		endpoint := Endpoint{Kind: LOCAL, Host: host, Port: 7897}
		if err := endpoint.Validate(); err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
	}
	for _, testCase := range []Endpoint{
		{Kind: LOCAL, Host: "10.0.0.1", Port: 7897},
		{Kind: LOCAL, Host: "127.0.0.1", Port: 7897, User: "u", Pass: "p"},
	} {
		if err := testCase.Validate(); !errors.Is(err, ErrEndpointShape) {
			t.Fatalf("%+v: err = %v, want ErrEndpointShape", testCase, err)
		}
	}
}

// TestParseRejects is the set of inputs that must not become an endpoint: each
// one would otherwise reach a transport with a field missing or with an exit the
// provider never assigned.
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
		// §2 shape rules: a rotation port and a login without the suffix are not
		// DataImpulse leases.
		{"dataimpulse rotation port", diRotate, ErrEndpointShape},
		{"dataimpulse login without suffix", "http://abc:def@gw.dataimpulse.com:10012", ErrEndpointShape},
		{"711 lower case region", "socks5h://u-zone-custom-region-us-session-12345678:pw@global.rotgb.trustsource.live:10000", ErrEndpointShape},
		{"711 seven digit session", "socks5h://u-zone-custom-region-US-session-1234567:pw@global.rotgb.trustsource.live:10000", ErrEndpointShape},
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
			// A parse failure is allowed in a log, so it must not quote the
			// credential-bearing URL back out — the whole password space of these
			// cases is "def".
			for _, leak := range []string{"def@", ":def", "pw@", ":pw"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("%q: error leaks credentials: %v", testCase.raw, err)
				}
			}
		})
	}
}

// TestRefAndRedactedWithholdCredentials is the rule the profile depends on:
// whatever an endpoint looks like, the two renderings that leave the process
// carry no password and no usable username.
func TestRefAndRedactedWithholdCredentials(t *testing.T) {
	for _, raw := range []string{diHTTP, diSOCKS, diLow, diHigh, trust711, clash} {
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
		if endpoint.Kind != LOCAL && !strings.Contains(redacted, strconv.Itoa(endpoint.Port)) {
			t.Fatalf("%s: redacted lost the port: %q", raw, redacted)
		}
		if strings.Contains(redacted, "@") && !strings.Contains(redacted, "***@") {
			t.Fatalf("%s: redacted username is not masked: %q", raw, redacted)
		}
	}
	if got := mustParse(t, diHTTP).Redacted(); got != "http://abc***@gw.dataimpulse.com:10012" {
		t.Fatalf("redacted = %q", got)
	}
	if got := mustParse(t, trust711).Redacted(); got != "socks5h://use***@global.rotgb.trustsource.live:10000" {
		t.Fatalf("711 redacted = %q", got)
	}
	if got := mustParse(t, clash).Redacted(); got != "http://127.0.0.1:7897" {
		t.Fatalf("local redacted = %q", got)
	}
}

// TestRedactedHidesSeparators is the log half of §2: credentials that carry the
// userinfo separators (';', '@', ':') must not reappear in a log line, not even
// percent-encoded or sliver by sliver.
func TestRedactedHidesSeparators(t *testing.T) {
	endpoint := Endpoint{
		Kind: SOCKS5H,
		Host: "gw.dataimpulse.com",
		Port: 10012,
		User: "acct__cr.us;sessttl.30",
		Pass: "p@ss:w;rd",
	}
	redacted := endpoint.Redacted()
	for _, secret := range []string{"p@ss", "w;rd", "sessttl.30", "acct__cr.us"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted = %q, it carries %q", redacted, secret)
		}
	}
	if redacted != "socks5h://acc***@gw.dataimpulse.com:10012" {
		t.Fatalf("redacted = %q", redacted)
	}
	if strings.Contains(endpoint.Ref(), "p@ss") || strings.Contains(endpoint.Ref(), "sessttl") {
		t.Fatalf("ref = %q", endpoint.Ref())
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
		if first.URL() != second.URL() {
			t.Fatalf("%s: round trip changed the rendering:\n%s\n%s", raw, first.URL(), second.URL())
		}
	}
}

// TestURLEncodesCredentialsSeparately is contract §2's first rule: the username
// and the password are percent-encoded as two independent components, so ';' in
// a DataImpulse login and '@'/':' in a password cannot end the userinfo early or
// split one login into fields.
func TestURLEncodesCredentialsSeparately(t *testing.T) {
	endpoint := Endpoint{
		Kind: SOCKS5H,
		Host: "gw.dataimpulse.com",
		Port: 10012,
		User: "acct__cr.us;sessttl.30",
		Pass: "p@ss:w;rd",
	}
	assembled := endpoint.URL()
	const want = "socks5h://acct__cr.us%3Bsessttl.30:p%40ss%3Aw%3Brd@gw.dataimpulse.com:10012"
	if assembled != want {
		t.Fatalf("URL() = %q, want %q", assembled, want)
	}
	// Exactly one '@' — the separator. Anything else means a credential byte
	// escaped into the authority.
	if got := strings.Count(assembled, "@"); got != 1 {
		t.Fatalf("URL() carries %d '@': %q", got, assembled)
	}
	// ... and ':' appears three times: the scheme's, the user:password separator,
	// and the port's. The two the password carries are escaped.
	if got := strings.Count(assembled, ":"); got != 3 {
		t.Fatalf("URL() carries %d ':': %q", got, assembled)
	}

	parsed, err := url.Parse(assembled)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", assembled, err)
	}
	user := parsed.User.Username()
	pass, ok := parsed.User.Password()
	if !ok || user != endpoint.User || pass != endpoint.Pass {
		t.Fatalf("parsed credentials = %q / %q, want %q / %q", user, pass, endpoint.User, endpoint.Pass)
	}

	// The escaped form has to survive the package's own parser, including the
	// provider shape check, or URL() would emit something Validate rejects.
	reparsed, err := Parse(assembled)
	if err != nil {
		t.Fatalf("Parse(%q): %v", assembled, err)
	}
	if reparsed.User != endpoint.User || reparsed.Pass != endpoint.Pass || reparsed.Port != endpoint.Port {
		t.Fatalf("re-parse changed the endpoint: %+v", reparsed)
	}
}

// TestAtAppliesCountryAndPort replaces the old hash pick: the port comes from the
// table, so the pool only has to apply the country and validate the result.
func TestAtAppliesCountryAndPort(t *testing.T) {
	pool := Pool{Country: "de", Sticky: true, Ports: []int{10005}, Base: mustParse(t, diHTTP)}
	picked, err := pool.At(10005)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(picked.User, "__cr.de") {
		t.Fatalf("user = %q, want the __cr.de suffix", picked.User)
	}
	if picked.Country != "DE" || picked.Ref() != "di:de:10005" {
		t.Fatalf("country/ref = %q / %q", picked.Country, picked.Ref())
	}
	if picked.Port != 10005 || picked.Raw != "" {
		t.Fatalf("port/raw = %d / %q", picked.Port, picked.Raw)
	}
	// The rendering the transport dials keeps the rewritten country and still
	// escapes the login's ';'.
	for _, want := range []string{"__cr.de", "%3Bsessttl.30"} {
		if !strings.Contains(picked.URL(), want) {
			t.Fatalf("URL() = %q, want it to carry %q", picked.URL(), want)
		}
	}
}

// TestAtKeeps711Session is §4's other half: 711's candidate lives in -session-,
// so a lease port must not be written over the gateway port and the template's
// sessTime/sessAuto have to reach the provider untouched.
func TestAtKeeps711Session(t *testing.T) {
	pool := Pool{Country: "GB", Sticky: true, Base: mustParse(t, trust711)}
	picked, err := pool.At(0)
	if err != nil {
		t.Fatal(err)
	}
	if picked.Port != Trust711Port {
		t.Fatalf("port = %d, want %d", picked.Port, Trust711Port)
	}
	for _, want := range []string{"-region-GB-", "-session-12345678", "-sessTime-30-sessAuto-0"} {
		if !strings.Contains(picked.User, want) {
			t.Fatalf("711 user = %q, want it to keep %q", picked.User, want)
		}
	}
	if picked.Ref() != "711:gb:12345678" {
		t.Fatalf("711 ref = %q", picked.Ref())
	}
}

// TestAtRejectsPortOutsideLeaseRange keeps a caller from dialling a port the
// allocator would never hand out.
func TestAtRejectsPortOutsideLeaseRange(t *testing.T) {
	pool := Pool{Sticky: true, Ports: []int{10005}, Base: mustParse(t, diHTTP)}
	if _, err := pool.At(8000); !errors.Is(err, ErrEndpointShape) {
		t.Fatalf("At(8000) = %v, want ErrEndpointShape", err)
	}
	if _, err := pool.At(0); !errors.Is(err, ErrBadPort) {
		t.Fatalf("At(0) = %v, want ErrBadPort", err)
	}
}

// TestAtNonStickyReturnsTemplate: a local Clash is not pinnable, so the pool
// hands back exactly the template it was given.
func TestAtNonStickyReturnsTemplate(t *testing.T) {
	base := mustParse(t, clash)
	pool := Pool{Sticky: false, Base: base}
	picked, err := pool.At(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(picked, base) {
		t.Fatalf("non-sticky pool changed the template:\n%+v\n%+v", picked, base)
	}
}

// TestAtRefusesStickyLocal: pinning the machine's own proxy is a configuration
// the pipeline cannot honour, so it must fail loudly instead of claiming an exit
// that does not exist.
func TestAtRefusesStickyLocal(t *testing.T) {
	pool := Pool{Sticky: true, Base: mustParse(t, clash)}
	if _, err := pool.At(0); !errors.Is(err, ErrNotSticky) {
		t.Fatalf("err = %v, want ErrNotSticky", err)
	}
}

// TestSelectsPort pins which pools have a lease port to allocate.
func TestSelectsPort(t *testing.T) {
	if !(Pool{Base: mustParse(t, diHTTP)}).SelectsPort() {
		t.Fatal("a DataImpulse pool must allocate a lease port")
	}
	if (Pool{Base: mustParse(t, trust711)}).SelectsPort() {
		t.Fatal("711 pins its lease in the username, it must not allocate a port")
	}
	if (Pool{Base: mustParse(t, clash)}).SelectsPort() {
		t.Fatal("a local proxy has no lease port")
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
