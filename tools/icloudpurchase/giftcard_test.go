package main

import (
	"github.com/Laky-64/appleservices/icloud"
	"testing"
)

func TestGiftModeRequiresSeparateRedeemAndPurchaseIntent(t *testing.T) {
	for _, tc := range []struct {
		name                                                           string
		check, redeem, us, cn, skill, native, storeCheck, cancel, want bool
	}{
		{name: "readonly", check: true, skill: true, native: true, want: true},
		{name: "confirmed-chain", redeem: true, us: true, skill: true, native: true, want: true},
		{name: "missing-purchase-intent", redeem: true, skill: true, native: true},
		{name: "ambiguous-mode", check: true, redeem: true, us: true, skill: true, native: true},
		{name: "wrong-plan", redeem: true, cn: true, skill: true, native: true},
		{name: "cancel-conflict", redeem: true, us: true, skill: true, native: true, cancel: true},
		{name: "store-check-conflict", check: true, skill: true, native: true, storeCheck: true},
		{name: "no-native-session", check: true, skill: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGiftMode(true, "private-code.txt", "US", tc.check, tc.redeem, tc.us, tc.cn, tc.skill, tc.native, tc.storeCheck, tc.cancel)
			if (err == nil) != tc.want {
				t.Fatalf("intent validation: %v", err)
			}
		})
	}
}

func TestGiftIntentCannotBeRecreatedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	if err := recordGiftIntent(dir, "synthetic-card-digest"); err != nil {
		t.Fatal(err)
	}
	if err := recordGiftIntent(dir, "synthetic-card-digest"); err == nil {
		t.Fatal("new invocation can repeat the same card")
	}
}

func TestPurchaseReceiptRequiresActualPaidQuota(t *testing.T) {
	target := icloud.PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024}
	result := &icloud.PurchaseResult{State: "completed"}
	for _, quota := range []*icloud.StorageInfo{nil, {}, {TotalBytes: target.Bytes}, {TotalBytes: 5 * 1024 * 1024 * 1024, PaidBytes: target.Bytes}} {
		if purchaseQuotaVerified(result, quota, target) {
			t.Fatal("purchase receipt masked missing paid quota")
		}
	}
	if !purchaseQuotaVerified(result, &icloud.StorageInfo{TotalBytes: target.Bytes, PaidBytes: target.Bytes}, target) {
		t.Fatal("confirmed capacity rejected")
	}
}
