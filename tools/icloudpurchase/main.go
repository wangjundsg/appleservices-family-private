// icloudpurchase submits only an explicitly confirmed, allowlisted 50 GB
// monthly offer using an already accepted iCloud session, then verifies it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/nativeadi"
	"howett.net/plist"
)

func run() error {
	dir := flag.String("dir", "", "bound account directory")
	acceptedFile := flag.String("accepted", "", "private accepted iCloud checkpoint")
	journalFile := flag.String("journal", "", "new owner-only attempt journal; never replay an existing attempt")
	confirm := flag.Bool("confirm-cn-50gb", false, "owner has explicitly authorized CN 50GB at CNY 6 per month")
	confirmUS := flag.Bool("confirm-us-50gb", false, "owner has explicitly authorized US 50GB at USD 0.99 per month, plus applicable tax")
	cancelAutoRenew := flag.Bool("cancel-auto-renew", false, "disable renewal for the existing iCloud+ subscription; service remains active until expiry")
	storeCheck := flag.Bool("store-check-only", false, "check this Mac's native Store authentication without any purchase")
	nativeStore := flag.Bool("native-store", false, "use this account Mac's authenticated native Store and exact returned confirmation")
	localStoreCode := flag.Bool("local-store-code", false, "generate a code on this account's existing trusted Mac; never request SMS")
	storeRetries := flag.Int("store-auth-retries", 0, "at most 3 routing-error retries during Store authentication; never retry a purchase")
	signedStore := flag.Bool("signed-store", false, "SAP-sign native Store authentication; claude-skill uses the physical Store GUID")
	matchedBag := flag.Bool("matched-bag", false, "diagnose the SAP endpoint and setup pair returned for the same Store bag")
	appStoreProfile := flag.Bool("appstore-profile", false, "diagnose signed App Store authentication using the Apple bag endpoint and account headers")
	claudeSkill := flag.Bool("claude-skill", false, "follow .claude icloud-plus-purchase with physical Store GUID and native plist flow")
	storeSessionOut := flag.String("store-session-out", "", "new owner-only output retaining this account's Store credentials and cookies")
	storeSessionIn := flag.String("store-session-in", "", "private Store session for this account and physical GUID")
	storeCodeFile := flag.String("store-code-file", "", "owner-only file containing the latest live device code; reread on each challenged request")
	giftFile := flag.String("gift-card-file", "", "owner-only file containing one US account-credit gift code; never pass a code on the command line")
	giftCountry := flag.String("gift-card-country", "", "issuing country declared by the card owner; currently US only")
	giftCheck := flag.Bool("gift-card-check-only", false, "read Store billing readiness, balance and gift-code preflight; no redemption or purchase")
	confirmGift := flag.Bool("confirm-gift-card", false, "redeem the supplied account-credit card once, then attempt the separately confirmed 50GB plan")
	flag.Parse()
	giftMode := *giftFile != "" || *giftCountry != "" || *giftCheck || *confirmGift
	if err := validateGiftMode(giftMode, *giftFile, *giftCountry, *giftCheck, *confirmGift, *confirmUS, *confirm, *claudeSkill, *nativeStore, *storeCheck, *cancelAutoRenew); err != nil {
		return err
	}
	if *matchedBag {
		if !*claudeSkill || !*signedStore || *appStoreProfile {
			return fmt.Errorf("matched-bag requires claude-skill and signed-store without appstore-profile")
		}
		os.Setenv("APPLESERVICES_SKILL_MATCHED_BAG", "1")
	}
	if *claudeSkill && !*nativeStore {
		return fmt.Errorf("claude-skill requires native-store")
	}
	if *storeSessionIn != "" && (!*claudeSkill || !*nativeStore) {
		return fmt.Errorf("private Store resume requires the native purchase skill")
	}
	if *storeRetries < 0 || *storeRetries > 3 {
		return fmt.Errorf("Store authentication retries must be between 0 and 3")
	}
	if ((!*confirm && !*confirmUS && !*cancelAutoRenew && !*giftCheck) || (*confirm && *confirmUS)) || *dir == "" || *acceptedFile == "" || *journalFile == "" {
		return fmt.Errorf("explicit purchase confirmation, account, accepted checkpoint and new private journal required")
	}
	if _, err := os.Stat(*journalFile); !os.IsNotExist(err) {
		return fmt.Errorf("attempt journal already exists; inspect the result and quota before any retry")
	}
	username, password := os.Getenv("APPLESERVICES_APPLEID"), os.Getenv("APPLESERVICES_PASSWORD")
	storeCode := os.Getenv("APPLESERVICES_STORE_CODE")
	os.Unsetenv("APPLESERVICES_APPLEID")
	os.Unsetenv("APPLESERVICES_PASSWORD")
	os.Unsetenv("APPLESERVICES_STORE_CODE")
	if *storeCodeFile != "" {
		if !*claudeSkill || *localStoreCode || storeCode != "" || *storeSessionIn != "" {
			return fmt.Errorf("live code file requires purchase skill and cannot combine with another code source or Store resume")
		}
		defer os.Remove(*storeCodeFile)
	}
	if *localStoreCode && (!*nativeStore && !*storeCheck || storeCode != "") {
		return fmt.Errorf("local-store-code requires native Store mode and cannot combine with a supplied code")
	}
	if username == "" || (password == "" && *storeSessionIn == "") {
		return fmt.Errorf("credentials required through environment")
	}
	var saved appleservices.Session
	data, err := os.ReadFile(filepath.Join(*dir, "session.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || !strings.EqualFold(saved.AppleID, username) || saved.DSID == "" {
		return fmt.Errorf("account session binding failed")
	}
	var accepted struct {
		Session     appleservices.Session `json:"session"`
		ADSID       string                `json:"adsid"`
		ServiceData map[string]any        `json:"service-data"`
	}
	data, err = os.ReadFile(*acceptedFile)
	if err != nil || json.Unmarshal(data, &accepted) != nil || !strings.EqualFold(accepted.Session.AppleID, username) || accepted.ADSID != saved.DSID || accepted.Session.DSID == "" || accepted.Session.Tokens["mmeAuthToken"] == "" {
		return fmt.Errorf("accepted iCloud checkpoint account binding failed")
	}
	var device appleservices.Device
	data, err = os.ReadFile(filepath.Join(*dir, "device.json"))
	if err != nil || json.Unmarshal(data, &device) != nil || len(device.Identifier) != 16 || device.SerialNumber == "" {
		return fmt.Errorf("bound device unavailable")
	}
	profile := filepath.Join(*dir, "native-adi")
	var hardware struct {
		Push struct {
			Token []byte `plist:"token"`
		} `plist:"push"`
	}
	data, err = os.ReadFile(filepath.Join(profile, "hw_info.plist"))
	if err != nil {
		return fmt.Errorf("hardware unavailable")
	}
	if _, err = plist.Unmarshal(data, &hardware); err != nil || len(hardware.Push.Token) != 32 {
		return fmt.Errorf("account Mac APS token unavailable")
	}
	library, _ := filepath.Abs(".cache/openbubbles-store/rust_lib_bluebubbles.dll")
	native, err := nativeadi.NewClient(nativeadi.Config{LibraryPath: library, ProfileDir: profile, Python: "python", Identifier: device.Identifier, Serial: device.SerialNumber, PushToken: hardware.Push.Token})
	if err != nil {
		return err
	}
	attemptTimeout := 3 * time.Minute
	if *storeCodeFile != "" {
		attemptTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
	defer cancel()
	if err := native.Start(ctx); err != nil {
		return err
	}
	defer native.Close()
	headers, err := native.Headers()
	if err != nil {
		return err
	}
	token := accepted.Session.Tokens["mmeAuthToken"]
	quota, err := icloud.FetchStorageInfo(ctx, accepted.ServiceData, username, accepted.Session.DSID, accepted.ADSID, token, headers)
	if err != nil {
		return err
	}
	fmt.Printf("pre-purchase quota; total-bytes=%d; paid-bytes=%d\n", quota.TotalBytes, quota.PaidBytes)
	// Do not submit any upgrade while a paid plan is already present.
	if quota.PaidBytes != 0 && !*cancelAutoRenew && !*giftCheck {
		return fmt.Errorf("existing paid capacity detected; purchase not submitted")
	}
	target := icloud.PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 600, Currency: "CNY", Period: "month"}
	expectedStoreTerms := "290399"
	if *confirmUS || *giftCheck {
		target = icloud.PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 99, Currency: "USD", Period: "month"}
		expectedStoreTerms = "290571"
	}
	storageToken := saved.Tokens["com.apple.gs.icloud.storage.buy"]
	var plans *icloud.StoragePlanInfo
	if !*cancelAutoRenew && !*giftCheck {
		plans, err = icloud.FetchStorefrontPlanInfoWithStorageToken(ctx, username, accepted.Session.DSID, accepted.ADSID, token, storageToken, headers)
		if err != nil {
			return err
		}
		if err := icloud.ValidatePurchaseTarget(plans, target); err != nil {
			return err
		}
		fmt.Printf("exact Apple offer verified: 50GB %s %d minor units/month\n", target.Currency, target.PriceMinor)
	}
	journal, err := os.OpenFile(*journalFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create new private attempt journal")
	}
	defer journal.Close()
	enc := json.NewEncoder(journal)
	binding := sha256.Sum256([]byte(username + "\x00" + saved.DSID + "\x00" + fmt.Sprintf("%x", device.Identifier)))
	if err := enc.Encode(map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "binding-sha256": fmt.Sprintf("%x", binding), "stage": "started", "target": target}); err != nil {
		return fmt.Errorf("cannot record purchase intent")
	}
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("cannot preserve purchase intent")
	}
	var commerce *icloud.StoreCommerceSession
	if *storeCheck || *nativeStore {
		if *localStoreCode {
			storeCode, err = native.Code(ctx)
			if err != nil {
				return fmt.Errorf("this account Mac's local verification code is unavailable")
			}
			fmt.Println("same-account local Mac code generated in memory; no SMS requested")
		}
		if *storeSessionIn != "" {
			commerce, err = icloud.LoadPrivateSkillSession(*storeSessionIn, username, accepted.Session.DSID)
		} else if *claudeSkill {
			fmt.Println("repository purchase skill: physical host Store GUID; account Mac retained for iCloud")
			var provider func() (string, error)
			if *localStoreCode {
				provider = func() (string, error) { return native.Code(ctx) }
			} else if storeCode != "" {
				provider = func() (string, error) { return storeCode, nil }
			} else if *storeCodeFile != "" {
				provider = func() (string, error) {
					announced := false
					for {
						info, err := os.Stat(*storeCodeFile)
						if err == nil && info.Size() <= 16 && time.Since(info.ModTime()) <= 90*time.Second {
							data, err := os.ReadFile(*storeCodeFile)
							return strings.TrimSpace(string(data)), err
						}
						if err != nil && !os.IsNotExist(err) {
							return "", fmt.Errorf("cannot read private live code file")
						}
						if !announced {
							fmt.Println("Store challenge retained; awaiting a fresh owner-provided device code")
							announced = true
						}
						select {
						case <-ctx.Done():
							return "", fmt.Errorf("live Store code wait expired")
						case <-time.After(250 * time.Millisecond):
						}
					}
				}
			}
			if *signedStore || *appStoreProfile {
				fmt.Println("signed current-bag Store authentication; physical GUID; same cookie jar")
				commerce, err = icloud.AuthenticateSignedSkillStore(ctx, username, password, headers, *appStoreProfile, provider)
			} else {
				commerce, err = icloud.AuthenticateSkillStore(ctx, username, password, provider)
			}
		} else if *appStoreProfile {
			fmt.Println("native App Store profile; signed exact Apple bag endpoint; account Mac retained")
			commerce, err = icloud.AuthenticateAppStoreForHardware(ctx, username, password, storeCode, headers, nativeadi.SyntheticMACAddress(device.Identifier), *storeRetries)
		} else if *signedStore {
			fmt.Println("native Store SAP signature enabled; account Mac hardware retained")
			commerce, err = icloud.AuthenticateSignedStoreForHardware(ctx, username, password, storeCode, headers, nativeadi.SyntheticMACAddress(device.Identifier), *storeRetries)
		} else {
			commerce, err = icloud.AuthenticateStoreForHardwareWithRetries(ctx, username, password, storeCode, headers, nativeadi.SyntheticMACAddress(device.Identifier), *storeRetries)
		}
		storeCode = ""
		status := map[string]any{"stage": "store-authentication", "purchase-submitted": false, "store-ready": err == nil && commerce.MatchesAccount(accepted.Session.DSID)}
		if err != nil {
			status["error"] = err.Error()
		}
		if err := enc.Encode(status); err != nil {
			return fmt.Errorf("cannot preserve Store check result")
		}
		journal.Sync()
		encoded, _ := json.Marshal(status)
		fmt.Println(string(encoded))
		if err != nil {
			return err
		}
		if !commerce.MatchesAccount(accepted.Session.DSID) {
			return fmt.Errorf("Store account binding failed; purchase not submitted")
		}
		if *storeSessionOut != "" {
			if err := commerce.SavePrivateSkillSession(*storeSessionOut, username); err != nil {
				return err
			}
		}
		if *storeCheck {
			return nil
		}
		if *giftCheck {
			_, _, err := prepareGiftCard(ctx, commerce, username, *giftCountry, *giftFile, enc)
			return err
		}
	}
	// One call only. Never replay an ambiguous purchase response automatically.
	var result *icloud.PurchaseResult
	var purchaseErr error
	if *claudeSkill {
		before, err := commerce.ReadSkillSubscription(ctx)
		if err != nil {
			return err
		}
		if before.Present {
			if *cancelAutoRenew {
				if err := commerce.CancelSkillAutoRenew(ctx, before); err != nil {
					return err
				}
				check, err := commerce.ReadSkillSubscription(ctx)
				if err != nil {
					return err
				}
				encoded, _ := json.Marshal(check)
				fmt.Printf("auto-renew cancellation verified: %s\n", encoded)
				if check.AutoRenew {
					return fmt.Errorf("auto-renew is still enabled")
				}
				return nil
			}
			encoded, _ := json.Marshal(before)
			fmt.Printf("existing iCloud subscription; purchase not submitted; %s\n", encoded)
			return nil
		}
		if *cancelAutoRenew {
			return fmt.Errorf("no active iCloud+ subscription to cancel")
		}
		if *confirmGift {
			if err := redeemGiftCard(ctx, commerce, username, *giftCountry, *giftFile, *dir, enc, journal); err != nil {
				return err
			}
			// Refresh the exact offer after redemption; balances and eligibility can change.
			plans, err = icloud.FetchStorefrontPlanInfoWithStorageToken(ctx, username, accepted.Session.DSID, accepted.ADSID, token, storageToken, headers)
			if err != nil {
				return err
			}
			if err := icloud.ValidatePurchaseTarget(plans, target); err != nil {
				return err
			}
		}
		result, purchaseErr = commerce.SubmitSkillPurchase(ctx, plans, target, expectedStoreTerms)
	} else if *nativeStore {
		result, purchaseErr = commerce.SubmitConfirmedStoragePurchase(ctx, plans, username, password, accepted.Session.DSID, accepted.ADSID, token, storageToken, headers, target)
	} else {
		result, purchaseErr = icloud.SubmitStoragePurchaseAnonymous(ctx, plans, username, password, accepted.Session.DSID, accepted.ADSID, token, storageToken, headers, target)
	}
	password = ""
	// Verify quota even when the returned marker or transport is ambiguous.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer verifyCancel()
	updated, quotaErr := icloud.FetchStorageInfo(verifyCtx, accepted.ServiceData, username, accepted.Session.DSID, accepted.ADSID, token, headers)
	status := map[string]any{"stage": "returned", "purchase-result": result, "quota-read-success": quotaErr == nil}
	var subscription *icloud.SkillSubscriptionInfo
	var subscriptionErr error
	if *claudeSkill {
		subscription, subscriptionErr = commerce.ReadSkillSubscription(verifyCtx)
		status["subscription"] = subscription
		if subscriptionErr != nil {
			status["subscription-error"] = subscriptionErr.Error()
		}
	}
	if purchaseErr != nil {
		status["purchase-error"] = purchaseErr.Error()
		if e, ok := purchaseErr.(*icloud.StorageError); ok && e.AppleCode != "" {
			status["apple-code"] = e.AppleCode
		}
	}
	if quotaErr != nil {
		status["quota-error"] = quotaErr.Error()
	}
	if updated != nil {
		status["total-bytes"], status["paid-bytes"] = updated.TotalBytes, updated.PaidBytes
	}
	completed := purchaseErr == nil && quotaErr == nil && purchaseQuotaVerified(result, updated, target)
	if *claudeSkill {
		completed = completed && subscriptionErr == nil && subscription != nil && subscription.MatchesTarget(target)
	}
	status["purchase-completed-and-quota-verified"] = completed
	if err := enc.Encode(status); err != nil {
		return fmt.Errorf("cannot preserve attempt result; inspect quota before retry")
	}
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("cannot preserve attempt result; inspect quota before retry")
	}
	encoded, _ := json.Marshal(status)
	fmt.Println(string(encoded))
	if !completed {
		return fmt.Errorf("subscription completion not verified; do not repeat this attempt blindly")
	}
	return nil
}

func purchaseQuotaVerified(result *icloud.PurchaseResult, quota *icloud.StorageInfo, target icloud.PurchaseTarget) bool {
	return result != nil && result.State == "completed" && quota != nil && quota.TotalBytes >= target.Bytes && quota.PaidBytes >= target.Bytes
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
