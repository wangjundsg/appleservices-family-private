package main

// Round-4 review checks: a service failure must not look like an address
// verdict, receipts must not echo provider text, and a migration must not skip
// the full validation.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

// preflightStatus returns a fixture that answers preflight with the given status.
func preflightStatus(t *testing.T, status int) *httptest.Server {
	t.Helper()
	base := deviceFixture(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/validation/appleid" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"fixture-provider-marker"}`))
			return
		}
		base.ServeHTTP(w, r)
	}))
}

func TestServiceFailureIsNotAnAddressVerdict(t *testing.T) {
	for _, status := range []int{429, 503} {
		srv := preflightStatus(t, status)
		code, receipt := invokeSignup(t, filepath.Join(t.TempDir(), "task.json"), srv.URL, "-stage", "preflight")
		srv.Close()
		if code == 0 {
			t.Fatalf("HTTP %d must not pass preflight", status)
		}
		reason, _ := receipt["reason"].(string)
		switch reason {
		case "mailbox_in_use", "mailbox_rejected", "mailbox_not_confirmed_available":
			t.Fatalf("HTTP %d was reported as an address verdict (%q)", status, reason)
		}
		if reason != "preflight_unavailable" {
			t.Fatalf("HTTP %d reason = %q, want preflight_unavailable", status, reason)
		}
	}
}

func TestTakenAddressIsDistinctFromRejected(t *testing.T) {
	// used=true must be reported as mailbox_in_use, not as a generic failure.
	fixture := newFlowFixture("held@example.test")
	fixture.phase = "created"
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()
	code, receipt := invokeSignup(t, filepath.Join(t.TempDir(), "task.json"), srv.URL, "-stage", "preflight")
	if code == 0 {
		t.Fatalf("a used address must not pass preflight: %v", receipt)
	}
	if reason, _ := receipt["reason"].(string); reason != "mailbox_in_use" {
		t.Fatalf("reason = %q, want mailbox_in_use", reason)
	}
}

type echoErrorTransport struct{ marker string }

func (e echoErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 400,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(e.marker)),
		Request:    r,
	}, nil
}

func TestPublicReceiptNeverEchoesProviderText(t *testing.T) {
	const marker = "fixture-secret-marker"
	prior := http.DefaultTransport
	http.DefaultTransport = echoErrorTransport{marker: marker}
	defer func() { http.DefaultTransport = prior }()

	srv := httptest.NewServer(deviceFixture(t))
	defer srv.Close()
	_, receipt := invokeSignup(t, filepath.Join(t.TempDir(), "task.json"), srv.URL,
		"-stage", "create", "-phone", "4155550134", "-captcha-key", "offline-dummy")
	raw, _ := json.Marshal(receipt)
	if strings.Contains(string(raw), marker) {
		t.Fatalf("provider text leaked into the receipt: %s", raw)
	}
	for _, key := range []string{"reason", "error"} {
		value, _ := receipt[key].(string)
		if value != "" && !publicReasons[value] && value != "stage_unconfirmed" {
			t.Fatalf("receipt carried a non-whitelisted code %q", value)
		}
	}
}

func TestMigrationRevalidatesTheWholeProfile(t *testing.T) {
	profile := profileForEngineTest(t, fingerprint.KindChrome)
	profile.Engine.Name = "HelloChrome_Auto"
	profile.SchemaVersion = 999
	profile.Signature = fingerprint.Signature(profile)

	st := newProfileState("broken@example.com")
	st.Profile = &profile
	st.ProfileMode = "new"
	_, _, err := installProfile(st, newProfileSession(t), profileConfig{Mode: "new", MigrateLegacy: true}, "", time.Second)
	if err == nil {
		t.Fatal("a migration must not carry a broken archive (schema 999) through")
	}
	if st.Profile != nil && st.Profile.Engine.Name != "HelloChrome_Auto" {
		t.Fatal("the broken profile was migrated and persisted")
	}
}
