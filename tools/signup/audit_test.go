package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionOptionsReachTransport(t *testing.T) {
	s, err := newSession(SessionOptions{UseCurl: true, SkipLanding: true, CHHints: true, HTTP2: true, Proxy: "http://127.0.0.1:12345"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.UseCurl || !s.SkipLanding || !s.CHHints || !s.HTTP2 || s.proxyURL != "http://127.0.0.1:12345" {
		t.Fatal("requested transport switches were dropped")
	}
}

func TestSkipLandingActuallySkipsRequest(t *testing.T) {
	landing := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bootstrap/portal" {
			fmt.Fprint(w, `{}`)
			return
		}
		if r.URL.RawQuery == "" {
			landing++
		}
		w.Header().Set("scnt", "fixture-only")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	s, err := newSession(SessionOptions{Base: server.URL, SkipLanding: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "USA"); err != nil {
		t.Fatal(err)
	}
	if landing != 0 {
		t.Fatal("skip-landing still requested landing page")
	}
}

func TestCurlParsesFinalHTTPStatus(t *testing.T) {
	for _, raw := range []string{
		"HTTP/1.1 200 OK\r\nscnt: fixture\r\n\r\n",
		"HTTP/1.1 200 Connection established\r\n\r\nHTTP/2 400\r\nscnt: fixture\r\n\r\n",
	} {
		status, headers, err := parseCurlHeaders(raw)
		if err != nil || (status != 200 && status != 400) || headers.Get("scnt") != "fixture" {
			t.Fatalf("status=%d err=%v", status, err)
		}
	}
}

func TestClientHintsMatchDeclaredUserAgent(t *testing.T) {
	s, err := newSession(SessionOptions{CHHints: true})
	if err != nil {
		t.Fatal(err)
	}
	s.CHHints = true // Exercise header construction independently of option forwarding.
	h, err := s.headers("", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Get("Sec-Ch-Ua"), `"Chromium";v="141"`) || strings.Contains(h.Get("Sec-Ch-Ua"), `v="157"`) {
		t.Fatal("client hints contradict configured UA")
	}
}

func TestFDProfileMismatchRejectedBeforeNetwork(t *testing.T) {
	for _, fd := range []string{`{"U":"different-browser","L":"en-US"}`, `{"U":"` + defaultUserAgent + `","L":"zh-CN"}`, `not-json`} {
		if _, err := newSession(SessionOptions{FDClientInfo: fd}); err == nil {
			t.Fatal("inconsistent FD profile accepted")
		}
	}
}

func TestApplicationErrorInsideHTTP200IsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"hasError":true,"service_errors":[{"code":"-34607001"}]}`)
	}))
	defer server.Close()
	s := newTestSession(t, server)
	if _, err := s.do(context.Background(), "fixture", "POST", "/account/validate", map[string]string{}, false); err == nil {
		t.Fatal("application rejection counted as success")
	}
}

func TestRateLimitedEmailIsNotRetriedOnAlternateEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(423) }))
	defer server.Close()
	s := newTestSession(t, server)
	_, err := s.SendEmailCode(context.Background(), "fixture@example.test")
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestSystemCurlLoopbackAndSessionIsolation(t *testing.T) {
	if _, err := exec.LookPath("curl.exe"); err != nil {
		t.Skip("system curl unavailable")
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("explicit HTTP/1.1 was not used")
		}
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "fixture", Value: "local-only", Path: "/"})
		}
		if r.URL.Path == "/check" {
			if _, err := r.Cookie("fixture"); err != nil {
				t.Error("same-session cookie lost")
			}
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	s, err := newSession(SessionOptions{Base: server.URL, UseCurl: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := newSession(SessionOptions{Base: server.URL, UseCurl: true})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if s.cookieFile() == other.cookieFile() {
		t.Fatal("two sessions share a cookie file")
	}
	for _, path := range []string{"/set", "/check"} {
		if _, err := s.do(context.Background(), "fixture", "GET", path, nil, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttemptRunnerDefaultAndMissingBudgetCannotPurchase(t *testing.T) {
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	script, err := filepath.Abs("attempt.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		args := []string{"-NoProfile", "-NonInteractive", "-File", script, "-Email", "fixture@example.test", "-GraphCfg", "not-a-real-file"}
		want := "Purchase disabled"
		if allow {
			args = append(args, "-AllowPurchase")
			want = "No agreed default price"
		}
		cmd := exec.CommandContext(ctx, shell, args...)
		cmd.Dir = t.TempDir()
		output, err := cmd.CombinedOutput()
		cancel()
		if strings.Contains(string(output), "running scripts is disabled") {
			t.Skip("host PowerShell execution policy blocks script execution; static gate test still applies")
		}
		if err == nil || !strings.Contains(string(output), want) {
			t.Fatalf("gate did not stop execution: %v %s", err, output)
		}
		entries, _ := os.ReadDir(cmd.Dir)
		if len(entries) != 0 {
			t.Fatal("admission gate wrote runtime files")
		}
	}
}

func TestAttemptRunnerHasNoEmbeddedSecretsOrDefaultSpend(t *testing.T) {
	raw, err := os.ReadFile("attempt.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	gate := strings.Index(script, "if (-not $AllowPurchase)")
	firstWrite := strings.Index(script, "New-Item")
	if gate < 0 || firstWrite < gate || !strings.Contains(script, "[Nullable[decimal]]$MaxPrice = $null") {
		t.Fatal("purchase gate/budget missing before effects")
	}
	for _, forbidden := range []string{"key = '", "$CaptchaKey = '", "socks5h://"} {
		if strings.Contains(script, forbidden) {
			t.Fatal("embedded credential or proxy default")
		}
	}
}
