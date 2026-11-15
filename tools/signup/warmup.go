package main

// Warm-up: make the session arrive at the create call the way a browser does.
//
// A real visitor loads the portal document, then the create configuration, then
// the widget page, and only then a handful of its script bundles. Each step
// carries the fetch metadata a browser would send for that kind of resource, and
// each step leaves cookies behind. None of this is forged data: the documents
// and bundles are public assets, and the only thing the code adds is the order,
// the pacing and the navigation headers.
//
// Warm-up is best effort. A failed step is recorded and the flow continues;
// only a total miss (no document and no config) makes the caller stop.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

// warmupStep is the receipt shape: what was fetched, how it answered, how big.
// Bodies are never kept.
type warmupStep struct {
	Step   string `json:"step"`
	Host   string `json:"host"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Bytes  int    `json:"bytes"`
}

// navigationHeaders mimic a top-level document load. Sec-Fetch-Site is "none"
// for the first navigation in a tab and "same-site"/"same-origin" afterwards.
func (s *Session) navigationHeaders(site, referer string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", s.userAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", s.locale)
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", site)
	h.Set("Sec-Fetch-User", "?1")
	if s.profileHints != nil {
		h.Set("Sec-Ch-Ua", s.profileHints.Brands)
		h.Set("Sec-Ch-Ua-Mobile", map[bool]string{true: "?1", false: "?0"}[s.profileHints.Mobile])
		h.Set("Sec-Ch-Ua-Platform", s.profileHints.Platform)
	}
	if referer != "" {
		h.Set("Referer", referer)
	}
	return h
}

// assetHeaders are what a page sends when it pulls one of its own bundles.
func (s *Session) assetHeaders(referer string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", s.userAgent)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", s.locale)
	h.Set("Sec-Fetch-Dest", "script")
	h.Set("Sec-Fetch-Mode", "no-cors")
	h.Set("Sec-Fetch-Site", "cross-site")
	if referer != "" {
		h.Set("Referer", referer)
	}
	return h
}

// widgetURL is the create iframe the portal embeds.
func (s *Session) widgetURL() string {
	return s.base + "/widget/account/?roleType=Agent&lv=0.4.3&widgetKey=" +
		url.QueryEscape(s.widgetKey) + "&v=3&appContext=account"
}

var scriptSrcPattern = regexp.MustCompile(`src="([^"]+\.js[^"]*)"`)

// runWarmup walks the browser path. maxAssets caps the bundle fetches; zero
// skips them.
// Apple can only observe its own properties, so "browse other websites" buys
// nothing. What it CAN see is that this browser's first and only Apple contact
// was the signup page — which no ordinary customer does. The journey prefix is an
// EXPERIMENT: homepage -> product -> support -> account page, carrying a referer
// chain and same-site markers. It reproduces the shape of such a visit; it does
// not reproduce a real click, and a real browser would also apply the source
// page's Referrer-Policy, which we can only approximate.
func (s *Session) runWarmup(ctx context.Context, pacer *pacing.Pacer, maxAssets int, journey bool) []warmupStep {
	var steps []warmupStep
	previous := "start"

	record := func(name string, status int, body []byte) {
		u, err := url.Parse(s.lastWarmupURL)
		host, path := "", ""
		if err == nil {
			host, path = u.Host, u.Path
		}
		steps = append(steps, warmupStep{Step: name, Host: host, Path: path, Status: status, Bytes: len(body)})
	}

	fetch := func(name, target string, header http.Header) (int, []byte, bool) {
		if pacer != nil {
			if _, err := pacer.Before(ctx, previous, name); err != nil {
				return 0, nil, false
			}
		}
		previous = name
		s.lastWarmupURL = target
		status, respHeader, body, err := s.send(ctx, http.MethodGet, target, header, nil)
		if err != nil {
			steps = append(steps, warmupStep{Step: name, Status: 0})
			return 0, nil, false
		}
		s.absorb(respHeader)
		record(name, status, body)
		return status, body, status >= 200 && status < 300
	}

	portalBase := strings.Replace(s.base, "//appleid.", "//account.", 1)
	landing := portalBase + "/account"

	referer := ""
	if journey {
		home, product, support := s.journeyURLs()
		if _, _, ok := fetch("journey-home", home, s.navigationHeaders("none", "")); ok {
			referer = referenceURL(s.lastWarmupURL)
		}
		// Same scheme, host and port as the homepage: same-origin, not same-site.
		if _, _, ok := fetch("journey-product", product, s.navigationHeaders("same-origin", referer)); ok {
			referer = referenceURL(s.lastWarmupURL)
		}
		if _, _, ok := fetch("journey-support", support, s.navigationHeaders("same-site", referer)); ok {
			// A customer who came from Apple's own pages arrives at the account
			// page by clicking, so the landing carries that referer.
			referer = referenceURL(s.lastWarmupURL)
		}
	}
	// www/support/account are all apple.com, i.e. same-site (different origins).
	landingHeaders := s.navigationHeaders("same-site", referer)
	if referer == "" {
		landingHeaders = s.navigationHeaders("none", "")
	}
	if _, _, ok := fetch("landing", landing, landingHeaders); !ok {
		warnf("warmup: portal document did not load; continuing")
	}

	config := s.base + "/account?countryCode=USA"
	if _, _, ok := fetch("config", config, s.navigationHeaders("same-site", landing)); !ok {
		warnf("warmup: create configuration did not load; continuing")
	}

	widget := s.widgetURL()
	if _, body, ok := fetch("widget", widget, s.navigationHeaders("same-origin", config)); ok && maxAssets > 0 {
		seen := map[string]bool{}
		for _, match := range scriptSrcPattern.FindAllStringSubmatch(string(body), -1) {
			if len(seen) >= maxAssets {
				break
			}
			src := match[1]
			if !strings.HasPrefix(src, "http") {
				src = "https://appleid.cdn-apple.com" + src
			}
			if seen[src] {
				continue
			}
			seen[src] = true
			fetch("asset", src, s.assetHeaders(widget))
		}
	}

	return steps
}

// journeyURLs keeps the real Apple properties in production and the same shape
// on the test host, so offline tests never reach the public internet.
func (s *Session) journeyURLs() (home, product, support string) {
	if s.base == defaultBase {
		return "https://www.apple.com/", "https://www.apple.com/shop/buy-iphone", "https://support.apple.com/"
	}
	return s.base + "/www/", s.base + "/www/shop/buy-iphone", s.base + "/support/"
}

// referenceURL strips the query so the referer stays a plausible page URL.
func referenceURL(raw string) string {
	if idx := strings.Index(raw, "?"); idx >= 0 {
		return raw[:idx]
	}
	return raw
}

// warmupCookieCount proves the chain left state behind.
func (s *Session) warmupCookieCount() int {
	if s.client == nil || s.client.Jar == nil {
		return 0
	}
	total := 0
	for _, raw := range []string{s.base, strings.Replace(s.base, "//appleid.", "//account.", 1)} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		total += len(s.client.Jar.Cookies(u))
	}
	return total
}

// describeWarmup is the receipt summary.
func describeWarmup(steps []warmupStep, cookies int) map[string]any {
	ok := 0
	for _, step := range steps {
		if step.Status >= 200 && step.Status < 300 {
			ok++
		}
	}
	return map[string]any{
		"steps":   steps,
		"fetched": len(steps),
		"ok":      ok,
		"cookies": cookies,
	}
}

var _ = fmt.Sprintf
var _ = time.Second
