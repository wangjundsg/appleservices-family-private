package appleservices

import (
	"context"
	"errors"
	"time"

	"github.com/Laky-64/appleservices/icloud"
)

type StorageInfo = icloud.StorageInfo
type StoragePlanInfo = icloud.StoragePlanInfo
type PurchaseTarget = icloud.PurchaseTarget
type PurchaseResult = icloud.PurchaseResult

func (l *Login) passwordForStore() string {
	if l.storePassword != "" {
		return l.storePassword
	}
	return l.creds.Password
}

func (l *Login) PreviewStoragePurchase(ctx context.Context) (*icloud.StoreFormInfo, error) {
	info, err := l.StoragePlanInfo(ctx)
	if err != nil {
		return nil, err
	}
	if !l.storeCommerce.MatchesAccount(l.client.DSID) {
		if _, err := l.CheckStoragePurchaseSession(ctx, ""); err != nil {
			return nil, err
		}
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: Store anisette generation failed")
	}
	return l.storeCommerce.PreviewStoragePurchase(ctx, info, l.creds.AppleID, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers, PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 600, Currency: "CNY", Period: "month"})
}

// CheckStoragePurchaseSession signs in to the Store, preserving its separate
// commerce credentials in memory. It never calls a purchase endpoint.
func (l *Login) CheckStoragePurchaseSession(ctx context.Context, code string) (*icloud.StoreAuthenticationInfo, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before Store authentication")
	}
	if l.storeCommerce.MatchesAccount(l.client.DSID) && code == "" {
		return &icloud.StoreAuthenticationInfo{Ready: true}, nil
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: Store anisette generation failed")
	}
	session, err := icloud.AuthenticateStore(ctx, l.creds.AppleID, l.passwordForStore(), code, headers)
	if err != nil {
		return nil, err
	}
	l.storeCommerce = session
	return &icloud.StoreAuthenticationInfo{Ready: true}, nil
}

// CheckSkillStoragePurchaseSession follows one matched Apple Store bag and
// retains the same cookie session across password and Store 2FA. The provider
// is called only after Apple requests a verification code.
func (l *Login) CheckSkillStoragePurchaseSession(ctx context.Context, codeProvider func() (string, error)) (*icloud.StoreAuthenticationInfo, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before Store authentication")
	}
	if l.storeCommerce.MatchesAccount(l.client.DSID) {
		return &icloud.StoreAuthenticationInfo{Ready: true}, nil
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: Store anisette generation failed")
	}
	session, err := icloud.AuthenticateMatchedSkillStore(ctx, l.creds.AppleID, l.passwordForStore(), headers, codeProvider)
	if err != nil {
		return nil, err
	}
	if !session.MatchesAccount(l.client.DSID) {
		return nil, errors.New("appleservices: Store account binding failed")
	}
	l.storeCommerce = session
	return &icloud.StoreAuthenticationInfo{Ready: true}, nil
}

// SubmitSkillStoragePurchase submits one exact allowlisted offer, then
// verifies both Apple's subscription detail and a fresh paid quota read.
func (l *Login) SubmitSkillStoragePurchase(ctx context.Context, target PurchaseTarget, expectedTerms string) (*PurchaseResult, error) {
	if !l.storeCommerce.MatchesAccount(l.client.DSID) {
		return nil, errors.New("appleservices: Store session is unavailable")
	}
	before, err := l.storeCommerce.ReadSkillSubscription(ctx)
	if err != nil {
		return nil, err
	}
	if before.Present {
		return nil, errors.New("appleservices: an iCloud+ subscription is already active")
	}
	plans, err := l.StoragePlanInfo(ctx)
	if err != nil {
		return nil, err
	}
	result, err := l.storeCommerce.SubmitSkillPurchase(ctx, plans, target, expectedTerms)
	if err != nil || result == nil || result.State != "completed" {
		return result, err
	}
	subscription, err := l.storeCommerce.ReadSkillSubscription(ctx)
	if err != nil || !subscription.MatchesTarget(target) {
		return result, errors.New("appleservices: purchase returned but subscription verification is incomplete")
	}
	for attempt := 0; attempt < 5; attempt++ {
		quota, quotaErr := l.StorageInfo(ctx)
		if quotaErr == nil && quota.TotalBytes >= target.Bytes && quota.PaidBytes >= target.Bytes {
			result.QuotaVerified = true
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, errors.New("appleservices: purchase returned but quota verification timed out")
		case <-time.After(2 * time.Second):
		}
	}
	return result, errors.New("appleservices: purchase returned but paid quota is not active")
}

// StorageInfo continues from this project's verified delegate session. It does
// not inspect or alter the official client's account cache or windows.
func (l *Login) StorageInfo(ctx context.Context) (*StorageInfo, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before requesting storage")
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: storage anisette generation failed")
	}
	c := l.client
	// A successful delegate login can return a mobileme token before its
	// quota configuration is populated. Refresh the delegate once in that
	// case so a stale service-data snapshot does not permanently block quota
	// reads.
	if !icloud.HasStorageInfoEndpoint(c.serviceData) {
		var settings map[string]any
		if fetchedSettings, settingsErr := icloud.FetchAccountSettings(ctx, c.DSID, c.ADSID, c.MMEAuthToken, headers); settingsErr == nil {
			settings = fetchedSettings
		}
		pet, petErr := icloud.PETFromSPD(l.result.SessionPayload)
		if petErr == nil && l.fetchDelegate != nil {
			if refreshed, refreshErr := l.fetchDelegate(headers, l.creds.AppleID, l.adsid, pet); refreshErr == nil {
				if refreshed.DSID != "" {
					c.DSID = refreshed.DSID
				}
				if refreshed.MMEAuthToken != "" {
					c.MMEAuthToken = refreshed.MMEAuthToken
				}
				c.CloudKitToken, c.serviceData = refreshed.CloudKitToken, refreshed.ServiceData
			}
		}
		if settings != nil {
			if c.serviceData == nil {
				c.serviceData = map[string]any{}
			}
			c.serviceData["account-settings"] = settings
		}
	}
	return icloud.FetchStorageInfo(ctx, c.serviceData, l.creds.AppleID, c.DSID, c.ADSID, c.MMEAuthToken, headers)
}

// StoragePlanInfo returns Apple's read-only storage offer response. It does
// not start a purchase or alter the account.
func (l *Login) StoragePlanInfo(ctx context.Context) (*icloud.StoragePlanInfo, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before requesting storage plans")
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: storage plan anisette generation failed")
	}
	if !icloud.HasStoragePlanEndpoint(l.client.serviceData) {
		pet, petErr := icloud.PETFromSPD(l.result.SessionPayload)
		if petErr == nil && l.fetchDelegate != nil {
			if refreshed, refreshErr := l.fetchDelegate(headers, l.creds.AppleID, l.adsid, pet); refreshErr == nil {
				if refreshed.DSID != "" {
					l.client.DSID = refreshed.DSID
				}
				if refreshed.MMEAuthToken != "" {
					l.client.MMEAuthToken = refreshed.MMEAuthToken
				}
				l.client.CloudKitToken, l.client.serviceData = refreshed.CloudKitToken, refreshed.ServiceData
			}
		}
	}
	if icloud.HasQuotaUpdateEndpoint(l.client.serviceData) {
		info, err := icloud.FetchQuotaUpdatePlanInfo(ctx, l.client.serviceData, l.creds.AppleID, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers)
		if err == nil {
			return info, nil
		}
		// The native upgrade URL now serves the modern quota web UI, whose
		// offers come from its own JSON API rather than HTML form controls.
		// That page cannot drive a purchase, so fall back to the classic
		// storefront offer page, which carries the buy form itself. Every
		// other failure is returned as-is.
		var decode *icloud.StorageError
		if !errors.As(err, &decode) || decode.Stage != "plan-decode" {
			return nil, err
		}
		return icloud.FetchStorefrontPlanInfoWithStorageToken(ctx, l.creds.AppleID, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers)
	}
	info, err := icloud.FetchStoragePlanInfo(ctx, l.client.serviceData, l.creds.AppleID, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, headers)
	if err == nil {
		return info, nil
	}
	var failure *icloud.StorageError
	if errors.As(err, &failure) && failure.Stage == "configuration" {
		return icloud.FetchStorefrontPlanInfoWithStorageToken(ctx, l.creds.AppleID, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers)
	}
	return nil, err
}

// prepareStoragePurchase verifies quota and the exact offer before any
// purchase-side session exists. A non-nil result means the purchase must not
// continue (an already paid plan).
func (l *Login) prepareStoragePurchase(ctx context.Context, target PurchaseTarget) (*icloud.StoragePlanInfo, map[string]string, *PurchaseResult, error) {
	// Quota lookup may refresh the delegate token. Do this before reading
	// offers, whose private source URL includes that token.
	quota, err := l.StorageInfo(ctx)
	if err != nil {
		return nil, nil, nil, &icloud.StorageError{Stage: "purchase-current-quota"}
	}
	if quota.PaidBytes >= target.Bytes {
		return nil, nil, &PurchaseResult{State: "existing-paid-plan", HTTPStatus: 200, QuotaVerified: true}, nil
	}
	info, err := l.StoragePlanInfo(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := icloud.ValidatePurchaseTarget(info, target); err != nil {
		return nil, nil, nil, err
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, nil, nil, errors.New("appleservices: purchase anisette generation failed")
	}
	return info, headers, nil, nil
}

// SubmitStoragePurchase validates the exact Apple offer and runs the page
// chain the way the official Windows client does: a fresh cookie jar with no
// native Store session, browser-shaped requests, and the volumeAuthenticate
// form creating the commerce session. A returned handoff is not treated as a
// completed subscription.
func (l *Login) SubmitStoragePurchase(ctx context.Context, target PurchaseTarget) (*PurchaseResult, error) {
	info, headers, existing, err := l.prepareStoragePurchase(ctx, target)
	if err != nil || existing != nil {
		return existing, err
	}
	result, err := icloud.SubmitStoragePurchaseAnonymous(ctx, info, l.creds.AppleID, l.passwordForStore(), l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers, target)
	if err != nil {
		return nil, err
	}
	return l.verifyStoragePurchase(ctx, result, target)
}

// SubmitStoragePurchaseWithStoreSession is the earlier variant that performs
// a native SAP Store login first. The official client never does this for a
// storage purchase; the path is kept for comparison and compatibility with
// storefronts that require a pre-existing commerce session.
func (l *Login) SubmitStoragePurchaseWithStoreSession(ctx context.Context, target PurchaseTarget) (*PurchaseResult, error) {
	info, headers, existing, err := l.prepareStoragePurchase(ctx, target)
	if err != nil || existing != nil {
		return existing, err
	}
	if !l.storeCommerce.MatchesAccount(l.client.DSID) {
		if _, err := l.CheckStoragePurchaseSession(ctx, ""); err != nil {
			return nil, err
		}
	}
	result, err := l.storeCommerce.SubmitStoragePurchase(ctx, info, l.creds.AppleID, l.passwordForStore(), l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, l.client.StorageBuyToken, headers, target)
	if err != nil {
		return nil, err
	}
	return l.verifyStoragePurchase(ctx, result, target)
}

// ProbeStorageEndpoints runs read-only probes against the storage and
// subscription endpoints the store bag advertises. It reports only status,
// content type and a value-free body shape. withStoreSession additionally
// probes the same endpoints with the native Store session credentials.
func (l *Login) ProbeStorageEndpoints(ctx context.Context, withStoreSession bool) ([]icloud.StorageEndpointProbe, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before probing storage endpoints")
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: probe anisette generation failed")
	}
	var commerce *icloud.StoreCommerceSession
	if withStoreSession {
		if !l.storeCommerce.MatchesAccount(l.client.DSID) {
			if _, err := l.CheckStoragePurchaseSession(ctx, ""); err != nil {
				return nil, err
			}
		}
		commerce = l.storeCommerce
	}
	return icloud.ProbeStorageEndpoints(ctx, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, headers, commerce), nil
}

// ProbeCustom runs caller-supplied endpoint probes (from the worker's
// probe-urls.json) with the session's own credentials.
func (l *Login) ProbeCustom(ctx context.Context, specs []icloud.ProbeSpec, withStoreSession bool) ([]icloud.StorageEndpointProbe, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before probing storage endpoints")
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: probe anisette generation failed")
	}
	var commerce *icloud.StoreCommerceSession
	if withStoreSession {
		if !l.storeCommerce.MatchesAccount(l.client.DSID) {
			if _, err := l.CheckStoragePurchaseSession(ctx, ""); err != nil {
				return nil, err
			}
		}
		commerce = l.storeCommerce
	}
	return icloud.ProbeCustomEndpoints(ctx, specs, l.client.DSID, l.client.ADSID, l.client.MMEAuthToken, headers, commerce), nil
}

// verifyStoragePurchase re-reads the quota after a completed purchase; a
// success marker without verified paid capacity is not reported as completed.
func (l *Login) verifyStoragePurchase(ctx context.Context, result *PurchaseResult, target PurchaseTarget) (*PurchaseResult, error) {
	if result == nil || result.State != "completed" {
		return result, nil
	}
	quota, err := l.StorageInfo(ctx)
	result.QuotaVerified = err == nil && quota.TotalBytes >= target.Bytes && quota.PaidBytes >= target.Bytes
	if !result.QuotaVerified {
		result.State = "verification-pending"
	}
	return result, nil
}

// SubmitStorageBuyDirect posts the mainland China 50 GB monthly purchase with
// the native store session alone. It exists for accounts whose iCloud
// delegate Apple refuses: the quota and plan lookups are unavailable there,
// but the store purchase does not require them.
func (l *Login) SubmitStorageBuyDirect(ctx context.Context) (*PurchaseResult, error) {
	if l.needsTwoFactor || l.client == nil {
		return nil, errors.New("appleservices: complete iCloud login before purchasing")
	}
	headers, err := l.anisette.Headers()
	if err != nil {
		return nil, errors.New("appleservices: purchase anisette generation failed")
	}
	return icloud.SubmitStorageBuyDirect(ctx, l.client.ADSID, headers, l.storeCommerce)
}
