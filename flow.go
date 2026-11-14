package appleservices

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Laky-64/appleservices/anisette"
	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
)

type loginBackend interface {
	Login(username, password string) (*gsa.LoginResult, error)
	RequestTrustedDeviceCode(dsid, idmsToken string) error
	SubmitTrustedDeviceCode(dsid, idmsToken, code string) error
	RequestSMSCode(dsid, idmsToken string) error
	SubmitSMSCode(dsid, idmsToken, code string) error
	Snapshot(dsid string, tokens map[string]string) (gsa.Session, error)
}

type backendFactory func(gsa.AnisetteProvider, *gsa.Session) loginBackend
type delegateFetcher func(map[string]string, string, string, string) (icloud.DelegateTokens, error)

func defaultBackend(p gsa.AnisetteProvider, sess *gsa.Session) loginBackend {
	if sess != nil {
		return gsa.NewClientFromSession(p, *sess)
	}
	return gsa.NewClient(p)
}

// Login handles Apple ID authentication and any pending two-factor challenge.
// Call Client to complete iCloud login; no account data is fetched.
type Login struct {
	creds           Credentials
	store           Store
	backend         loginBackend
	anisette        gsa.AnisetteProvider
	stateful        *anisette.Provider
	fetchDelegate   delegateFetcher
	needsTwoFactor  bool
	result          *gsa.LoginResult
	adsid           string
	idmsToken       string
	client          *Client
	serialNumber    string
	storeCommerce   *icloud.StoreCommerceSession
	gsaCheckpointed bool
}

func prepareLogin(creds Credentials, store Store, requirePassword bool, opts ...Option) (*Login, *Session, error) {
	if store == nil {
		return nil, nil, errors.New("appleservices: store is required")
	}
	creds.AppleID = strings.TrimSpace(creds.AppleID)
	if creds.AppleID == "" || requirePassword && creds.Password == "" {
		return nil, nil, errors.New("appleservices: Apple ID and password are required")
	}
	o := options{newBackend: defaultBackend, fetchDelegate: icloud.FetchDelegateTokens}
	for _, opt := range opts {
		opt(&o)
	}
	l := &Login{creds: creds, store: store, fetchDelegate: o.fetchDelegate}
	if o.anisette != nil {
		l.anisette = o.anisette
	} else {
		dev, err := store.LoadDevice()
		if err != nil {
			return nil, nil, fmt.Errorf("appleservices: load device: %w", err)
		}
		st := anisette.NewState()
		if dev != nil {
			l.serialNumber = dev.SerialNumber
		}
		if dev != nil && len(dev.Identifier) == 16 {
			st = anisette.State{Identifier: dev.Identifier, AdiPB: dev.ProvisioningBlob}
		}
		l.stateful = anisette.NewProviderFromState(st, o.servers)
		l.anisette = l.stateful
	}
	sess, err := store.LoadSession()
	if err != nil {
		return nil, nil, fmt.Errorf("appleservices: load session: %w", err)
	}
	var gsess *gsa.Session
	// Older, unbound sessions and sessions belonging to another account are ignored.
	if sess != nil && strings.EqualFold(sess.AppleID, creds.AppleID) && (!requirePassword || strings.TrimSpace(os.Getenv("APPLESERVICES_FRESH_GSA")) != "1") {
		s := toGSASession(*sess)
		gsess = &s
	}
	if o.serialNumber != "" {
		l.serialNumber = o.serialNumber
	}
	// Mac v2/NAC validation is bound to the native provider's own serial and
	// hardware profile. Do not overwrite its SRL header with the Windows
	// account-registration serial during this explicit diagnostic path.
	if l.serialNumber != "" && strings.TrimSpace(os.Getenv("APPLESERVICES_NAC_VALIDATION_FILE")) == "" {
		l.anisette = identityAnisette{provider: l.anisette, serial: l.serialNumber}
	}
	l.backend = o.newBackend(l.anisette, gsess)
	if l.serialNumber != "" {
		// Keep the worker's AppleIDSettings client identity selectable for
		// controlled comparisons. A delegate refusal alone does not identify
		// which GSA client header caused it.
		if os.Getenv("APPLESERVICES_NO_NATIVE_DEVICE_AUTH") == "" {
			if backend, ok := l.backend.(interface{ EnableNativeDeviceAuthentication() }); ok {
				backend.EnableNativeDeviceAuthentication()
			}
		}
	}
	if len(o.pushToken) > 0 {
		backend, ok := l.backend.(interface{ SetPushToken([]byte) error })
		if !ok {
			return nil, nil, errors.New("appleservices: backend cannot bind an APS token")
		}
		if err := backend.SetPushToken(o.pushToken); err != nil {
			return nil, nil, err
		}
	}
	return l, sess, nil
}

func BeginLogin(creds Credentials, store Store, opts ...Option) (*Login, error) {
	l, _, err := prepareLogin(creds, store, true, opts...)
	if err != nil {
		return nil, err
	}
	// Cached cookies may avoid a new 2FA prompt, but never establish login on their own.
	if err := l.authenticate(); err != nil {
		return nil, err
	}
	return l, nil
}

// ResumeLogin restores a passwordless local worker from an account-bound GSA
// session and an Apple-accepted iCloud checkpoint. It performs no password
// handshake and never weakens the binding between the account, GSA ADSID and
// iCloud delegate ADSID.
func ResumeLogin(appleID string, store Store, accepted RestoredICloudSession, opts ...Option) (*Login, error) {
	l, saved, err := prepareLogin(Credentials{AppleID: appleID}, store, false, opts...)
	if err != nil {
		return nil, err
	}
	if saved == nil || !strings.EqualFold(saved.AppleID, l.creds.AppleID) || saved.DSID == "" || saved.DSID != accepted.ADSID {
		return nil, errors.New("appleservices: saved account session binding is invalid")
	}
	pet := saved.Tokens["com.apple.gs.idms.pet"]
	heartbeat := saved.Tokens["com.apple.gs.idms.hb"]
	accountToken := saved.Tokens["com.apple.gs.appleid.auth"]
	if pet == "" || heartbeat == "" || accountToken == "" || accepted.DSID == "" || accepted.MMEAuthToken == "" || accepted.ServiceData == nil {
		return nil, errors.New("appleservices: saved login session is incomplete")
	}
	services := make(map[string]any, len(saved.Tokens))
	for service, token := range saved.Tokens {
		if token != "" {
			services[service] = map[string]any{"token": token}
		}
	}
	payload := map[string]any{"adsid": saved.DSID, "t": services}
	if saved.DeviceIdentityToken != "" {
		payload["GsIdmsToken"] = saved.DeviceIdentityToken
	}
	l.result = &gsa.LoginResult{SessionPayload: payload}
	l.adsid, l.needsTwoFactor, l.gsaCheckpointed = saved.DSID, false, true
	l.client = &Client{DSID: accepted.DSID, ADSID: accepted.ADSID, MMEAuthToken: accepted.MMEAuthToken,
		StorageBuyToken: saved.Tokens["com.apple.gs.icloud.storage.buy"], CloudKitToken: accepted.CloudKitToken, serviceData: accepted.ServiceData}
	return l, nil
}

func (l *Login) persistDevice() error {
	if l.stateful == nil {
		return nil
	}
	st := l.stateful.State()
	if err := l.store.SaveDevice(&Device{Identifier: st.Identifier, ProvisioningBlob: st.AdiPB, SerialNumber: l.serialNumber}); err != nil {
		return fmt.Errorf("appleservices: save device: %w", err)
	}
	return nil
}

func (l *Login) authenticate() error {
	res, loginErr := l.backend.Login(l.creds.AppleID, l.creds.Password)
	// Provisioning can change even when account authentication fails.
	saveErr := l.persistDevice()
	if loginErr != nil {
		return errors.Join(fmt.Errorf("appleservices: login: %w", loginErr), saveErr)
	}
	if saveErr != nil {
		return saveErr
	}
	if res == nil || res.SessionPayload == nil {
		return errors.New("appleservices: login returned no session payload")
	}
	adsid := spdString(res.SessionPayload, "adsid")
	idmsToken := spdString(res.SessionPayload, "GsIdmsToken")
	if adsid == "" {
		return errors.New("appleservices: login returned no account ID")
	}
	if res.NeedsTwoFactor && idmsToken == "" {
		return errors.New("appleservices: two-factor challenge missing GsIdmsToken")
	}
	l.result = res
	l.adsid, l.idmsToken = adsid, idmsToken
	l.needsTwoFactor = res.NeedsTwoFactor
	l.client = nil
	l.storeCommerce = nil
	return nil
}

func (l *Login) NeedsTwoFactor() bool { return l.needsTwoFactor }

// BeginAccountValidation starts a fresh password-authenticated challenge for
// an already configured account. It is used by the dashboard when Apple asks
// for a device code again; it never opens or drives the official client.
func (l *Login) BeginAccountValidation() error {
	if l.creds.Password == "" {
		return errors.New("appleservices: password is required to start a new account validation challenge")
	}
	res, err := l.backend.Login(l.creds.AppleID, l.creds.Password)
	if err != nil {
		return fmt.Errorf("appleservices: validation login: %w", err)
	}
	if res == nil || res.SessionPayload == nil {
		return errors.New("appleservices: validation returned no session")
	}
	adsid, idms := spdString(res.SessionPayload, "adsid"), spdString(res.SessionPayload, "GsIdmsToken")
	if adsid == "" || adsid != l.adsid || idms == "" {
		return errors.New("appleservices: validation returned a different account")
	}
	l.result, l.adsid, l.idmsToken = res, adsid, idms
	l.needsTwoFactor, l.client = res.NeedsTwoFactor, nil
	l.storeCommerce = nil
	if !l.needsTwoFactor {
		return nil
	}
	return l.RequestCode(TrustedDevice)
}

type TwoFactorMethod int

const (
	TrustedDevice TwoFactorMethod = iota
	SMS
)

func (l *Login) RequestCode(method TwoFactorMethod) error {
	if !l.needsTwoFactor {
		return errors.New("appleservices: no two-factor challenge is pending")
	}
	switch method {
	case TrustedDevice:
		return l.backend.RequestTrustedDeviceCode(l.adsid, l.idmsToken)
	case SMS:
		return l.backend.RequestSMSCode(l.adsid, l.idmsToken)
	default:
		return fmt.Errorf("appleservices: unknown two-factor method %d", method)
	}
}

func (l *Login) SubmitCode(method TwoFactorMethod, code string) error {
	if !l.needsTwoFactor {
		return errors.New("appleservices: no two-factor challenge is pending")
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("appleservices: verification code must contain six digits")
	}
	var err error
	switch method {
	case TrustedDevice:
		err = l.backend.SubmitTrustedDeviceCode(l.adsid, l.idmsToken, code)
	case SMS:
		err = l.backend.SubmitSMSCode(l.adsid, l.idmsToken, code)
	default:
		return fmt.Errorf("appleservices: unknown two-factor method %d", method)
	}
	if err != nil {
		return fmt.Errorf("appleservices: submit code: %w", err)
	}
	if verified, ok := l.backend.(interface{ VerifiedLoginResult() *gsa.LoginResult }); ok {
		if result := verified.VerifiedLoginResult(); result != nil {
			if result.NeedsTwoFactor || spdString(result.SessionPayload, "adsid") != l.adsid {
				return errors.New("appleservices: verification returned an invalid account session")
			}
			if _, err := icloud.PETFromSPD(result.SessionPayload); err != nil {
				return err
			}
			l.result = result
			l.needsTwoFactor = false
			l.storeCommerce = nil
			if err := l.persistDevice(); err != nil {
				return err
			}
			return l.checkpointVerifiedGSA()
		}
	}
	if err := l.authenticate(); err != nil {
		return err
	}
	if l.needsTwoFactor {
		return errors.New("appleservices: still prompted for 2FA after submitting a code")
	}
	return l.checkpointVerifiedGSA()
}

// checkpointVerifiedGSA preserves a just-verified two-factor session before
// the independent iCloud delegate exchange. A delegate refusal must not throw
// away an accepted SMS/device challenge and force another verification code.
func (l *Login) checkpointVerifiedGSA() error {
	if l.gsaCheckpointed {
		return nil
	}
	if l.needsTwoFactor || l.result == nil || l.result.SessionPayload == nil {
		return errors.New("appleservices: verified GSA session is unavailable")
	}
	gsess, err := l.backend.Snapshot(l.adsid, spdTokens(l.result.SessionPayload))
	if err != nil {
		return fmt.Errorf("appleservices: snapshot verified GSA session: %w", err)
	}
	sess := fromGSASession(gsess)
	sess.AppleID = l.creds.AppleID
	sess.DeviceIdentityToken = spdString(l.result.SessionPayload, "GsIdmsToken")
	if err := l.store.SaveSession(&sess); err != nil {
		return fmt.Errorf("appleservices: save verified GSA session: %w", err)
	}
	l.gsaCheckpointed = true
	return nil
}

// Client is the result of a completed iCloud login. Tokens are sensitive.
// This login-only client has no data retrieval or modification methods.
type Client struct {
	DSID         string
	ADSID        string
	MMEAuthToken string
	// StorageBuyToken is the GSA service token scoped to iCloud+ storage
	// commerce (com.apple.gs.icloud.storage.buy). It is distinct from the
	// iCloud mobileme token and is required by Apple's Store purchase flow.
	StorageBuyToken string
	CloudKitToken   string
	serviceData     map[string]any
}

// ResumeCheckpoint contains credentials; persist it only in the account's private directory.
func (c *Client) ResumeCheckpoint() RestoredICloudSession {
	return RestoredICloudSession{DSID: c.DSID, ADSID: c.ADSID, MMEAuthToken: c.MMEAuthToken,
		CloudKitToken: c.CloudKitToken, ServiceData: c.serviceData}
}

// Client exchanges the authenticated Apple ID session for iCloud tokens.
// A session is saved only after iCloud accepts the login, with or without 2FA.
func (l *Login) Client() (*Client, error) {
	if l.needsTwoFactor {
		return nil, errors.New("appleservices: two-factor authentication required; submit a code first")
	}
	if l.client != nil {
		return l.client, nil
	}
	pet, err := icloud.PETFromSPD(l.result.SessionPayload)
	if err != nil {
		return nil, err
	}
	headers, headerErr := l.anisette.Headers()
	saveErr := l.persistDevice()
	if headerErr != nil {
		return nil, errors.Join(fmt.Errorf("appleservices: anisette headers: %w", headerErr), saveErr)
	}
	if saveErr != nil {
		return nil, saveErr
	}
	// iCloud delegate acceptance is independent from account authentication.
	// Persist the verified account-bound GSA state first so a delegate/terms
	// refusal cannot consume a successful password or 2FA challenge.
	if err := l.checkpointVerifiedGSA(); err != nil {
		return nil, err
	}
	dt, err := l.fetchDelegate(headers, l.creds.AppleID, l.adsid, pet)
	if err != nil {
		// Account-side registration can succeed without iCloud service data.
		// Never turn that partial result into an authenticated iCloud client,
		// including when an older launcher enables the retired degraded flag.
		return nil, fmt.Errorf("appleservices: iCloud login: %w", err)
	}
	if dt.DSID == "" || dt.MMEAuthToken == "" {
		return nil, errors.New("appleservices: iCloud login returned no account ID or auth token")
	}
	gsess, err := l.backend.Snapshot(l.adsid, spdTokens(l.result.SessionPayload))
	if err != nil {
		return nil, fmt.Errorf("appleservices: snapshot session: %w", err)
	}
	sess := fromGSASession(gsess)
	sess.AppleID = l.creds.AppleID
	sess.DeviceIdentityToken = spdString(l.result.SessionPayload, "GsIdmsToken")
	if err := l.store.SaveSession(&sess); err != nil {
		return nil, fmt.Errorf("appleservices: save session: %w", err)
	}
	l.client = &Client{DSID: dt.DSID, ADSID: l.adsid, MMEAuthToken: dt.MMEAuthToken,
		StorageBuyToken: spdToken(l.result.SessionPayload, "com.apple.gs.icloud.storage.buy"),
		CloudKitToken:   dt.CloudKitToken, serviceData: dt.ServiceData}
	return l.client, nil
}

// SaveSession checkpoints refreshed cookies for an already authenticated login.
// Service tokens and account credentials remain in memory.
func (l *Login) SaveSession() error {
	if l.client == nil || l.needsTwoFactor {
		return errors.New("appleservices: complete authentication before saving the session")
	}
	gsess, err := l.backend.Snapshot(l.adsid, spdTokens(l.result.SessionPayload))
	if err != nil {
		return fmt.Errorf("appleservices: snapshot session: %w", err)
	}
	sess := fromGSASession(gsess)
	sess.AppleID = l.creds.AppleID
	sess.DeviceIdentityToken = spdString(l.result.SessionPayload, "GsIdmsToken")
	if err := l.store.SaveSession(&sess); err != nil {
		return fmt.Errorf("appleservices: save session: %w", err)
	}
	return nil
}

func toGSASession(s Session) gsa.Session {
	cookies := map[string][]gsa.CookieKV{}
	for _, c := range s.Cookies {
		cookies[c.URL] = append(cookies[c.URL], gsa.CookieKV{Name: c.Name, Value: c.Value})
	}
	return gsa.Session{DSID: s.DSID, Cookies: cookies, Tokens: s.Tokens}
}

func fromGSASession(g gsa.Session) Session {
	var cookies []Cookie
	for url, kvs := range g.Cookies {
		for _, kv := range kvs {
			cookies = append(cookies, Cookie{URL: url, Name: kv.Name, Value: kv.Value})
		}
	}
	return Session{DSID: g.DSID, Cookies: cookies, Tokens: g.Tokens}
}

func spdString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func spdToken(m map[string]any, service string) string {
	tokens, _ := m["t"].(map[string]any)
	entry, _ := tokens[service].(map[string]any)
	token, _ := entry["token"].(string)
	return token
}

func spdTokens(m map[string]any) map[string]string {
	tokens, _ := m["t"].(map[string]any)
	result := map[string]string{}
	for service, raw := range tokens {
		entry, _ := raw.(map[string]any)
		if token, _ := entry["token"].(string); token != "" {
			result[service] = token
		}
	}
	return result
}
