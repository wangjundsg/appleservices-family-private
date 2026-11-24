package dashboard

import (
	"context"
	"encoding/json"
	appleservices "github.com/Laky-64/appleservices"
	"os"
	"path/filepath"
	"testing"
)

func cnTestAcceptedAccount(t *testing.T, id string) *Account {
	t.Helper()
	a := &Account{ID: id, AppleID: id + "@example.test", Dir: t.TempDir()}
	saved := appleservices.Session{AppleID: a.AppleID, DSID: "fixture-gsa", Tokens: map[string]string{"com.apple.gs.icloud.auth": "fixture-token"}}
	accepted := acceptedICloudCheckpoint{Session: appleservices.Session{AppleID: a.AppleID, DSID: "fixture-icloud", Tokens: map[string]string{"mmeAuthToken": "fixture-mme"}}, ADSID: saved.DSID, ServiceData: map[string]any{"tokens": map[string]any{"mmeAuthToken": "fixture-mme"}}}
	for name, value := range map[string]any{"session.json": saved, "icloud-accepted.json": accepted} {
		if err := writeRegisterJSON(filepath.Join(a.Dir, name), value); err != nil {
			t.Fatal(err)
		}
	}
	return a
}
func TestCNBackgroundQueuesExistingOrganizerWithoutLegacyReceipt(t *testing.T) {
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	mother := cnTestAcceptedAccount(t, "mother")
	child := cnTestAcceptedAccount(t, "child")
	s.accounts = []*Account{mother, child}
	if s.readyFamilyOrganizer(mother.ID) {
		t.Fatal("fixture unexpectedly has legacy receipt")
	}
	if r := s.queueCNFamily(child, mother.ID, true); r["ok"] != true {
		t.Fatal(r)
	}
	if job := readCNFamilyJob(child); job == nil || job.Organizer != mother.ID || job.Step != "family-join" {
		t.Fatal(job)
	}
	fresh := &Server{config: s.config, accounts: s.accounts}
	if job := readCNFamilyJob(child); job == nil || !fresh.cnFamilyPolicy().Enabled {
		t.Fatal("restart lost authorized job")
	}
	for _, a := range []*Account{mother, child} {
		if r := s.queueCNFamily(a, a.ID, false); r["ok"] == true {
			t.Fatal("self organization accepted")
		}
	}
	newer := &Account{ID: "new-child", Dir: t.TempDir()}
	s.accounts = append(s.accounts, newer)
	s.runCNFamilyJobs(context.Background()) // No active workers: only enroll, never mutate.
	if job := readCNFamilyJob(newer); job == nil || job.State != "waiting" || job.Organizer != mother.ID {
		t.Fatal("new activation was not queued", job)
	}
	if readCNFamilyJob(mother) != nil {
		t.Fatal("organizer enrolled itself")
	}
	data, _ := os.ReadFile(filepath.Join(child.Dir, "cn-family-job.json"))
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	for key := range fields {
		switch key {
		case "account", "organizer", "state", "step", "updated_at", "reason":
		default:
			t.Fatalf("unexpected persisted field %s", key)
		}
	}
}
func TestCNBackgroundStepsPersistAndNeverEnterCommerce(t *testing.T) {
	a := &Account{ID: "child", Dir: t.TempDir()}
	job := &cnFamilyJob{Account: a.ID, Organizer: "mother", State: "waiting", Step: "family-join"}
	calls := []string{}
	for i := 0; i < 4; i++ {
		advanceCNFamilyJob(context.Background(), a, job, func(ctx context.Context, step string) map[string]any {
			calls = append(calls, step)
			return map[string]any{"ok": true}
		})
		job = readCNFamilyJob(a)
	}
	if job.State != "completed" || len(calls) != 3 || calls[0] != "family-join" || calls[1] != "family-join-storage" || calls[2] != "family-verify" {
		t.Fatal(job, calls)
	}
}
func TestCNBackgroundUnknownWritesAndFullFamilyStop(t *testing.T) {
	for _, tc := range []struct{ state, step, reason string }{{"waiting", "family-join", "family_full"}, {"running", "family-join", "family_readback_required"}, {"waiting", "family-join-storage", "family_readback_required"}} {
		a := &Account{ID: "child", Dir: t.TempDir()}
		job := &cnFamilyJob{Account: a.ID, Organizer: "mother", State: tc.state, Step: tc.step}
		calls := 0
		fn := func(ctx context.Context, step string) map[string]any {
			calls++
			if tc.state == "running" && step != "family-verify" {
				t.Fatal("replayed interrupted mutation")
			}
			return map[string]any{"ok": false, "reason": tc.reason}
		}
		advanceCNFamilyJob(context.Background(), a, job, fn)
		job = readCNFamilyJob(a)
		advanceCNFamilyJob(context.Background(), a, job, fn)
		if calls != 1 || job.State != "blocked" || job.Reason != tc.reason {
			t.Fatal(job, calls)
		}
	}
}
func TestCNBackgroundRequiresConsentAndSupportsStopping(t *testing.T) {
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	m := cnTestAcceptedAccount(t, "mother")
	c := cnTestAcceptedAccount(t, "child")
	s.accounts = []*Account{m, c}
	for _, body := range []string{`{"account":"child","organizer":"mother","step":"enqueue"}`, `{"account":"child","organizer":"child","step":"enqueue","accept_terms":true}`} {
		w := registerTestCall(s, "POST", "/api/cn-family-background", body)
		var r map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		if r["ok"] == true {
			t.Fatal("unauthorized binding accepted")
		}
	}
	_ = s.queueCNFamily(c, m.ID, true)
	for _, step := range []string{"cancel-job", "disable-default"} {
		w := registerTestCall(s, "POST", "/api/cn-family-background", `{"account":"child","step":"`+step+`"}`)
		var r map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		if r["ok"] != true {
			t.Fatal(r)
		}
	}
	if readCNFamilyJob(c).State != "canceled" || s.cnFamilyPolicy().Enabled {
		t.Fatal("stop was not persisted")
	}
}
