package dashboard

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Laky-64/appleservices/internal/control"
)

func TestCNRenewalMustBeExplicitlyOff(t *testing.T) {
	for _, sub := range []*control.SubscriptionInfo{nil, {}, {Present: true, CN50GB: true, AutoRenew: true}, {Present: true, CN50GB: false}} {
		flow := registrationFamilyFlow{account: &Account{}, input: handoffInput{Family: registrationFamily{Role: "organizer", Region: "CN", AcceptSubscription: true}}, worker: func(action string) control.Result {
			if action != "storage-cancel-auto-renew" {
				t.Fatal(action)
			}
			return control.Result{OK: true, Subscription: sub}
		}}
		if result := flow.run("subscription-cancel", ""); result["ok"] == true {
			t.Fatal("unproven cancellation accepted")
		}
	}
}

func TestMemberResumeRequiresExactOrganizerBinding(t *testing.T) {
	mother, child := &Account{ID: "mother"}, &Account{ID: "child", Dir: "child-dir"}
	for _, sameFamily := range []bool{false, true} {
		flow := registrationFamilyFlow{account: child, organizer: mother, input: handoffInput{Family: registrationFamily{Role: "member", Organizer: "mother", Region: "CN"}}}
		flow.worker = func(string) control.Result { t.Fatal("child reached commerce"); return control.Result{} }
		flow.family = func(account *Account, args ...string) (*control.FamilyInfo, error) {
			if len(args) > 0 {
				if account != mother || args[0] != "-verify-pool-member-dir" || args[1] != child.Dir {
					t.Fatal("resume replayed membership mutation")
				}
				if !sameFamily {
					return nil, errors.New("different family")
				}
			}
			result := testFamilyReady(account == mother)
			result.Members = 6
			return result, nil
		}
		for _, step := range []string{"family-join", "family-join-storage", "family-verify"} {
			result := flow.run(step, "")
			if (result["ok"] == true) != sameFamily {
				t.Fatalf("%s: %v", step, result)
			}
		}
	}
}

func TestCNFlowEndpointRejectsPurchaseAndUnknownAccount(t *testing.T) {
	s := registerTestServer(t)
	s.accounts = append(s.accounts, &Account{ID: "fixture", Dir: t.TempDir()})
	for _, body := range []string{`{"account":"missing","role":"organizer","step":"subscription-buy"}`, `{"role":"member","step":"activate"}`, `{"account":"fixture","role":"organizer","step":"subscription-buy"}`} {
		w := registerTestCall(s, "POST", "/api/cn-family", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.OK {
			t.Fatal("unexpected action accepted")
		}
	}
}

func TestPaymentWaitOnlyAllowsAttendedCommerceActions(t *testing.T) {
	for _, action := range []string{"family-add-self", "storage-skill-purchase-us", "storage-buy", "storage-purchase", "deny", "refresh"} {
		if cnPaymentAction(action) {
			t.Fatal("payment wait allowed unrelated action", action)
		}
	}
	if !cnPaymentAction("storage-skill-purchase-cn") || !cnPaymentAction("storage-info") {
		t.Fatal("payment path unavailable")
	}
}
