// Package proxypool turns the proxy configurations the account pipeline has to
// choose between into one comparable type, and answers the three questions the
// pipeline asks about it: which exit one account must use, whether that exit
// still works, and what the profile file should say about it afterwards.
//
// Apple correlates accounts by the address their traffic leaves from, so "one
// account, one exit" is a correctness property rather than a preference. The
// same account directory has to come back to the same lease on every run, and
// two accounts must not share one. DataImpulse distinguishes candidates by port,
// so the port an account holds is a lease that has to be written down and locked
// across processes — allocator.go — not re-derived from a hash two runs would
// happily compute the same way. 711 carries its lease inside the username and
// needs no table.
//
// The sticky identity of an endpoint (its Ref) is credential-free: a Ref is
// written into profile.json, which is a fingerprint file and not a secret store.
// The rules this file enforces are the ones in
// docs/PROXY-USAGE-CONTRACT-2026-11-13.md: address format and encoding (§2),
// candidate allocation (§4) and "credentials never reach a log line" (§7).
package proxypool

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind is the transport an endpoint speaks.
type Kind int

const (
	// HTTP is an HTTP proxy (the CONNECT or absolute-URI kind). Both
	// residential providers hand this out on their http:// URLs.
	HTTP Kind = iota

	// SOCKS5H is SOCKS5 with proxy-side name resolution. The distinction from
	// plain SOCKS5 is not cosmetic: resolving the name locally would put the
	// DNS lookup, and with it the account's real region, outside the tunnel.
	SOCKS5H

	// LOCAL is this machine's own proxy: no credentials, no fixed exit. Usable
	// only where nothing depends on which address a request leaves from.
	LOCAL
)

// String returns the URL scheme the kind is written with.
func (k Kind) String() string {
	switch k {
	case SOCKS5H:
		return "socks5h"
	case LOCAL:
		return "local"
	default:
		return "http"
	}
}

// scheme is the scheme URL() and Redacted() rebuild the endpoint with.
func (k Kind) scheme() string {
	if k == SOCKS5H {
		return "socks5h"
	}
	return "http"
}

// Endpoint is one proxy entrance, parsed into the fields the pipeline reasons
// about. Raw keeps the string the operator pasted in; everything else is
// derived, so a caller never has to re-parse a URL to know the port or country.
type Endpoint struct {
	Raw     string // original URL (before redaction); empty when assembled by Pick
	Kind    Kind
	Host    string
	Port    int
	User    string // includes the provider's own suffix, e.g. __cr.us / -region-/-session-
	Pass    string
	Sticky  bool   // true when the entrance is bound to one fixed exit
	Country string // upper case ISO country, "" when the entrance does not choose one
}

// Provider identifiers used by Ref.
const (
	providerDataImpulse = "dataimpulse"
	provider711         = "711"

	// anyCountry is the Ref segment for an endpoint that does not pin a
	// country, so a Ref always keeps the same segment count.
	anyCountry = "any"
)

// The three provider suffixes are the only place the pipeline learns a
// country, a session or a lease out of a username. They are read here and
// nowhere else so Ref and the profile cannot disagree about what an endpoint
// is.
var (
	// DataImpulse: <login>__cr.<cc>;sessttl.30
	diCountryRe = regexp.MustCompile(`(__cr\.)([A-Za-z]{2})`)
	// 711: <user>-zone-custom-region-<CC>-session-<sid>
	regionRe = regexp.MustCompile(`(-region-)([A-Za-z]{2})(?:-|$)`)
	// Session ids, whichever provider names them. The value stops at '-' because
	// both providers keep their remaining parameters behind one: 711 writes
	// -sessTime-/-sessAuto- right after -session-<id>.
	sessionRe = regexp.MustCompile(`(?:-session-|;session\.|;sessid\.)([A-Za-z0-9._]+)`)
)

// The provider username shapes Validate enforces (contract §2). They are
// anchored on purpose: a login that does not match is a configuration the caller
// has to fix, and guessing at it would move the account to an exit nobody asked
// for.
var (
	// DataImpulse: <login>__cr.<cc>;sessttl.<n>. The contract puts the country in
	// lower case for this provider; the local session time is fixed at
	// sessttl.30 but any digit run is accepted so a template that pins another
	// value still parses.
	diUserRe = regexp.MustCompile(`^([^\s@/:;]+)__cr\.([a-z]{2});sessttl\.([0-9]+)$`)
	// 711: <account>-zone-custom-region-<CC>-session-<8 digits>, optionally
	// followed by -sessTime-<5..180>-sessAuto-<0|1>. The two optional parts are
	// one unit: sessTime without sessAuto is a template half-edited.
	user711Re = regexp.MustCompile(`^(.+?)-zone-custom-region-([A-Za-z]+)-session-([0-9A-Za-z]+)(?:-sessTime-([0-9]+)-sessAuto-([0-9]+))?$`)
)

// Errors a caller has to be able to tell apart.
var (
	// ErrInvalid marks a proxy URL this package cannot read at all.
	ErrInvalid = errors.New("proxypool: invalid proxy url")
	// ErrUnknownScheme marks a URL whose scheme names no transport we drive.
	ErrUnknownScheme = errors.New("proxypool: unsupported proxy scheme")
	// ErrNoPort marks a URL without an explicit port. Neither provider will
	// let a lease be guessed at, so a missing port is a typo, not a default.
	ErrNoPort = errors.New("proxypool: proxy url has no port")
	// ErrBadPort marks a port outside 1..65535.
	ErrBadPort = errors.New("proxypool: unusable port")
	// ErrNoAuth marks a provider URL without credentials.
	ErrNoAuth = errors.New("proxypool: proxy url has no credentials")
	// ErrNoPorts marks a sticky pool that has nothing to pick from.
	ErrNoPorts = errors.New("proxypool: pool has no ports")
	// ErrNotSticky marks a pool that asks a local proxy to hold one exit.
	ErrNotSticky = errors.New("proxypool: local proxy cannot hold a sticky exit")
	// ErrEmptyRef marks an endpoint with no stable reference to record.
	ErrEmptyRef = errors.New("proxypool: endpoint has no egress reference")
	// ErrNoIP marks a measured exit address that is not an address.
	ErrNoIP = errors.New("proxypool: empty egress address")
	// ErrEndpointShape marks an endpoint that parses but does not match the
	// provider's own format: a DataImpulse login on a rotation port, a 711
	// session that is not eight digits, a local proxy that grew credentials.
	// Callers get this instead of a silently "repaired" exit (contract §2).
	ErrEndpointShape = errors.New("proxypool: endpoint does not match its provider format")
	// ErrNoAccount marks an allocation without an account id: the table is keyed
	// by account, so an empty one would hand the same port to every caller.
	ErrNoAccount = errors.New("proxypool: allocation needs an account id")
	// ErrPortLock marks a port table another process kept locked for longer than
	// Allocate is willing to wait.
	ErrPortLock = errors.New("proxypool: proxy port table is locked")
	// ErrPortTable marks a port table this package cannot read or write.
	ErrPortTable = errors.New("proxypool: proxy port table unavailable")
	// ErrNoFreePort marks a candidate set with nothing left to lease.
	ErrNoFreePort = errors.New("proxypool: every candidate port is leased")
	// ErrLeaseOutsidePool marks an account that already holds a live lease on a
	// port the caller no longer offers. Handing out a second port would give the
	// account a second exit, which is the one thing the table exists to prevent.
	ErrLeaseOutsidePool = errors.New("proxypool: account holds a port outside the candidate set")
)

// Parse reads one provider URL. Credentials are kept verbatim because URL()
// has to reproduce them, but they never enter Ref or Redacted: those two are
// the only renderings allowed to leave the process.
func Parse(raw string) (Endpoint, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Endpoint{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// url.Error quotes the entire URL, password included. Only the
		// underlying complaint is safe to hand back.
		return Endpoint{}, fmt.Errorf("%w: %v", ErrInvalid, unwrapURLError(err))
	}

	var kind Kind
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		kind = HTTP
	case "socks5h":
		kind = SOCKS5H
	default:
		return Endpoint{}, fmt.Errorf("%w: %q (want http, https or socks5h)", ErrUnknownScheme, parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return Endpoint{}, fmt.Errorf("%w: no host", ErrInvalid)
	}
	portText := parsed.Port()
	if portText == "" {
		return Endpoint{}, fmt.Errorf("%w: %s has none", ErrNoPort, host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("%w: %q", ErrBadPort, portText)
	}

	var user, pass string
	if parsed.User != nil {
		user = parsed.User.Username()
		pass, _ = parsed.User.Password()
	}

	e := Endpoint{Raw: trimmed, Kind: kind, Host: host, Port: port, User: user, Pass: pass}

	// A bare loopback HTTP URL is this machine's own proxy, not a provider
	// lease: it carries no credentials and pins no exit.
	switch {
	case kind == HTTP && user == "" && isLoopback(host):
		e.Kind = LOCAL
	case user == "" || pass == "":
		return Endpoint{}, fmt.Errorf("%w: %s://%s:%d needs a username and a password", ErrNoAuth, kind.scheme(), host, port)
	default:
		e.Country = countryFromUser(user)
		e.Sticky = stickyFrom(user, port)
	}
	// The shape check comes last: everything above decides what the endpoint is,
	// and this decides whether the provider will accept it (contract §2).
	if err := e.Validate(); err != nil {
		return Endpoint{}, err
	}
	return e, nil
}

// unwrapURLError drops the *url.Error wrapper, which carries the URL itself.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// countryFromUser reads the provider's country suffix.
func countryFromUser(user string) string {
	if match := diCountryRe.FindStringSubmatch(user); match != nil {
		return strings.ToUpper(match[2])
	}
	if match := regionRe.FindStringSubmatch(user); match != nil {
		return strings.ToUpper(match[2])
	}
	return ""
}

// sessionFromUser reads the provider's session id, if it names one.
func sessionFromUser(user string) string {
	if match := sessionRe.FindStringSubmatch(user); match != nil {
		return strings.Trim(match[1], ".-;")
	}
	return ""
}

// stickyFrom reports whether the entrance is pinned to one exit. DataImpulse
// pins on the lease port (10000..20000 all fall on the same sticky pool: one
// port is one lease), 711 pins on -session-. Either way an explicit session id
// pins as well, which is the one signal both providers share.
func stickyFrom(user string, port int) bool {
	if sessionFromUser(user) != "" {
		return true
	}
	return port >= DataImpulseLeaseStart && port <= DataImpulseLeaseEnd
}

// Provider names the service an endpoint belongs to, or "" when it is a
// hand-rolled proxy. The CLI uses it to refuse a template pasted into the wrong
// -provider.
func (e Endpoint) Provider() string {
	if strings.Contains(e.User, "__cr.") || strings.Contains(e.Host, "dataimpulse.com") {
		return providerDataImpulse
	}
	if strings.Contains(e.User, "-region-") || strings.Contains(e.Host, "trustsource.live") {
		return provider711
	}
	return ""
}

// Ref is the stable, credential-free identity of the endpoint: what goes into
// profile.Expected.EgressRef and what an operator compares between two runs.
//
//	di:us:10012         DataImpulse lease on port 10012
//	di:us:sess-abc      DataImpulse session id
//	711:us:sess-123     711 session sess-123
//	local:7897          this machine's proxy port
func (e Endpoint) Ref() string {
	country := strings.ToLower(e.Country)
	if country == "" {
		country = anyCountry
	}
	switch e.Provider() {
	case providerDataImpulse:
		return "di:" + country + ":" + leaseSegment(e)
	case provider711:
		return "711:" + country + ":" + leaseSegment(e)
	}
	if e.Kind == LOCAL {
		return "local:" + strconv.Itoa(e.Port)
	}
	return e.Kind.String() + ":" + strings.ToLower(e.Host) + ":" + strconv.Itoa(e.Port)
}

// leaseSegment is the part of a Ref that says which lease inside a provider the
// endpoint sits on.
func leaseSegment(e Endpoint) string {
	if session := sessionFromUser(e.User); session != "" {
		return session
	}
	return strconv.Itoa(e.Port)
}

// Validate reports whether the endpoint may be handed to a transport at all.
// Parse calls it, and so does every pool that rewrites a country or a port, so a
// URL that parses but would dial an exit the provider never handed out fails
// loudly here instead of degrading into "some exit" later (contract §2).
func (e Endpoint) Validate() error {
	switch e.Provider() {
	case providerDataImpulse:
		return e.validateDataImpulse()
	case provider711:
		return e.validate711()
	}
	if e.Kind == LOCAL {
		return e.validateLocal()
	}
	return nil
}

// validateDataImpulse pins the residential shape: the login carries the country
// in lower case and the session time, and the lease lives on 10000..20000
// inclusive. A port outside that range is the provider's rotating entrance, not
// a candidate this package may claim to hold (§2, §4).
func (e Endpoint) validateDataImpulse() error {
	if e.User == "" || e.Pass == "" {
		return fmt.Errorf("%w: %s needs a username and a password", ErrNoAuth, e.Redacted())
	}
	if diUserRe.FindStringSubmatch(e.User) == nil {
		return fmt.Errorf("%w: %s DataImpulse username must be <login>__cr.<lower-case cc>;sessttl.<n>", ErrEndpointShape, e.Redacted())
	}
	if e.Port < DataImpulseLeaseStart || e.Port > DataImpulseLeaseEnd {
		return fmt.Errorf("%w: %s port %d is outside the lease range %d-%d",
			ErrEndpointShape, e.Redacted(), e.Port, DataImpulseLeaseStart, DataImpulseLeaseEnd)
	}
	return nil
}

// validate711 pins the second provider: upper-case region code, an eight digit
// session, an optional sessTime/sessAuto pair inside the ranges the local
// contract allows, and the one gateway port the template ships on (§2).
func (e Endpoint) validate711() error {
	if e.User == "" || e.Pass == "" {
		return fmt.Errorf("%w: %s needs a username and a password", ErrNoAuth, e.Redacted())
	}
	match := user711Re.FindStringSubmatch(e.User)
	if match == nil {
		return fmt.Errorf("%w: %s 711 username must be <account>-zone-custom-region-<CC>-session-<8 digits> with an optional -sessTime-<5-180>-sessAuto-<0|1>",
			ErrEndpointShape, e.Redacted())
	}
	region := match[2]
	if len(region) != 2 || region != strings.ToUpper(region) {
		return fmt.Errorf("%w: %s region must be two upper-case letters, e.g. -region-US-", ErrEndpointShape, e.Redacted())
	}
	if session := match[3]; len(session) != 8 || !isDigits(session) {
		return fmt.Errorf("%w: %s session must be eight decimal digits after -session-", ErrEndpointShape, e.Redacted())
	}
	if match[4] != "" || match[5] != "" {
		seconds, err := strconv.Atoi(match[4])
		if err != nil || seconds < 5 || seconds > 180 {
			return fmt.Errorf("%w: %s sessTime must be 5-180 seconds", ErrEndpointShape, e.Redacted())
		}
		if match[5] != "0" && match[5] != "1" {
			return fmt.Errorf("%w: %s sessAuto must be 0 or 1", ErrEndpointShape, e.Redacted())
		}
	}
	if e.Port != Trust711Port {
		return fmt.Errorf("%w: %s 711 leases live on port %d", ErrEndpointShape, e.Redacted(), Trust711Port)
	}
	return nil
}

// validateLocal keeps the machine's own proxy credential-free and on this
// machine: it is the one entrance that cannot carry an account's exit.
func (e Endpoint) validateLocal() error {
	if e.User != "" || e.Pass != "" {
		return fmt.Errorf("%w: %s local proxy must not carry credentials", ErrEndpointShape, e.Redacted())
	}
	if !isLoopback(e.Host) {
		return fmt.Errorf("%w: %s local proxy must be a loopback host (127.0.0.1 or localhost)", ErrEndpointShape, e.Redacted())
	}
	return nil
}

// isDigits reports whether s is a non-empty run of decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// URL reassembles the endpoint with its credentials. Only a transport builder
// may call it; every log line goes through Redacted instead.
//
// Contract §2 asks for the username and the password to be URL-encoded
// separately — quote(user, safe="") and quote(password, safe="") — and only then
// joined with ':' and '@'. url.UserPassword keeps that split: the two halves can
// never bleed into each other. Its escaping, though, is the loose RFC 3986
// §3.2.1 set, which leaves sub-delims such as ';' alone, and a DataImpulse login
// ends in "__cr.<cc>;sessttl.<n>" — a gateway that splits userinfo on ';' would
// read two fields out of one login. So each half is escaped with safe="" here
// and spliced onto the url.URL rendering of scheme://host:port.
func (e Endpoint) URL() string {
	base := url.URL{
		Scheme: e.Kind.scheme(),
		Host:   net.JoinHostPort(e.Host, strconv.Itoa(e.Port)),
	}
	if e.User == "" && e.Pass == "" {
		return base.String()
	}
	return base.Scheme + "://" + e.userinfo() + "@" + base.Host
}

// userinfo renders the credentials the way contract §2 wants them. The split
// comes from url.UserPassword, so the username and the password are read exactly
// as url.Parse reads them back.
func (e Endpoint) userinfo() string {
	info := url.UserPassword(e.User, e.Pass)
	user := info.Username()
	pass, _ := info.Password()
	return escapeCredential(user) + ":" + escapeCredential(pass)
}

// escapeCredential percent-encodes every byte outside RFC 3986 §2.3 unreserved,
// which is what Python's quote(part, safe="") does and what the contract's
// examples assume. net/url exposes no encoder with exactly that set: PathEscape
// keeps ':' and '@', UserPassword keeps ';'.
func escapeCredential(s string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hex[c>>4])
		out.WriteByte(hex[c&0x0f])
	}
	return out.String()
}

// Redacted is the log-safe rendering: the password is gone entirely and the
// username keeps only enough of a prefix to tell two logins apart.
func (e Endpoint) Redacted() string {
	host := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	scheme := e.Kind.scheme()
	if e.User == "" {
		return scheme + "://" + host
	}
	return scheme + "://" + maskUser(e.User) + "@" + host
}

// maskUser keeps a short prefix so an operator can still tell which login a
// line is about, and nothing else.
func maskUser(user string) string {
	runes := []rune(user)
	if len(runes) <= 3 {
		return "***"
	}
	return string(runes[:3]) + "***"
}

// withCountry rewrites the provider's country suffix, leaving a username that
// carries none untouched.
func (e Endpoint) withCountry(country string) Endpoint {
	if country == "" {
		return e
	}
	if loc := diCountryRe.FindStringSubmatchIndex(e.User); loc != nil {
		e.User = e.User[:loc[4]] + strings.ToLower(country) + e.User[loc[5]:]
	} else if loc := regionRe.FindStringSubmatchIndex(e.User); loc != nil {
		e.User = e.User[:loc[4]] + strings.ToUpper(country) + e.User[loc[5]:]
	}
	e.Country = strings.ToUpper(country)
	return e
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Pool is one routing decision waiting for a lease port.
type Pool struct {
	Country string   // exit country; empty keeps whatever the template says
	Sticky  bool     // one account, one exit
	Ports   []int    // dataimpulse lease ports; empty falls back to Start..End
	Start   int      // first lease port when Ports is empty (0 = default start)
	End     int      // last lease port when Ports is empty (0 = default end)
	Base    Endpoint // template; the username carries __cr.<cc> / -region-/-session-
}

// DataImpulse publishes its sticky leases on 10000..20000 inclusive (contract
// §2). Both ends are usable, so the range is closed. 711 hands every customer
// the same front-door port and distinguishes candidates in the username.
const (
	DataImpulseLeaseStart = 10000
	DataImpulseLeaseEnd   = 20000

	// Trust711Port is the only port the 711 template ships on.
	Trust711Port = 10000

	// DefaultStart and DefaultEnd bound a pool that names no ports.
	DefaultStart = 10000
	DefaultEnd   = 10020
)

// Candidates returns the sorted, de-duplicated lease ports this pool can pick
// from.
func (p Pool) Candidates() ([]int, error) {
	var ports []int
	if len(p.Ports) > 0 {
		ports = append(ports, p.Ports...)
	} else {
		start, end := p.Start, p.End
		if start == 0 && end == 0 {
			start, end = DefaultStart, DefaultEnd
		}
		if start == 0 {
			start = end
		}
		if end == 0 {
			end = start
		}
		if start < 1 || end < 1 || end < start {
			return nil, fmt.Errorf("%w: range %d-%d", ErrNoPorts, start, end)
		}
		ports = make([]int, 0, end-start+1)
		for port := start; port <= end; port++ {
			ports = append(ports, port)
		}
	}

	seen := make(map[int]bool, len(ports))
	out := ports[:0]
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("%w: %d", ErrBadPort, port)
		}
		if seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	if len(out) == 0 {
		return nil, ErrNoPorts
	}
	sort.Ints(out)
	return out, nil
}

// At returns the endpoint this pool describes on one specific lease port. The
// port table decides which port an account holds (allocator.go); At is how that
// decision becomes a dialable endpoint, without a second, hash-shaped opinion
// about which port it should have been.
//
// port is ignored by a pool that does not select ports: 711 carries its lease in
// -session-, and a non-sticky template is the answer itself.
func (p Pool) At(port int) (Endpoint, error) {
	base := p.Base
	if !p.Sticky {
		return base, nil
	}
	if base.Kind == LOCAL {
		return Endpoint{}, fmt.Errorf("%w: %s", ErrNotSticky, base.Ref())
	}

	base = base.withCountry(p.Country)
	base.Sticky = true
	if p.SelectsPort() {
		if port < 1 || port > 65535 {
			return Endpoint{}, fmt.Errorf("%w: %d", ErrBadPort, port)
		}
		base.Port = port
		// Raw described the template's port, which is no longer the port this
		// endpoint uses; URL() is the honest rendering from here on.
		base.Raw = ""
	}
	// The country rewrite and the port change both have to survive the
	// provider's own shape rules, or the caller gets a URL that dials an exit
	// nobody allocated.
	if err := base.Validate(); err != nil {
		return Endpoint{}, err
	}
	return base, nil
}

// SelectsPort reports whether the exit is chosen by lease port, which is what
// the port table exists to hand out. Only a provider whose stickiness lives
// outside the username can be moved port to port; 711 pins its lease in
// -session- and a local proxy holds no lease at all.
func (p Pool) SelectsPort() bool {
	if p.Base.Provider() == provider711 {
		return false
	}
	return len(p.Ports) > 0 || p.Start != 0 || p.End != 0 || p.Base.Provider() == providerDataImpulse
}

// ParsePorts reads the -ports syntax the CLI accepts: comma separated single
// ports and inclusive ranges, e.g. "10000-10020", "10000,10011" or a mix. An
// empty spec returns no ports, which lets Pool fall back to its own default.
func ParsePorts(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var ports []int
	seen := make(map[int]bool)
	add := func(port int) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%w: %d is outside 1..65535", ErrBadPort, port)
		}
		if !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lowText, highText, isRange := strings.Cut(part, "-")
		if !isRange {
			port, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("%w: %q is not a port", ErrBadPort, part)
			}
			if err := add(port); err != nil {
				return nil, err
			}
			continue
		}
		low, err := strconv.Atoi(strings.TrimSpace(lowText))
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a port range", ErrBadPort, part)
		}
		high, err := strconv.Atoi(strings.TrimSpace(highText))
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a port range", ErrBadPort, part)
		}
		if high < low {
			return nil, fmt.Errorf("%w: range %q is empty", ErrBadPort, part)
		}
		for port := low; port <= high; port++ {
			if err := add(port); err != nil {
				return nil, err
			}
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("%w: %q lists no ports", ErrBadPort, spec)
	}
	sort.Ints(ports)
	return ports, nil
}
