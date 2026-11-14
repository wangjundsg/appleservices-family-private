package main

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/Laky-64/appleservices"
)

func TestHelpAndInputValidationDoNotAuthenticate(t *testing.T) {
	t.Setenv("APPLESERVICES_APPLEID", "")
	t.Setenv("APPLESERVICES_PASSWORD", "")
	var output bytes.Buffer
	if err := run([]string{"-help"}, strings.NewReader(""), &output, &output); err != flag.ErrHelp {
		t.Fatalf("help returned %v", err)
	}
	for _, args := range [][]string{nil, {"-twofactor", "unknown"}, {"-dir", ""}, {"unexpected"}} {
		if err := run(args, strings.NewReader(""), &output, &output); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestFileSessionRoundTripAndLogout(t *testing.T) {
	s := fileStore{dir: t.TempDir()}
	sess := &appleservices.Session{AppleID: "owner@example.test", DSID: "test-dsid", Cookies: []appleservices.Cookie{
		{URL: "https://gsa.apple.com/auth", Name: "test-cookie", Value: "test-value"},
	}}
	if err := s.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSession()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AppleID != sess.AppleID || len(loaded.Cookies) != 1 {
		t.Fatal("lost account binding or cookies")
	}
	if err := s.SaveDevice(&appleservices.Device{Identifier: make([]byte, 16)}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"-dir", s.dir, "-logout"}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.sessionPath()); !os.IsNotExist(err) {
		t.Fatal("logout did not remove local session")
	}
	if _, err := os.Stat(s.devicePath()); err != nil {
		t.Fatal("logout removed device identity")
	}
	if strings.Contains(output.String(), "test-value") {
		t.Fatal("printed session credential")
	}
}

func TestHelpDoesNotExposeEnvironmentCredentials(t *testing.T) {
	t.Setenv("APPLESERVICES_APPLEID", "private@example.test")
	t.Setenv("APPLESERVICES_PASSWORD", "test-private-password")
	var output bytes.Buffer
	if err := run([]string{"-help"}, strings.NewReader(""), &output, &output); err != flag.ErrHelp {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private@example.test") || strings.Contains(output.String(), "test-private-password") {
		t.Fatal("help exposed environment credentials")
	}
}

type fakeTwoFactorLogin struct {
	requests  []appleservices.TwoFactorMethod
	submitted appleservices.TwoFactorMethod
	code      string
}

func (l *fakeTwoFactorLogin) RequestCode(m appleservices.TwoFactorMethod) error {
	l.requests = append(l.requests, m)
	return nil
}
func (l *fakeTwoFactorLogin) SubmitCode(m appleservices.TwoFactorMethod, code string) error {
	l.submitted, l.code = m, code
	return nil
}

func TestVerificationResendAndMethodSwitchUseSameLogin(t *testing.T) {
	l := &fakeTwoFactorLogin{}
	var output bytes.Buffer
	if err := completeTwoFactor(l, appleservices.TrustedDevice, strings.NewReader("resend\nsms\n123456\n"), &output); err != nil {
		t.Fatal(err)
	}
	if len(l.requests) != 3 || l.requests[0] != appleservices.TrustedDevice || l.requests[1] != appleservices.TrustedDevice || l.requests[2] != appleservices.SMS {
		t.Fatal("resend changed method or SMS was not requested")
	}
	if l.submitted != appleservices.SMS || l.code != "123456" {
		t.Fatal("code was not submitted with the selected verification method")
	}
	if strings.Contains(output.String(), "123456") {
		t.Fatal("printed verification code")
	}
}

func TestAcceptedCheckpointIsBoundAndPrivate(t *testing.T) {
	s := fileStore{dir: t.TempDir()}
	if err := s.saveAccepted("owner@example.test", appleservices.RestoredICloudSession{}); err == nil {
		t.Fatal("accepted an incomplete delegate result")
	}
	checkpoint := appleservices.RestoredICloudSession{DSID: "test-dsid", ADSID: "test-adsid", MMEAuthToken: "test-token", ServiceData: map[string]any{"test": true}}
	if err := s.saveAccepted("owner@example.test", checkpoint); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.dir + "/icloud-accepted.json")
	if err != nil || !bytes.Contains(data, []byte(`"adsid":"test-adsid"`)) || !bytes.Contains(data, []byte(`"AppleID":"owner@example.test"`)) {
		t.Fatal("lost checkpoint account binding")
	}
}
