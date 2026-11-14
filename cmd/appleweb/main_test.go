package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Laky-64/appleservices/internal/appleweb"
)

type fakeFlow struct{ terms, accepts int }

func (f *fakeFlow) Submit2FA(context.Context, string, string, json.RawMessage) (appleweb.ICloudSession, error) {
	return appleweb.ICloudSession{}, nil
}
func (f *fakeFlow) Terms(context.Context, string) (appleweb.ICloudTerms, error) {
	f.terms++
	return appleweb.ICloudTerms{Version: "12"}, nil
}
func (f *fakeFlow) AcceptTerms(context.Context, string, string, bool) (appleweb.ICloudSession, error) {
	f.accepts++
	return appleweb.ICloudSession{AppleID: "owner@example.test"}, nil
}
func TestTermsWithoutTwoFactorAreCompleted(t *testing.T) {
	f := &fakeFlow{}
	_, err := finish(context.Background(), f, appleweb.AuthStartResult{NeedsTerms: true}, true, func() (string, error) { t.Fatal("unexpected code request"); return "", nil }, func(string) {})
	if err != nil || f.terms != 1 || f.accepts != 1 {
		t.Fatal("terms-only flow did not finish")
	}
}
func TestTermsRequireExplicitConsent(t *testing.T) {
	f := &fakeFlow{}
	_, err := finish(context.Background(), f, appleweb.AuthStartResult{NeedsTerms: true}, false, nil, func(string) {})
	if err == nil || f.accepts != 0 {
		t.Fatal("terms accepted without consent")
	}
}
func TestWrongAccountCannotReplaceSavedSession(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(p, []byte("old-session"), 0600); err != nil {
		t.Fatal(err)
	}
	err := saveSession(p, appleweb.ICloudSession{AppleID: "other@example.test", DSID: "test", Cookies: []appleweb.SessionCookie{{Name: "test", Value: "test"}}}, "owner@example.test")
	data, _ := os.ReadFile(p)
	if err == nil || string(data) != "old-session" {
		t.Fatal("old session overwritten")
	}
}
