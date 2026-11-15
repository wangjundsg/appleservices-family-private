package appleservices

import (
	"errors"
	"testing"

	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
)

type memoryStore struct {
	device  *Device
	session *Session
	saves   int
	saveErr error
}

func (s *memoryStore) LoadDevice() (*Device, error)   { return s.device, nil }
func (s *memoryStore) SaveDevice(d *Device) error     { s.device = d; return nil }
func (s *memoryStore) LoadSession() (*Session, error) { return s.session, nil }
func (s *memoryStore) SaveSession(v *Session) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.session = v
	s.saves++
	return nil
}

type fakeAnisette struct{}

func (fakeAnisette) Headers() (map[string]string, error) {
	return map[string]string{"X-Mme-Device-Id": "test-device"}, nil
}

type fakeBackend struct {
	results   []*gsa.LoginResult
	loginErr  error
	submitErr error
	calls     int
	requests  []TwoFactorMethod
	submits   []TwoFactorMethod
}

func (b *fakeBackend) Login(_, _ string) (*gsa.LoginResult, error) {
	b.calls++
	if b.loginErr != nil {
		return nil, b.loginErr
	}
	return b.results[b.calls-1], nil
}
func (b *fakeBackend) RequestTrustedDeviceCode(_, _ string) error {
	b.requests = append(b.requests, TrustedDevice)
	return nil
}
func (b *fakeBackend) RequestSMSCode(_, _ string) error {
	b.requests = append(b.requests, SMS)
	return nil
}
func (b *fakeBackend) SubmitTrustedDeviceCode(_, _, _ string) error {
	b.submits = append(b.submits, TrustedDevice)
	return b.submitErr
}
func (b *fakeBackend) SubmitSMSCode(_, _, _ string) error {
	b.submits = append(b.submits, SMS)
	return b.submitErr
}
func (b *fakeBackend) Snapshot(dsid string, _ map[string]string) (gsa.Session, error) {
	return gsa.Session{DSID: dsid, Cookies: map[string][]gsa.CookieKV{
		"https://gsa.apple.com/auth": {{Name: "test-cookie", Value: "test-value"}},
	}}, nil
}

func testResult(twoFactor bool) *gsa.LoginResult {
	return &gsa.LoginResult{NeedsTwoFactor: twoFactor, SessionPayload: map[string]any{
		"adsid": "test-adsid", "GsIdmsToken": "test-idms",
		"t": map[string]any{"com.apple.gs.idms.pet": map[string]any{"token": "test-pet"}},
	}}
}

func testOptions(b *fakeBackend, fetch delegateFetcher) []Option {
	return []Option{WithAnisette(fakeAnisette{}), func(o *options) {
		o.newBackend = func(gsa.AnisetteProvider, *gsa.Session) loginBackend { return b }
		o.fetchDelegate = fetch
	}}
}

func testDelegate(map[string]string, string, string, string) (icloud.DelegateTokens, error) {
	return icloud.DelegateTokens{DSID: "test-dsid", MMEAuthToken: "test-mme"}, nil
}

func TestNoTwoFactorCheckpointsGSABeforeICloudLogin(t *testing.T) {
	s := &memoryStore{}
	b := &fakeBackend{results: []*gsa.LoginResult{testResult(false)}}
	delegates := 0
	fetch := func(h map[string]string, id, adsid, pet string) (icloud.DelegateTokens, error) {
		delegates++
		if id != "owner@example.test" || adsid != "test-adsid" || pet != "test-pet" || h["X-Mme-Device-Id"] == "" {
			t.Fatal("iCloud login did not receive authenticated input")
		}
		return testDelegate(h, id, adsid, pet)
	}
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, fetch)...)
	if err != nil {
		t.Fatal(err)
	}
	if s.saves != 0 {
		t.Fatal("saved session before iCloud accepted login")
	}
	c, err := l.Client()
	if err != nil {
		t.Fatal(err)
	}
	if c.MMEAuthToken == "" || s.saves != 2 || s.session.AppleID != "owner@example.test" || s.session.DeviceIdentityToken != "test-idms" {
		t.Fatal("successful login did not return tokens and save account-bound session")
	}
	if _, err := l.Client(); err != nil {
		t.Fatal(err)
	}
	if delegates != 1 || b.calls != 1 {
		t.Fatal("repeated Client call repeated authentication")
	}
}

func TestClientRetainsStorageBuyServiceToken(t *testing.T) {
	s := &memoryStore{}
	b := &fakeBackend{results: []*gsa.LoginResult{{NeedsTwoFactor: false, SessionPayload: map[string]any{
		"adsid": "test-adsid", "GsIdmsToken": "test-idms",
		"t": map[string]any{
			"com.apple.gs.idms.pet":           map[string]any{"token": "test-pet"},
			"com.apple.gs.icloud.storage.buy": map[string]any{"token": "test-storage-buy"},
		},
	}}}}
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	client, err := l.Client()
	if err != nil {
		t.Fatal(err)
	}
	if client.StorageBuyToken != "test-storage-buy" {
		t.Fatalf("storage commerce token was not retained")
	}
}

func TestResumeLoginUsesBoundSavedSessionsWithoutPasswordHandshake(t *testing.T) {
	t.Setenv("APPLESERVICES_FRESH_GSA", "1")
	store := &memoryStore{session: &Session{AppleID: "owner@example.test", DSID: "gsa-account", DeviceIdentityToken: "saved-idms", Tokens: map[string]string{
		"com.apple.gs.idms.pet":           "pet",
		"com.apple.gs.idms.hb":            "heartbeat",
		"com.apple.gs.appleid.auth":       "account-token",
		"com.apple.gs.icloud.storage.buy": "storage-token",
	}}}
	backend := &fakeBackend{}
	accepted := RestoredICloudSession{DSID: "icloud-account", ADSID: "gsa-account", MMEAuthToken: "mme-token", CloudKitToken: "cloudkit-token", ServiceData: map[string]any{"quota": true}}
	login, err := ResumeLogin("OWNER@example.test", store, accepted, testOptions(backend, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	if backend.calls != 0 || login.NeedsTwoFactor() {
		t.Fatal("passwordless resume performed a password login or requested 2FA")
	}
	client, err := login.Client()
	if err != nil {
		t.Fatal(err)
	}
	if client.DSID != accepted.DSID || client.ADSID != accepted.ADSID || client.MMEAuthToken != accepted.MMEAuthToken || client.StorageBuyToken != "storage-token" {
		t.Fatalf("restored client was not account-bound: %#v", client)
	}
	if err := login.SaveSession(); err != nil {
		t.Fatal(err)
	}
	if store.session.DeviceIdentityToken != "saved-idms" || spdString(login.result.SessionPayload, "GsIdmsToken") != "saved-idms" {
		t.Fatal("passwordless resume dropped the saved device identity token")
	}
}

func TestResumedStorePasswordDoesNotEnableGSAHandshakeOrEnterSession(t *testing.T) {
	store := &memoryStore{session: &Session{AppleID: "owner@example.test", DSID: "gsa-account", Tokens: map[string]string{"com.apple.gs.idms.pet": "pet", "com.apple.gs.idms.hb": "heartbeat", "com.apple.gs.appleid.auth": "account-token"}}}
	backend := &fakeBackend{}
	accepted := RestoredICloudSession{DSID: "icloud-account", ADSID: "gsa-account", MMEAuthToken: "mme-token", ServiceData: map[string]any{}}
	opts := append(testOptions(backend, testDelegate), WithStorePassword("Fixture-store-only123!"))
	login, err := ResumeLogin("owner@example.test", store, accepted, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if backend.calls != 0 || login.creds.Password != "" || login.passwordForStore() != "Fixture-store-only123!" {
		t.Fatal("Store credentials changed resume authentication")
	}
	if err := login.SaveSession(); err != nil {
		t.Fatal(err)
	}
	for _, token := range store.session.Tokens {
		if token == login.passwordForStore() {
			t.Fatal("Store password persisted")
		}
	}
}

func TestResumeLoginWithoutLegacyDeviceIdentityRequiresOneReconnect(t *testing.T) {
	store := &memoryStore{session: &Session{AppleID: "owner@example.test", DSID: "gsa-account", Tokens: map[string]string{
		"com.apple.gs.idms.pet": "pet", "com.apple.gs.idms.hb": "heartbeat", "com.apple.gs.appleid.auth": "account-token",
	}}}
	accepted := RestoredICloudSession{DSID: "icloud-account", ADSID: "gsa-account", MMEAuthToken: "mme-token", ServiceData: map[string]any{"quota": true}}
	login, err := ResumeLogin("owner@example.test", store, accepted, testOptions(&fakeBackend{}, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.AccountDevices(); !errors.Is(err, ErrDeviceIdentityRefreshRequired) {
		t.Fatalf("legacy session did not request a device identity refresh: %v", err)
	}
}

func TestResumeLoginRejectsCrossAccountOrIncompleteState(t *testing.T) {
	complete := map[string]string{"com.apple.gs.idms.pet": "pet", "com.apple.gs.idms.hb": "hb", "com.apple.gs.appleid.auth": "auth"}
	for _, test := range []struct {
		name     string
		session  *Session
		accepted RestoredICloudSession
	}{
		{"wrong-account", &Session{AppleID: "other@example.test", DSID: "gsa", Tokens: complete}, RestoredICloudSession{DSID: "icloud", ADSID: "gsa", MMEAuthToken: "mme", ServiceData: map[string]any{}}},
		{"wrong-adsid", &Session{AppleID: "owner@example.test", DSID: "gsa", Tokens: complete}, RestoredICloudSession{DSID: "icloud", ADSID: "other", MMEAuthToken: "mme", ServiceData: map[string]any{}}},
		{"missing-heartbeat", &Session{AppleID: "owner@example.test", DSID: "gsa", Tokens: map[string]string{"com.apple.gs.idms.pet": "pet", "com.apple.gs.appleid.auth": "auth"}}, RestoredICloudSession{DSID: "icloud", ADSID: "gsa", MMEAuthToken: "mme", ServiceData: map[string]any{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryStore{session: test.session}
			if _, err := ResumeLogin("owner@example.test", store, test.accepted, testOptions(&fakeBackend{}, testDelegate)...); err == nil {
				t.Fatal("unsafe saved session was resumed")
			}
		})
	}
}

func TestCheckpointRequiresAuthenticatedClient(t *testing.T) {
	store := &memoryStore{}
	backend := &fakeBackend{results: []*gsa.LoginResult{testResult(false)}}
	login, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, store, testOptions(backend, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	if login.SaveSession() == nil || store.saves != 0 {
		t.Fatal("checkpoint saved unverified session")
	}
	if _, err = login.Client(); err != nil {
		t.Fatal(err)
	}
	if err = login.SaveSession(); err != nil || store.saves != 3 || store.session.AppleID != "owner@example.test" {
		t.Fatal("active session was not checkpointed")
	}
	store.saveErr = errors.New("storage unavailable")
	if login.SaveSession() == nil {
		t.Fatal("checkpoint swallowed storage failure")
	}
}

func TestCachedSessionMustAuthenticateAndMatchAccount(t *testing.T) {
	for _, account := range []string{"OWNER@example.test", "other@example.test", ""} {
		t.Run(account, func(t *testing.T) {
			s := &memoryStore{session: &Session{AppleID: account, DSID: "old"}}
			b := &fakeBackend{results: []*gsa.LoginResult{testResult(true)}}
			opts := testOptions(b, testDelegate)
			opts = append(opts, func(o *options) {
				o.newBackend = func(_ gsa.AnisetteProvider, cached *gsa.Session) loginBackend {
					if (cached != nil) != (account == "OWNER@example.test") {
						t.Fatal("cached session account binding was not checked")
					}
					return b
				}
			})
			l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if b.calls != 1 || !l.NeedsTwoFactor() {
				t.Fatal("cached session bypassed current authentication")
			}
			if _, err := l.Client(); err == nil {
				t.Fatal("accepted pending two-factor challenge")
			}
		})
	}
}

func TestTwoFactorCompletesWithBothMethods(t *testing.T) {
	for _, method := range []TwoFactorMethod{TrustedDevice, SMS} {
		s := &memoryStore{}
		b := &fakeBackend{results: []*gsa.LoginResult{testResult(true), testResult(false)}}
		l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, testDelegate)...)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.RequestCode(method); err != nil {
			t.Fatal(err)
		}
		if err := l.SubmitCode(method, " 123456\n"); err != nil {
			t.Fatal(err)
		}
		if l.NeedsTwoFactor() || b.calls != 2 || b.requests[0] != method || b.submits[0] != method {
			t.Fatal("two-factor did not reauthenticate using the selected method")
		}
		if _, err := l.Client(); err != nil {
			t.Fatal(err)
		}
		if s.saves != 2 {
			t.Fatal("two-factor login did not checkpoint GSA before saving the accepted iCloud session")
		}
	}
}

func TestTwoFactorCheckpointSurvivesICloudDelegateRejection(t *testing.T) {
	store := &memoryStore{}
	backend := &fakeBackend{results: []*gsa.LoginResult{testResult(true), testResult(false)}}
	fetch := func(map[string]string, string, string, string) (icloud.DelegateTokens, error) {
		return icloud.DelegateTokens{}, errors.New("test delegate rejection")
	}
	login, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, store, testOptions(backend, fetch)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := login.SubmitCode(SMS, "123456"); err != nil {
		t.Fatal(err)
	}
	if store.saves != 1 || store.session == nil || store.session.AppleID != "owner@example.test" {
		t.Fatal("verified GSA session was not checkpointed before iCloud delegate")
	}
	if _, err := login.Client(); err == nil {
		t.Fatal("delegate rejection was reported as success")
	}
	if store.saves != 1 {
		t.Fatal("delegate rejection overwrote or removed the verified GSA checkpoint")
	}
}

func TestFailedVerificationRemainsPending(t *testing.T) {
	b := &fakeBackend{results: []*gsa.LoginResult{testResult(true)}, submitErr: errors.New("test rejection")}
	s := &memoryStore{}
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "12345", "abcdef", "123456"} {
		if err := l.SubmitCode(TrustedDevice, code); err == nil {
			t.Fatal("accepted invalid or rejected code")
		}
	}
	if !l.NeedsTwoFactor() || b.calls != 1 || len(b.submits) != 1 || s.saves != 0 {
		t.Fatal("failed code changed authentication state")
	}
}

func TestICloudRejectionPreservesVerifiedGSASession(t *testing.T) {
	t.Setenv("APPLESERVICES_ALLOW_DEGRADED_ICLOUD", "1")
	s := &memoryStore{}
	b := &fakeBackend{results: []*gsa.LoginResult{testResult(false)}}
	fetch := func(map[string]string, string, string, string) (icloud.DelegateTokens, error) {
		return icloud.DelegateTokens{}, errors.New("test iCloud rejection")
	}
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, fetch)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Client(); err == nil {
		t.Fatal("reported success without iCloud acceptance")
	}
	if s.saves != 1 || s.session == nil || s.session.AppleID != "owner@example.test" {
		t.Fatal("verified GSA session was not preserved after rejected iCloud login")
	}
}

func TestCachedSessionDoesNotHideWrongPassword(t *testing.T) {
	s := &memoryStore{session: &Session{AppleID: "owner@example.test"}}
	b := &fakeBackend{loginErr: errors.New("test wrong password")}
	if _, err := BeginLogin(Credentials{"owner@example.test", "wrong"}, s, testOptions(b, testDelegate)...); err == nil {
		t.Fatal("cached session hid a failed password check")
	}
}

func TestSessionSaveFailureDoesNotReturnSuccess(t *testing.T) {
	s := &memoryStore{saveErr: errors.New("test disk failure")}
	b := &fakeBackend{results: []*gsa.LoginResult{testResult(false)}}
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, testOptions(b, testDelegate)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Client(); err == nil {
		t.Fatal("reported session saved after write failure")
	}
}

type verifiedBackend struct{ *fakeBackend }

func (b *verifiedBackend) VerifiedLoginResult() *gsa.LoginResult {
	if len(b.submits) == 0 {
		return nil
	}
	return testResult(false)
}

func TestIssuedVerificationPETAvoidsAnotherChallenge(t *testing.T) {
	s := &memoryStore{}
	b := &verifiedBackend{&fakeBackend{results: []*gsa.LoginResult{testResult(true)}}}
	opts := testOptions(b.fakeBackend, testDelegate)
	opts = append(opts, func(o *options) {
		o.newBackend = func(gsa.AnisetteProvider, *gsa.Session) loginBackend { return b }
	})
	l, err := BeginLogin(Credentials{"owner@example.test", "test-password"}, s, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SubmitCode(SMS, "123456"); err != nil {
		t.Fatal(err)
	}
	if l.NeedsTwoFactor() || b.calls != 1 {
		t.Fatal("restarted password authentication despite an issued PET")
	}
	if _, err := l.Client(); err != nil {
		t.Fatal(err)
	}
	if s.saves != 2 {
		t.Fatal("verified header session was not checkpointed before completing iCloud login")
	}
}
