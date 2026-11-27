package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/appleweb"
)

func TestActivateSavedSessionRequiresBoundVerifiedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store := fileStore{dir: dir}
	called := 0
	activate := func(context.Context, string, string, string) error { called++; return nil }
	for _, session := range []*appleservices.Session{
		{AppleID: "other@example.test", DSID: "1", Tokens: map[string]string{"com.apple.gs.idms.pet": "fixture"}},
		{AppleID: "owner@example.test", DSID: "1"},
		{AppleID: "owner@example.test", DSID: "1", Tokens: map[string]string{"com.apple.gs.idms.pet": "fixture"}},
	} {
		if err := store.SaveSession(session); err != nil {
			t.Fatal(err)
		}
		err := activateSavedSession(context.Background(), dir, "owner@example.test", "python", activate)
		valid := session.AppleID == "owner@example.test" && session.Tokens["com.apple.gs.idms.pet"] != ""
		if (err == nil) != valid {
			t.Fatalf("unexpected checkpoint admission: %v", err)
		}
	}
	if called != 1 {
		t.Fatal("unverified or mismatched session reached activation")
	}
}

type repairFixture struct {
	terms, codes        int
	startErr, verifyErr error
	needsTerms          bool
}

func (f *repairFixture) Start(context.Context, appleweb.AuthFlow, string, string, string, string, string) (appleweb.AuthStartResult, error) {
	return appleweb.AuthStartResult{PendingID: "one-challenge", Needs2FA: true, NeedsTerms: f.needsTerms}, f.startErr
}
func fixtureWebSession() appleweb.ICloudSession {
	return appleweb.ICloudSession{AppleID: "owner@example.test", DSID: "fixture", Cookies: []appleweb.SessionCookie{{Name: "fixture", Value: "fixture"}}}
}
func (f *repairFixture) Submit2FA(_ context.Context, id, code string, _ json.RawMessage) (appleweb.ICloudSession, error) {
	f.codes++
	if id != "one-challenge" || code != "123456" {
		return appleweb.ICloudSession{}, errors.New("wrong challenge")
	}
	return fixtureWebSession(), f.verifyErr
}
func (f *repairFixture) Terms(context.Context, string) (appleweb.ICloudTerms, error) {
	return appleweb.ICloudTerms{Version: "42"}, nil
}
func (f *repairFixture) AcceptTerms(_ context.Context, id, v string, accepted bool) (appleweb.ICloudSession, error) {
	f.terms++
	if id != "one-challenge" || v != "42" || !accepted {
		return appleweb.ICloudSession{}, errors.New("wrong consent")
	}
	return fixtureWebSession(), nil
}
func TestAutomaticRepairUsesLiveCodeAndExplicitTerms(t *testing.T) {
	for _, tc := range []struct {
		name         string
		f            repairFixture
		wantErr      bool
		terms, codes int
	}{
		{"terms", repairFixture{needsTerms: true}, false, 1, 1},
		{"no terms", repairFixture{}, false, 0, 1},
		{"unknown repair", repairFixture{startErr: errors.New("private upstream response")}, true, 0, 0},
		{"refused code", repairFixture{needsTerms: true, verifyErr: errors.New("private upstream response")}, true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := repairAccount(context.Background(), &tc.f, "owner@example.test", "fixture", func(verify func(string) error) error { return verify("123456") })
			if (err != nil) != tc.wantErr || tc.f.terms != tc.terms || tc.f.codes != tc.codes {
				t.Fatalf("unexpected result: %v terms=%d codes=%d", err, tc.f.terms, tc.f.codes)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("leaked upstream details")
			}
		})
	}
}

func TestAutomaticActivationRequiresTermsReceiptAndQuota(t *testing.T) {
	for _, scenario := range []string{"already accepted", "terms", "unknown refusal", "ambiguous acceptance", "missing quota", "changed plan"} {
		t.Run(scenario, func(t *testing.T) {
			work := t.TempDir()
			exchanges, accepts := 0, 0
			run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				has := func(flag string) bool {
					for _, arg := range args {
						if arg == flag {
							return true
						}
					}
					return false
				}
				if has("-delegate-only") {
					exchanges++
					if scenario == "unknown refusal" {
						return []byte("HTTP 400 reason-code=3005"), errors.New("refused")
					}
					if exchanges == 1 && scenario != "already accepted" {
						return []byte("ErrorID=MOBILEME_TERMS_OF_SERVICE_UPDATE"), errors.New("terms")
					}
					if exchanges == 2 && !has("-terms-accepted") {
						t.Fatal("missing confirmed terms")
					}
					output := "delegate accepted; mme-token-present=true"
					if scenario != "missing quota" {
						output += "\nquota accepted;"
					}
					return []byte(output), nil
				}
				if has("-accept-terms") {
					accepts++
					if !strings.Contains(strings.Join(args, " "), "CN:42:") {
						t.Fatal("did not use actual current tuple")
					}
					if scenario != "ambiguous acceptance" {
						if err := os.WriteFile(filepath.Join(work, "terms.html.update.json"), []byte(`{"success":true}`), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return nil, errors.New("later delegate failure is distinct from terms acknowledgement")
				}
				if scenario == "changed plan" {
					return nil, errors.New("setup failed")
				}
				return []byte(`{"terms-plan":true,"region":"CN","icloud-version":"42","media-version":""}`), nil
			}
			err := activateWithNAC(context.Background(), run, "helper", "account", work)
			success := scenario == "already accepted" || scenario == "terms"
			if (err == nil) != success {
				t.Fatalf("unexpected result: %v", err)
			}
			if accepts > 1 || exchanges > 2 {
				t.Fatal("replayed a consequential request")
			}
			if (scenario == "ambiguous acceptance" || scenario == "unknown refusal" || scenario == "changed plan") && exchanges != 1 {
				t.Fatal("continued after unconfirmed step")
			}
		})
	}
}

func TestActivationRuntimeDoesNotCopyOtherHardware(t *testing.T) {
	source, destination := t.TempDir(), filepath.Join(t.TempDir(), "runtime")
	for _, name := range []string{"nac.py", "data.plist", "initialize-response.plist"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyActivationRuntime(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "nac.py")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "data.plist")); !os.IsNotExist(err) {
		t.Fatal("copied another hardware profile")
	}
}
