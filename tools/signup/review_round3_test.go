package main

// Round-3 review checks: an old profile must be re-checked on restore, and a
// legacy preset alias must never be accepted (or silently replaced) on its own.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/engine"
	"github.com/Laky-64/appleservices/internal/fingerprint"
)

func profileForEngineTest(t *testing.T, kind fingerprint.Kind) fingerprint.Profile {
	t.Helper()
	p, err := fingerprint.Select(fingerprint.Options{
		Kind: kind, Locale: "en-US", Timezone: "America/New_York",
		Region: fingerprint.Region{Country: "US", Source: "explicit"},
		Engine: engine.Capabilities(),
		Seed:   7,
	})
	if err != nil {
		t.Fatalf("select %s: %v", kind, err)
	}
	return p
}

// oldSafariProfile manufactures exactly what an older build could persist:
// Safari 15.3 riding the 16.0 preset.
func oldSafariProfile(t *testing.T) fingerprint.Profile {
	t.Helper()
	p, err := fingerprint.Select(fingerprint.Options{
		Kind: fingerprint.KindMacSafari, Locale: "en-US", Timezone: "America/New_York",
		Region: fingerprint.Region{Country: "US", Source: "explicit"},
		Engine: fingerprint.Capability{
			Name:       "utls",
			Hellos:     []string{"HelloSafari_16_0"},
			MaxBrowser: map[fingerprint.Kind]int{fingerprint.KindMacSafari: 18},
		},
		Seed: 1,
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	p.BrowserVersion = "15.3"
	p.Major = 15
	p.Signature = fingerprint.Signature(p)
	return p
}

func TestRestoredProfileIsRevalidatedForVersion(t *testing.T) {
	profile := oldSafariProfile(t)
	st := newProfileState("old@example.com")
	st.Profile = &profile
	st.ProfileMode = "new"

	session := newProfileSession(t)
	if _, _, err := installProfile(st, session, profileConfig{Mode: "new"}, "", time.Second); err == nil {
		t.Fatal("a saved Safari 15.3 profile riding the 16.0 preset must be refused on resume")
	}
}

func TestLegacyAliasIsNotAcceptedSilently(t *testing.T) {
	profile := profileForEngineTest(t, fingerprint.KindChrome)
	profile.Engine.Name = "HelloChrome_Auto"
	profile.Signature = fingerprint.Signature(profile)

	st := newProfileState("alias@example.com")
	st.Profile = &profile
	st.ProfileMode = "new"
	if _, _, err := installProfile(st, newProfileSession(t), profileConfig{Mode: "new"}, "", time.Second); err == nil {
		t.Fatal("an unversioned alias must be refused, not silently used")
	}
}

func TestLegacyAliasMigratesOnlyOnRequest(t *testing.T) {
	profile := profileForEngineTest(t, fingerprint.KindChrome)
	profile.Engine.Name = "HelloChrome_Auto"
	profile.Signature = fingerprint.Signature(profile)

	st := newProfileState("alias@example.com")
	st.Profile = &profile
	st.ProfileMode = "new"
	restored, engineInfo, err := installProfile(st, newProfileSession(t), profileConfig{Mode: "new", MigrateLegacy: true}, "", time.Second)
	if err != nil {
		t.Fatalf("explicit migration failed: %v", err)
	}
	want := fmt.Sprintf("HelloChrome_%d", restored.Major)
	if restored.Engine.Name != want {
		t.Fatalf("migrated to %s, want %s", restored.Engine.Name, want)
	}
	if engineInfo["migratedTo"] != want {
		t.Fatalf("receipt does not record the migration: %v", engineInfo)
	}
	if st.Profile == nil || st.Profile.Engine.Name != want {
		t.Fatal("the migrated profile was not persisted into the task")
	}
	if err := fingerprint.Validate(*st.Profile, engine.Capabilities()); err != nil {
		t.Fatalf("migrated profile does not validate: %v", err)
	}
}

func TestLegacyAliasMigrationRefusesVersionMismatch(t *testing.T) {
	profile := profileForEngineTest(t, fingerprint.KindChrome)
	profile.BrowserVersion = "131"
	profile.Major = 131
	profile.Engine.Name = "HelloChrome_Auto"
	profile.Signature = fingerprint.Signature(profile)

	cap := engine.Capabilities()
	cap.Hellos = []string{"HelloChrome_133"}
	cap.HelloVersions = map[string]string{"HelloChrome_133": "133"}
	if _, err := fingerprint.MigrateLegacyProfile(profile, cap); err == nil {
		t.Fatal("migration must refuse when no preset matches the profile version")
	}
}

// A reserved address (used=true, no creation receipt of ours) must never be
// reported as an account this task produced.
func TestUsedAddressWithoutReceiptIsNotOurAccount(t *testing.T) {
	fixture := newFlowFixture("held@example.test")
	// Pretend the address is already held by an unfinished registration.
	fixture.phase = "created"
	srv := httptest.NewServer(fixture.handler())
	defer srv.Close()

	st := newProfileState("held@example.test")
	st.Stage = "preflight"
	session, err := newSession(SessionOptions{Base: srv.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	w := &workflow{state: st, session: session, path: filepath.Join(t.TempDir(), "t.json")}
	err = w.verify(context.Background())
	if err == nil {
		t.Fatal("used=true without a creation receipt must not pass verify")
	}
	if !strings.Contains(err.Error(), "final_creation_receipt_required") {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Created {
		t.Fatal("verify must not mark the task created")
	}
}
