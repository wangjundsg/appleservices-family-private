package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Laky-64/appleservices/icloud"
)

func validateGiftMode(enabled bool, file, country string, check, confirm, us, cn, skill, native, storeCheck, cancel bool) error {
	if !enabled {
		return nil
	}
	if file == "" || country != "US" || check == confirm || !skill || !native || storeCheck || cancel || cn || (!check && !us) {
		return fmt.Errorf("gift card requires native purchase skill, a US card file, and either check-only or gift-card plus US 50GB confirmation")
	}
	return nil
}

func prepareGiftCard(ctx context.Context, commerce *icloud.StoreCommerceSession, appleID, country, path string, enc *json.Encoder) (*icloud.GiftCardAttempt, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read private gift card file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil || len(data) > 128 {
		return nil, "", fmt.Errorf("invalid gift card file")
	}
	code := strings.ToUpper(strings.TrimSpace(string(data)))
	a, err := commerce.PrepareGiftCard(ctx, appleID, country, code)
	if err != nil {
		return nil, "", err
	}
	check := a.Check()
	if err := enc.Encode(map[string]any{"stage": "gift-card-preflight", "check": check}); err != nil {
		return nil, "", fmt.Errorf("cannot preserve gift card preflight")
	}
	printed, _ := json.Marshal(check)
	fmt.Printf("gift card preflight: %s\n", printed)
	if len(check.MissingFields) != 0 {
		return nil, "", fmt.Errorf("existing billing profile is incomplete; supply missing fields before redemption")
	}
	digest := sha256.Sum256([]byte(strings.ToLower(appleID) + "\x00" + code))
	return a, fmt.Sprintf("%x", digest), nil
}

func redeemGiftCard(ctx context.Context, commerce *icloud.StoreCommerceSession, appleID, country, path, accountDir string, enc *json.Encoder, journal *os.File) error {
	a, digest, err := prepareGiftCard(ctx, commerce, appleID, country, path, enc)
	if err != nil {
		return err
	}
	// Unlike an arbitrary per-run journal, this guard survives restarting the
	// command with a new journal. Keep it after both success and unknown outcomes.
	if err := recordGiftIntent(accountDir, digest); err != nil {
		return err
	}
	if err := enc.Encode(map[string]string{"stage": "gift-card-submission-intent"}); err != nil {
		return fmt.Errorf("cannot preserve gift card journal")
	}
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("cannot preserve gift card journal")
	}
	result, submitErr := a.Submit(ctx)
	if err := enc.Encode(map[string]any{"stage": "gift-card-returned", "result": result, "verified": submitErr == nil}); err != nil {
		return fmt.Errorf("cannot preserve gift card result; do not repeat redemption")
	}
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("cannot preserve gift card result; do not repeat redemption")
	}
	if submitErr != nil {
		return submitErr
	}
	if result == nil || result.State != "credited" || result.BalanceMinor < 99 {
		return fmt.Errorf("gift credit cannot fund the base US 50GB offer; purchase not submitted")
	}
	fmt.Println("gift card credit verified; continuing to the explicitly confirmed US 50GB offer")
	return nil
}

func recordGiftIntent(accountDir, digest string) error {
	guardDir := filepath.Join(accountDir, "gift-card-attempts")
	if err := os.MkdirAll(guardDir, 0700); err != nil {
		return fmt.Errorf("cannot create private gift card intent directory")
	}
	guard, err := os.OpenFile(filepath.Join(guardDir, digest+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("gift card attempt exists or cannot be recorded; inspect balance and previous result before continuing")
	}
	defer guard.Close()
	if _, err := guard.WriteString("{\"stage\":\"submission-intent\"}\n"); err != nil {
		return fmt.Errorf("cannot preserve gift card intent")
	}
	if err := guard.Sync(); err != nil {
		return fmt.Errorf("cannot preserve gift card intent")
	}
	return nil
}
