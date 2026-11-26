package nativeadi

import (
	"context"
	"strings"
	"testing"
)

func TestNativeHTTPProxyOverridesBypassWithoutLeakingCredentials(t *testing.T) {
	t.Setenv("APPLESERVICES_NATIVE_HTTP_PROXY", "http://user:private-marker@localhost:8080")
	t.Setenv("HTTPS_PROXY", "socks5h://localhost:1080")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("APPLESERVICES_PASSWORD", "private-password")
	env, err := Environment()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[strings.ToLower(key)] = value
	}
	if values["https_proxy"] != "http://user:private-marker@localhost:8080" || values["http_proxy"] != values["https_proxy"] || values["no_proxy"] != "" {
		t.Fatal("explicit native proxy was bypassed or changed")
	}
	if _, ok := values["appleservices_password"]; ok {
		t.Fatal("password inherited")
	}
	for _, proxy := range []string{"socks5h://user:private-marker@localhost:1080", "http://private-marker", "http://localhost:bad", "http://localhost:8080/path", "http://user-{country}:private-marker@localhost:8080"} {
		t.Setenv("APPLESERVICES_NATIVE_HTTP_PROXY", proxy)
		_, err := Environment()
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("invalid proxy not safely rejected")
		}
	}
}

func TestNativeFailureSurvivesHelperExit(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := &Client{replies: make(chan reply, 1), done: make(chan struct{})}
		c.replies <- reply{Error: "native_tls_handshake_failed"}
		close(c.done)
		_, err := c.receive(context.Background(), 0)
		if err == nil || !strings.Contains(err.Error(), "native_tls_handshake_failed") {
			t.Fatalf("lost failure: %v", err)
		}
	}
}
