package proxypool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// A preflight distinguishes three outcomes, because the pipeline reacts to them
// differently: a banned exit is worth another port (ErrBlocked), a broken or
// overloaded one is worth another try (ErrRetryable), and everything else is a
// configuration the caller has to fix instead of retrying (ErrUnexpected).
var (
	// ErrBlocked means the target answered 403 on this exit. On a residential
	// lease that usually means this port's address is burned for this site,
	// so the caller switches to another port.
	ErrBlocked = errors.New("proxypool: exit blocked by target (403)")
	// ErrRetryable covers 5xx, 429, transport failures and timeouts: another
	// exit may well work.
	ErrRetryable = errors.New("proxypool: transient failure")
	// ErrUnexpected covers a status that is neither success nor any of the
	// above, e.g. a 407 from a wrong password.
	ErrUnexpected = errors.New("proxypool: unexpected response")
)

// Probe answers "can this endpoint carry a request to target right now?".
// It is an interface so tests never have to reach the network, and so a future
// engine (a real browser, a raw CONNECT) can be swapped in.
type Probe interface {
	Check(ctx context.Context, e Endpoint, target string) (status int, ip string, err error)
}

// ProbeResult is what a preflight measured. Preflight itself only reports the
// verdict; the CLI needs the status and the address for its log line, which is
// why the measurement is a separate return value.
type ProbeResult struct {
	Status int
	IP     string
}

// HTTPProbe fetches target through the endpoint with a plain HTTP client.
type HTTPProbe struct {
	// Timeout bounds one check. Zero means the caller's context is the only
	// bound.
	Timeout time.Duration
}

// checkTimeout is the ceiling applied when a caller sets no Timeout: a probe
// that hangs must not hold a pipeline stage open indefinitely.
const checkTimeout = 30 * time.Second

// probeBodyLimit caps how much of the probe response is read. The address echo
// services answer in a few bytes; anything larger is not an address.
const probeBodyLimit = 4096

// Check performs one request through e and returns the status, the address the
// response echoed (may be empty) and the transport error, if any. Statuses are
// reported as they came back: redirects are not followed, because a 3xx already
// proves the exit works and following it would spend another request on the
// target.
func (h HTTPProbe) Check(ctx context.Context, e Endpoint, target string) (int, string, error) {
	target = strings.TrimSpace(target)
	probeURL, err := url.Parse(target)
	if err != nil || (probeURL.Scheme != "http" && probeURL.Scheme != "https") || probeURL.Host == "" {
		return 0, "", fmt.Errorf("%w: probe target is not an http(s) URL", ErrUnexpected)
	}

	proxyURL, err := url.Parse(e.URL())
	if err != nil {
		return 0, "", fmt.Errorf("%w: endpoint %s cannot be rebuilt: %v", ErrUnexpected, e.Ref(), err)
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = checkTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		TLSHandshakeTimeout: timeout,
		// The probe is one request per lease: keeping the connection alive
		// against a metered residential proxy is a cost with no benefit.
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL.String(), nil)
	if err != nil {
		return 0, "", fmt.Errorf("%w: build request: %v", ErrUnexpected, err)
	}
	request.Header.Set("User-Agent", "appleservices-proxypool/1")

	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, probeBodyLimit))
	return response.StatusCode, extractIP(string(body)), nil
}

// Preflight runs one check through p and turns the outcome into a verdict.
// The rules are the ones the pipeline retries on, nothing more: success is
// 2xx/3xx, a 403 is a banned exit, 5xx and 429 are worth another attempt.
func Preflight(ctx context.Context, p Probe, e Endpoint, target string) error {
	_, err := PreflightResult(ctx, p, e, target)
	return err
}

// PreflightResult is Preflight plus the measurement the caller prints.
func PreflightResult(ctx context.Context, p Probe, e Endpoint, target string) (ProbeResult, error) {
	status, ip, err := p.Check(ctx, e, target)
	result := ProbeResult{Status: status, IP: ip}
	if err != nil {
		// A transport error says nothing about the exit being banned; it says
		// the lease could not be used this time.
		return result, fmt.Errorf("%w: %w", ErrRetryable, err)
	}
	switch {
	case status >= 200 && status < 400:
		return result, nil
	case status == http.StatusForbidden:
		return result, fmt.Errorf("%w: %s answered %d", ErrBlocked, e.Ref(), status)
	case status >= 500 || status == http.StatusTooManyRequests:
		return result, fmt.Errorf("%w: %s answered %d", ErrRetryable, e.Ref(), status)
	default:
		return result, fmt.Errorf("%w: %s answered %d", ErrUnexpected, e.Ref(), status)
	}
}

// ipv4InText finds candidate addresses in a body that is not an address itself,
// e.g. a JSON blob or a "origin: 1.2.3.4, 5.6.7.8" line.
var ipv4InText = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)

// extractIP pulls the exit address out of a probe response. The echo services
// answer with a bare address, some answer with JSON, and the rest is best
// effort: a miss costs the profile a measurement, not the run.
func extractIP(body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return ""
	}
	if ip := normaliseIP(text); ip != "" {
		return ip
	}
	var payload struct {
		IP     string `json:"ip"`
		Query  string `json:"query"`
		Origin string `json:"origin"`
	}
	if json.Unmarshal([]byte(text), &payload) == nil {
		for _, field := range []string{payload.IP, payload.Query, payload.Origin} {
			for _, part := range strings.Split(field, ",") {
				if ip := normaliseIP(strings.TrimSpace(part)); ip != "" {
					return ip
				}
			}
		}
	}
	for _, candidate := range ipv4InText.FindAllString(text, -1) {
		if ip := normaliseIP(candidate); ip != "" {
			return ip
		}
	}
	return ""
}

// normaliseIP accepts only a literal address, so a body that merely looks like
// one ("999.1.1.1") never reaches the profile as a measurement.
func normaliseIP(text string) string {
	ip := net.ParseIP(strings.TrimSpace(text))
	if ip == nil {
		return ""
	}
	return ip.String()
}
