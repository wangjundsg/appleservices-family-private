package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

// warmupFixture serves the three documents a browser would load plus one bundle
// host, and records how each request looked.
type warmupFixture struct {
	mu       sync.Mutex
	requests []*http.Request
	asset    *httptest.Server
}

func (f *warmupFixture) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.mu.Unlock()

		switch {
		case strings.HasPrefix(r.URL.Path, "/www/") || strings.HasPrefix(r.URL.Path, "/support/"):
			http.SetCookie(w, &http.Cookie{Name: "journey", Value: "1", Path: "/"})
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>apple</html>"))
		case r.URL.Path == "/account" && r.URL.RawQuery == "":
			http.SetCookie(w, &http.Cookie{Name: "landing", Value: "1", Path: "/"})
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<html><body>portal</body></html>`))
		case r.URL.Path == "/account":
			http.SetCookie(w, &http.Cookie{Name: "config", Value: "1", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"locale":"en_US"}`))
		case strings.HasPrefix(r.URL.Path, "/widget/account/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script src="` + f.asset.URL + `/static/widget.js"></script>` +
				`<script src="` + f.asset.URL + `/static/widget.js"></script></html>`))
		default:
			w.WriteHeader(404)
		}
	})
}

func TestWarmupFollowsTheBrowserPath(t *testing.T) {
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "cdn", Value: "1", Path: "/"})
		_, _ = w.Write([]byte("/*js*/"))
	}))
	defer assets.Close()
	fixture := &warmupFixture{asset: assets}
	server := httptest.NewServer(fixture.handler(t))
	defer server.Close()

	session, err := newSession(SessionOptions{Base: server.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	session.widgetKey = "test-widget"

	var slept []time.Duration
	pacer := pacing.New(pacing.Config{Enabled: true, Min: 10 * time.Millisecond, Max: 20 * time.Millisecond, Seed: 5},
		func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		})

	steps := session.runWarmup(context.Background(), pacer, 3, false)
	if len(steps) < 3 {
		t.Fatalf("warm-up took %d steps: %+v", len(steps), steps)
	}
	if steps[0].Step != "landing" || steps[1].Step != "config" || steps[2].Step != "widget" {
		t.Fatalf("unexpected order: %+v", steps)
	}
	// The duplicate script tag must be fetched once.
	assetCount := 0
	for _, s := range steps {
		if s.Step == "asset" {
			assetCount++
		}
	}
	if assetCount != 1 {
		t.Fatalf("asset fetched %d times, want 1", assetCount)
	}
	if len(slept) != len(steps) {
		t.Fatalf("paced %d times for %d steps", len(slept), len(steps))
	}
	if cookies := session.warmupCookieCount(); cookies < 2 {
		t.Fatalf("warm-up left %d cookies, want the landing and config cookies", cookies)
	}

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	var landing *http.Request
	for _, r := range fixture.requests {
		if r.URL.Path == "/account" && r.URL.RawQuery == "" && landing == nil {
			landing = r
		}
	}
	assetSeen := false
	for _, r := range fixture.requests {
		if strings.HasPrefix(r.URL.Path, "/widget/account/") {
			assetSeen = true
		}
	}
	if landing == nil {
		t.Fatal("portal document was never fetched")
	}
	if got := landing.Header.Get("Sec-Fetch-Mode"); got != "navigate" {
		t.Fatalf("landing Sec-Fetch-Mode = %q", got)
	}
	if got := landing.Header.Get("Sec-Fetch-Dest"); got != "document" {
		t.Fatalf("landing Sec-Fetch-Dest = %q", got)
	}
	if got := landing.Header.Get("Sec-Fetch-Site"); got != "none" {
		t.Fatalf("first navigation Sec-Fetch-Site = %q", got)
	}
	if !strings.Contains(landing.Header.Get("Accept"), "text/html") {
		t.Fatalf("landing Accept = %q", landing.Header.Get("Accept"))
	}
	if !assetSeen {
		t.Fatal("widget document was never fetched")
	}
}

func TestAssetRequestsLookLikeSubresources(t *testing.T) {
	session, err := newSession(SessionOptions{Base: "https://appleid.apple.com", Locale: "en-US", Timezone: "America/New_York"})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	h := session.assetHeaders("https://appleid.apple.com/widget/account/")
	if h.Get("Sec-Fetch-Dest") != "script" || h.Get("Sec-Fetch-Mode") != "no-cors" || h.Get("Sec-Fetch-Site") != "cross-site" {
		t.Fatalf("asset headers = %v", h)
	}
	if h.Get("Referer") == "" {
		t.Fatal("asset request must carry the widget referer")
	}
	if h.Get("Sec-Fetch-User") != "" {
		t.Fatal("subresources must not claim a user gesture")
	}

	nav := session.navigationHeaders("same-site", "https://account.apple.com/account")
	if nav.Get("Sec-Fetch-User") != "?1" || nav.Get("Upgrade-Insecure-Requests") != "1" {
		t.Fatalf("navigation headers = %v", nav)
	}
}

func TestHumanModeFreezesPacingAndWarmup(t *testing.T) {
	fixture := &warmupFixture{asset: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))}
	defer fixture.asset.Close()
	// The state machine fixture answers the API calls; warm-up needs documents,
	// so this test runs warm-up through the real CLI against a combined server.
	comb := &strictSignup{calls: map[string]int{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/www/") || strings.HasPrefix(r.URL.Path, "/support/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>apple</html>"))
		case r.URL.Path == "/account" && r.URL.RawQuery == "" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>portal</html>"))
		case strings.HasPrefix(r.URL.Path, "/widget/account/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>widget</html>"))
		default:
			comb.handler(t).ServeHTTP(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	path := filepath.Join(t.TempDir(), "task.json")
	args := []string{
		"-email", "fixture@example.test",
		"-phone", "4155550134", "-phonecc", "US", "-stage", "all", "-json",
		"-human", "-pacing-min", "0s", "-pacing-max", "1ms", "-warmup-assets", "0",
		"-captcha-answer", "ABCDE", "-state", path,
	}
	code, receipt := invokeSignup(t, path, server.URL, args...)
	if code != 4 {
		t.Fatalf("exit = %d (%v), want 4", code, receipt)
	}
	warmup, _ := receipt["warmup"].(map[string]any)
	if warmup == nil {
		t.Fatalf("receipt has no warm-up block: %v", receipt)
	}
	if fetched, _ := warmup["fetched"].(float64); fetched < 3 {
		t.Fatalf("warm-up fetched %v steps: %v", warmup["fetched"], warmup)
	}
	pc, _ := receipt["pacing"].(map[string]any)
	if pc == nil || pc["enabled"] != true {
		t.Fatalf("pacing block = %v", receipt["pacing"])
	}

	saved, err := loadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if saved.Human == nil || !saved.Human.Pacing.Enabled {
		t.Fatal("pacing configuration was not frozen into the task")
	}
	seed := saved.Human.Pacing.Seed
	firstWarmup := saved.Warmup["fetched"]

	// Resume: same task, same seed, and the warm-up is not repeated.
	code, receipt = invokeSignup(t, path, server.URL, args...)
	if code != 4 {
		t.Fatalf("resume exit = %d (%v)", code, receipt)
	}
	again, err := loadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if again.Human.Pacing.Seed != seed {
		t.Fatalf("resume changed the pacing seed: %d != %d", again.Human.Pacing.Seed, seed)
	}
	if again.Warmup["fetched"] != firstWarmup {
		t.Fatalf("warm-up was repeated on resume: %v != %v", again.Warmup["fetched"], firstWarmup)
	}
}

// The warmed browser must arrive at the account page the way a customer does:
// from Apple's own pages, with a click-through referer — not as a cold direct
// hit on the signup URL.
func TestWarmupStartsWithAnAppleJourney(t *testing.T) {
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer assets.Close()
	fixture := &warmupFixture{asset: assets}
	server := httptest.NewServer(fixture.handler(t))
	defer server.Close()

	session, err := newSession(SessionOptions{Base: server.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	session.widgetKey = "test-widget"

	steps := session.runWarmup(context.Background(), nil, 0, true)
	if len(steps) < 4 {
		t.Fatalf("journey warm-up took %d steps: %+v", len(steps), steps)
	}
	want := []string{"journey-home", "journey-product", "journey-support", "landing"}
	for i, name := range want {
		if steps[i].Step != name {
			t.Fatalf("step %d = %s, want %s (%+v)", i, steps[i].Step, name, steps)
		}
	}

	fixture.mu.Lock()
	var landing *http.Request
	for _, r := range fixture.requests {
		if r.URL.Path == "/account" && r.URL.RawQuery == "" {
			landing = r
			break
		}
	}
	fixture.mu.Unlock()
	if landing == nil {
		t.Fatal("landing was never fetched")
	}
	if got := landing.Header.Get("Referer"); !strings.HasSuffix(strings.TrimSuffix(got, "/"), "/support") {
		t.Fatalf("landing referer = %q, want the support page", got)
	}

	// Without the journey the first step is the landing itself.
	session2, _ := newSession(SessionOptions{Base: server.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 10 * time.Second})
	session2.widgetKey = "test-widget"
	plain := session2.runWarmup(context.Background(), nil, 0, false)
	if len(plain) == 0 || plain[0].Step != "landing" {
		t.Fatalf("journey disabled but first step = %+v", plain)
	}
}
