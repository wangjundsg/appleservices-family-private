package icloud

import (
	"strings"
	"testing"
)

func TestStorageErrorReportsNumericAppleCodeWithoutResponseDetails(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"5005", "icloud: storage purchase-store-authentication failed (HTTP 200; Apple code 5005)"},
		{"-5000", "icloud: storage purchase-store-authentication failed (HTTP 200; Apple code -5000)"},
		{"", "icloud: storage purchase-store-authentication failed (HTTP 200)"},
		{"secret-token", "icloud: storage purchase-store-authentication failed (HTTP 200)"},
	} {
		err := &StorageError{Stage: "purchase-store-authentication", HTTPStatus: 200, AppleCode: tc.code, Hint: "private response"}
		if got := err.Error(); got != tc.want {
			t.Fatalf("unexpected safe error: %q", got)
		}
	}
}

func TestResolveStorageEndpointSubstitutesOnlyAppleTemplate(t *testing.T) {
	data := map[string]any{"mme-info": map[string]any{"quota": map[string]any{
		"storageInfoURL": "https://p01-quota.icloud.com/v1/$dsid$/$MMeAuthToken$",
		"storagePlanURL": "https://buy.icloud.com/$apple_id$",
	}}}
	got, err := ResolveStorageEndpoint(data, "storageInfoURL", "a+b@example.com", "123", "tok/one")
	if err != nil || got != "https://p01-quota.icloud.com/v1/123/tok%2Fone" {
		t.Fatalf("endpoint=%q err=%v", got, err)
	}
	data["mme-info"].(map[string]any)["quota"].(map[string]any)["storageInfoURL"] = "https://evil.example/$dsid$"
	if _, err = ResolveStorageEndpoint(data, "storageInfoURL", "a", "1", "t"); err == nil {
		t.Fatal("accepted non-Apple host")
	}
}

func TestParseStorageInfo(t *testing.T) {
	data := []byte(`{"storage_data":{"storage_info_in_bytes":{"commerce_storage_in_bytes":2147483648},"quota_info_in_bytes":{"total_quota":10737418240,"total_used":3221225472,"total_available":7516192768}}}`)
	info, err := parseStorageInfo(data)
	if err != nil || info.TotalBytes != 10737418240 || info.UsedBytes != 3221225472 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestStorageEndpointUsesRootDataclassAndDoesNotGuessMissingEndpoints(t *testing.T) {
	config := map[string]any{"com.apple.Dataclass.Quota": map[string]any{
		"storageInfoURL": "https://p02-quota.icloud.com/quota",
	}}
	got, err := ResolveStorageEndpoint(config, "storageInfoURL", "owner@example.test", "123", "test-token")
	if err != nil || got != "https://p02-quota.icloud.com/quota" {
		t.Fatal("delegate quota address was not preserved")
	}
	if got, err := ResolveStorageEndpoint(config, "storagePlanURL", "owner@example.test", "123", "test-token"); err == nil || got != "" {
		t.Fatal("missing plan endpoint was invented")
	}
}

func TestResolveStorageEndpointFindsNestedQuotaConfig(t *testing.T) {
	data := map[string]any{"service-data": map[string]any{"nested": map[string]any{
		"storageInfoURL": "https://p01-quota.icloud.com/v1/$dsid$/$MMeAuthToken$",
	}}}
	got, err := ResolveStorageEndpoint(data, "storageInfoURL", "a@example.com", "123", "tok")
	if err != nil || got != "https://p01-quota.icloud.com/v1/123/tok" {
		t.Fatalf("endpoint=%q err=%v", got, err)
	}
}

func TestResolveStorageEndpointDefaultQuotaService(t *testing.T) {
	got, err := ResolveStorageEndpoint(map[string]any{}, "storageInfoURL", "a@example.com", "123", "tok")
	if err != nil || got != "https://p01-quota.icloud.com/quotaservice/external/ios/123/storageUsageInfo" {
		t.Fatalf("endpoint=%q err=%v", got, err)
	}
}

func TestResolveStorageEndpointAcceptsNativeUppercasePlaceholders(t *testing.T) {
	data := map[string]any{
		"com.apple.Dataclass.Quota": map[string]any{
			"storageInfoURL": "https://p01-quota.icloud.com/v1/$APPLE_ID$/$DS_PRS_ID$/$UDID$",
		},
	}
	got, err := ResolveStorageEndpoint(data, "storageInfoURL", "a+b@example.com", "123", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a%2Bb%40example.com") || !strings.Contains(got, "/123/123") {
		t.Fatalf("uppercase placeholders were not substituted: %q", got)
	}
}

func TestResolveQuotaUpdateEndpointUsesNativePlaceholders(t *testing.T) {
	data := map[string]any{"mme-info": map[string]any{"quota": map[string]any{
		"quotaUpdateURL": "https://p01-quota.icloud.com/upgrade/$APPLE_ID$/$DS_PRS_ID$/$UDID$",
	}}}
	got, err := ResolveQuotaUpdateEndpoint(data, "a+b@example.com", "123")
	if err != nil || got != "https://p01-quota.icloud.com/upgrade/a%2Bb%40example.com/123/123" {
		t.Fatalf("endpoint=%q err=%v", got, err)
	}
}
