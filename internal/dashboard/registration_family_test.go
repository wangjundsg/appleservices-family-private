package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/control"
)

func TestFamilyHandoffCannotSkipBoundJoinReceipt(t *testing.T) {
	s := registerTestServer(t)
	folder := registerFixture(t, s, pipelineReceipt{Stage: "family", Status: "running"})
	_ = writeRegisterJSON(filepath.Join(folder, "input.json"), handoffInput{Handoff: "dashboard", Email: "member@example.test", Family: registrationFamily{Role: "member", Organizer: "mother"}})
	w := registerTestCall(s, "POST", "/api/register/"+testRegisterID+"/handoff", `{"action":"family-join-storage"}`)
	if !strings.Contains(w.Body.String(), "family_readback_required") {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(folder, "handoff-actions.json")); !os.IsNotExist(err) {
		t.Fatal("out-of-order operation acquired an intent")
	}
}

func testFamilyReady(organizer bool) *control.FamilyInfo {
	return &control.FamilyInfo{Member: true, Organizer: organizer, Self: 1, Organizers: 1, Members: 2, StorageKnown: true, StorageParticipating: true, StorageAvailable: true, StorageEnabled: true, StorageBytes: int64(registrationPlanBytes)}
}

func TestFamilyOrganizerBuysOnceThenCreatesAndShares(t *testing.T) {
	account := &Account{ID: "mother", Dir: t.TempDir()}
	paid := false
	buys := 0
	created := false
	shared := false
	f := registrationFamilyFlow{allowSubscription: true, account: account, input: handoffInput{Family: registrationFamily{Role: "organizer", Region: "CN", AcceptSubscription: true}}}
	f.worker = func(action string) control.Result {
		switch action {
		case "storage-info":
			var size uint64
			if paid {
				size = registrationPlanBytes
			}
			return control.Result{OK: true, Storage: &icloud.StorageInfo{PaidBytes: size}}
		case "storage-plan":
			return control.Result{OK: true, StoragePlan: &icloud.StoragePlanInfo{Plans: []icloud.StoragePlan{{Bytes: registrationPlanBytes, Price: 600, Currency: "CNY", Period: "month", Available: true}}}}
		case "storage-skill-purchase-cn":
			buys++
			paid = true
			return control.Result{OK: true, Purchase: &icloud.PurchaseResult{State: "completed"}}
		default:
			t.Fatalf("unexpected worker action %s", action)
			return control.Result{}
		}
	}
	f.family = func(target *Account, extra ...string) (*control.FamilyInfo, error) {
		if target != account {
			t.Fatal("wrong account")
		}
		if len(extra) > 0 {
			switch extra[0] {
			case "-create-family":
				if !paid {
					t.Fatal("created before payment readback")
				}
				created = true
			case "-share-icloud-storage":
				if !created {
					t.Fatal("shared before create")
				}
				shared = true
			default:
				t.Fatal(extra)
			}
		}
		if shared {
			return testFamilyReady(true), nil
		}
		return &control.FamilyInfo{Member: created, Organizer: created, Self: 1, Organizers: 1}, nil
	}
	if r := f.run("subscription-plan", ""); r["purchase_action"] != "storage-skill-purchase-cn" {
		t.Fatal(r)
	}
	for _, action := range []string{"subscription-buy", "subscription-verify", "family-create", "family-share", "family-verify"} {
		if r := f.run(action, "storage-skill-purchase-cn"); r["ok"] != true {
			t.Fatal(action, r)
		}
	}
	if r := f.run("subscription-buy", "storage-skill-purchase-cn"); r["ok"] != true || buys != 1 {
		t.Fatal("existing paid plan was repurchased", r, buys)
	}
	var marker map[string]string
	if readRegisterJSON(account.Dir+"/family-ready.json", &marker) != nil || marker["account"] != "mother" {
		t.Fatal("organizer readiness not persisted")
	}
}

func TestFamilyMemberNeverPurchasesAndUsesBoundOrganizer(t *testing.T) {
	mother := &Account{ID: "mother", Dir: t.TempDir()}
	member := &Account{ID: "member", Dir: t.TempDir()}
	joined, shared := false, false
	f := registrationFamilyFlow{account: member, organizer: mother, input: handoffInput{Family: registrationFamily{Role: "member", Organizer: "mother"}}}
	f.worker = func(action string) control.Result {
		t.Fatalf("member invoked purchase worker: %s", action)
		return control.Result{}
	}
	f.family = func(target *Account, extra ...string) (*control.FamilyInfo, error) {
		if len(extra) > 0 {
			if extra[0] == "-add-pool-member-dir" {
				if target != mother || extra[1] != member.Dir || extra[2] != "-target-password-env" {
					t.Fatal("wrong binding", extra)
				}
				joined = true
			} else if extra[0] == "-verify-pool-member-dir" {
				if target != mother || !joined || extra[1] != member.Dir {
					t.Fatal("wrong exact member readback")
				}
			} else if extra[0] == "-join-shared-storage" {
				if target != member || !joined {
					t.Fatal("wrong storage target")
				}
				shared = true
			} else {
				t.Fatal("unexpected mutation", extra)
			}
		}
		if target == mother {
			return testFamilyReady(true), nil
		}
		if shared {
			return testFamilyReady(false), nil
		}
		return &control.FamilyInfo{Member: joined, Self: 1, Organizers: 1}, nil
	}
	for _, action := range []string{"subscription-plan", "subscription-auth", "subscription-buy", "family-create", "family-share"} {
		if f.run(action, "storage-skill-purchase-cn")["ok"] == true {
			t.Fatal("member allowed organizer action", action)
		}
	}
	for _, action := range []string{"family-join", "family-join-storage", "family-verify"} {
		if r := f.run(action, ""); r["ok"] != true {
			t.Fatal(action, r)
		}
	}
}

func TestFamilyRejectsFullFamilyAndWrongRoleBeforeMutation(t *testing.T) {
	mother := &Account{ID: "mother"}
	member := &Account{ID: "member"}
	f := registrationFamilyFlow{account: member, organizer: mother, input: handoffInput{Family: registrationFamily{Role: "member", Organizer: "mother"}}}
	f.family = func(a *Account, args ...string) (*control.FamilyInfo, error) {
		if len(args) > 0 {
			t.Fatal("full family was mutated")
		}
		if a == member {
			return &control.FamilyInfo{}, nil
		}
		v := testFamilyReady(true)
		v.Members = 6
		return v, nil
	}
	if r := f.run("family-join", ""); r["reason"] != "family_full" {
		t.Fatal(r)
	}
	f.input.Family.Organizer = "different"
	if r := f.run("family-join", ""); r["reason"] != "family_binding_invalid" {
		t.Fatal(r)
	}
}

func TestSubscriptionGateAndOfferLimits(t *testing.T) {
	f := registrationFamilyFlow{account: &Account{}, input: handoffInput{Family: registrationFamily{Role: "organizer", Region: "CN", AcceptSubscription: true}}}
	f.worker = func(string) control.Result { t.Fatal("disabled subscription reached worker"); return control.Result{} }
	if r := f.run("subscription-buy", "storage-skill-purchase-cn"); r["reason"] != "subscription_disabled" {
		t.Fatal(r)
	}
	for _, plan := range []icloud.StoragePlan{
		{Bytes: registrationPlanBytes, Price: 100, Currency: "USD", Period: "month", Available: true},
		{Bytes: registrationPlanBytes * 4, Price: 600, Currency: "CNY", Period: "month", Available: true},
		{Bytes: registrationPlanBytes, Price: 99, Currency: "USD", Period: "year", Available: true},
	} {
		if registrationPlanAction(&icloud.StoragePlanInfo{Plans: []icloud.StoragePlan{plan}}) != "" {
			t.Fatal("unsupported offer accepted", plan)
		}
	}
	plan := icloud.StoragePlan{Bytes: registrationPlanBytes, Price: 99, Currency: "USD", Period: "month", Available: true}
	if registrationPlanAction(&icloud.StoragePlanInfo{Plans: []icloud.StoragePlan{plan}}) != "storage-skill-purchase-us" {
		t.Fatal("existing US target rejected")
	}
}

func TestFamilyRequestValidationAndDefaultDisabled(t *testing.T) {
	for _, f := range []registrationFamily{{Role: "member"}, {Role: "organizer"}, {Role: "member", Organizer: "mother", AcceptSubscription: true}, {Role: "invalid"}} {
		if f.validate() == nil {
			t.Fatal("invalid family request", f)
		}
	}
	s := registerTestServer(t)
	body := registerRequest{Automatic: true, PhoneCC: "GB", AcceptTerms: true, CaptchaProvider: "yes", Family: registrationFamily{Role: "organizer", Region: "US", AcceptSubscription: true}}
	body.Egress.Mode = "direct"
	if _, err := s.startRegistration(body); err == nil {
		t.Fatal("organizer admission allowed while disabled")
	}
	body.Family = registrationFamily{Role: "member", Organizer: "missing"}
	if _, err := s.startRegistration(body); err == nil {
		t.Fatal("member admitted without ready organizer")
	}
}
