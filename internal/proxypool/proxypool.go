// Package proxypool turns the proxy configurations the account pipeline has to
// choose between into one comparable type, and answers the three questions the
// pipeline asks about it: which exit one account must use, whether that exit
// still works, and what the profile file should say about it afterwards.
//
// Apple correlates accounts by the address their traffic leaves from, so "one
// account, one exit" is a correctness property rather than a preference. The
// same account directory has to come back to the same lease on every run, and
// two accounts must not share one. That is what makes Pick a pure function of
// the account id, and what makes the sticky identity of an endpoint (its Ref)
// credential-free: a Ref is written into profile.json, which is a fingerprint
// file and not a secret store.
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
	// Session ids, whichever provider names them.
	sessionRe = regexp.MustCompile(`(?:-session-|;session\.|;sessid\.)([A-Za-z0-9._-]+)`)
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
	if kind == HTTP && user == "" && isLoopback(host) {
		e.Kind = LOCAL
		e.Sticky = false
		return e, nil
	}
	if user == "" || pass == "" {
		return Endpoint{}, fmt.Errorf("%w: %s://%s:%d needs a username and a password", ErrNoAuth, kind.scheme(), host, port)
	}
	e.Country = countryFromUser(user)
	e.Sticky = stickyFrom(user, port)
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
	return port >= dataImpulseLeaseStart && port <= dataImpulseLeaseEnd
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

// URL reassembles the endpoint with its credentials. Only a transport builder
// may call it; every log line goes through Redacted instead.
func (e Endpoint) URL() string {
	u := url.URL{
		Scheme: e.Kind.scheme(),
		Host:   net.JoinHostPort(e.Host, strconv.Itoa(e.Port)),
	}
	switch {
	case e.User != "" && e.Pass != "":
		u.User = url.UserPassword(e.User, e.Pass)
	case e.User != "":
		u.User = url.User(e.User)
	}
	return u.String()
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

// Pool is one routing decision waiting for an account id.
type Pool struct {
	Country string   // exit country; empty keeps whatever the template says
	Sticky  bool     // one account, one exit
	Ports   []int    // dataimpulse lease ports; empty falls back to Start..End
	Start   int      // first lease port when Ports is empty (0 = default start)
	End     int      // last lease port when Ports is empty (0 = default end)
	Base    Endpoint // template; the username carries __cr.<cc> / -region-/-session-
}

// DataImpulse publishes its sticky leases on 10000..20000. A pool that does not
// say where to look starts at the low end of that range, which is the span the
// provider hands out on a fresh account.
const (
	dataImpulseLeaseStart = 10000
	dataImpulseLeaseEnd   = 20000

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

// Pick returns the endpoint accountID has to use. The same account id always
// yields the same lease, so a retry of the same account cannot mint a second
// exit; two account ids that land on the same lease are a collision the caller
// has to see, not something Pick hides by falling through to the next port.
func (p Pool) Pick(accountID string) (Endpoint, error) {
	return p.PickOffset(accountID, 0)
}

// PickOffset is Pick moved offset places down the lease list. The CLI uses it
// after a target has banned an exit: the next port is a different lease, and
// picking it by the same account id keeps the choice reproducible.
func (p Pool) PickOffset(accountID string, offset int) (Endpoint, error) {
	base := p.Base
	if !p.Sticky {
		// A non-sticky template is the answer itself; a local Clash cannot be
		// pinned and nothing here pretends otherwise.
		return base, nil
	}
	if base.Kind == LOCAL {
		return Endpoint{}, fmt.Errorf("%w: %s", ErrNotSticky, base.Ref())
	}

	base = base.withCountry(p.Country)
	base.Sticky = true
	if !p.rewritesPort() {
		// 711 ties its lease to -session- inside the username, so the port is
		// part of the provider's front door and must not be rewritten.
		return base, nil
	}

	ports, err := p.Candidates()
	if err != nil {
		return Endpoint{}, err
	}
	index := (homeIndex(accountID, len(ports)) + offset%len(ports) + len(ports)) % len(ports)
	picked := base
	picked.Port = ports[index]
	// Raw described the template's port, which is no longer the port this
	// endpoint uses; URL() is the honest rendering from here on.
	picked.Raw = ""
	return picked, nil
}

// rewritesPort reports whether the pool selects a lease port. Only endpoints
// whose stickiness lives outside the username can be moved port to port.
func (p Pool) rewritesPort() bool {
	if p.Base.Provider() == provider711 {
		return false
	}
	return len(p.Ports) > 0 || p.Start != 0 || p.End != 0 || p.Base.Provider() == providerDataImpulse
}

// homeIndex maps an account id to a lease slot. FNV-1a is the mixer every other
// seed-to-choice mapping in this repository uses; its low bits are the weak
// ones, so the final xorshift spreads them before the modulo, which is what
// keeps a run of similar ids ("acct-01", "acct-02", ...) off the same port.
func homeIndex(accountID string, n int) int {
	if n <= 1 {
		return 0
	}
	hash := uint32(2166136261)
	for i := 0; i < len(accountID); i++ {
		hash ^= uint32(accountID[i])
		hash *= 16777619
	}
	hash ^= hash >> 16
	return int(hash % uint32(n))
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
