package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHME records what the resume logic asked for; the test owns the behaviour.
type fakeHME struct {
	generate  func() (string, error)
	reserve   func(candidate, label string) error
	setActive func(email string, active bool) error
	delete    func(email string) error
}

func (f *fakeHME) Generate(context.Context, string) (string, error) { return f.generate() }

func (f *fakeHME) Reserve(_ context.Context, candidate, label, _ string) error {
	return f.reserve(candidate, label)
}

func (f *fakeHME) SetActive(_ context.Context, email string, active bool) error {
	return f.setActive(email, active)
}

func (f *fakeHME) Delete(_ context.Context, email string) error { return f.delete(email) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReserveReusesCandidateAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	generated := 0
	var reserved []string
	failReserve := true
	fake := &fakeHME{
		generate: func() (string, error) {
			generated++
			return "cand-1@privaterelay.appleid.com", nil
		},
		reserve: func(candidate, label string) error {
			reserved = append(reserved, candidate+"|"+label)
			if failReserve {
				return errors.New("connection reset by peer")
			}
			return nil
		},
	}

	if err := reserve(context.Background(), fake, dir, "", "netflix", io.Discard); err == nil {
		t.Fatal("first reserve should surface the transport error")
	}
	pending, err := readPending(dir)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if pending == nil {
		t.Fatal("interrupted reserve left no pending record")
	}
	if pending.Action != "reserve" || pending.Address != "cand-1@privaterelay.appleid.com" ||
		pending.Label != "netflix" || pending.OperationID == "" {
		t.Fatalf("pending record = %+v, want the reserve candidate and label", pending)
	}

	failReserve = false
	var out bytes.Buffer
	if err := reserve(context.Background(), fake, dir, "", "netflix", &out); err != nil {
		t.Fatalf("resumed reserve failed: %v", err)
	}
	if generated != 1 {
		t.Fatalf("Generate calls = %d, want 1 (a resume must not burn a new candidate)", generated)
	}
	if len(reserved) != 2 || reserved[0] != reserved[1] {
		t.Fatalf("reserve calls = %v, want the same candidate twice", reserved)
	}
	if !strings.Contains(out.String(), "pending=resumed operation="+pending.OperationID) {
		t.Errorf("stdout = %q, want a resumed line naming the operation", out.String())
	}
	if !strings.Contains(out.String(), "reserved=cand-1@privaterelay.appleid.com label=netflix") {
		t.Errorf("stdout = %q, want the reserved line", out.String())
	}
	if _, statErr := os.Stat(pendingPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("pending record survived the successful reserve (err=%v)", statErr)
	}
}

func TestReserveRecordsPendingBeforeCallingRemote(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeHME{
		generate: func() (string, error) {
			t.Error("Generate must not be called when -email supplies the candidate")
			return "", nil
		},
		reserve: func(candidate, label string) error {
			op, err := readPending(dir)
			if err != nil || op == nil {
				t.Errorf("Reserve ran without a pending record on disk (err=%v)", err)
				return nil
			}
			if op.Action != "reserve" || op.Address != candidate || op.Label != label {
				t.Errorf("pending = %+v, want it to match the in-flight reserve", op)
			}
			return nil
		},
	}
	var out bytes.Buffer
	if err := reserve(context.Background(), fake, dir, "manual@privaterelay.appleid.com", "shopping", &out); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !strings.Contains(out.String(), "reserved=manual@privaterelay.appleid.com label=shopping") {
		t.Errorf("stdout = %q, want the reserved line", out.String())
	}
}

func TestReserveRejectsMismatchedPendingCandidate(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, &pendingOp{
		OperationID: "op-1", Action: "reserve", Address: "kept@privaterelay.appleid.com", Label: "kept",
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	fake := &fakeHME{
		generate: func() (string, error) { t.Error("Generate must not run"); return "", nil },
		reserve:  func(string, string) error { t.Error("Reserve must not run"); return nil },
	}
	err := reserve(context.Background(), fake, dir, "other@privaterelay.appleid.com", "other", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unfinished reserve targets") {
		t.Fatalf("error = %v, want a mismatch rejection", err)
	}
	if op, readErr := readPending(dir); readErr != nil || op == nil || op.Address != "kept@privaterelay.appleid.com" {
		t.Fatalf("pending record was changed: %+v (err=%v)", op, readErr)
	}
}

func TestDeleteResumesAfterDeactivate(t *testing.T) {
	dir := t.TempDir()
	deactivations, deletions := 0, 0
	failDelete := true
	fake := &fakeHME{
		setActive: func(email string, active bool) error {
			deactivations++
			if active {
				t.Errorf("delete must deactivate %s, got active=true", email)
			}
			return nil
		},
		delete: func(string) error {
			deletions++
			if failDelete {
				failDelete = false
				return errors.New("delete timed out")
			}
			return nil
		},
	}

	if err := deleteAlias(context.Background(), fake, dir, "a@privaterelay.appleid.com", io.Discard); err == nil {
		t.Fatal("first delete should surface the remote error")
	}
	pending, err := readPending(dir)
	if err != nil || pending == nil {
		t.Fatalf("read pending = %+v, %v; want a record after the failed delete", pending, err)
	}
	if pending.Action != "delete" || !pending.DeactivateDone || pending.DeleteDone {
		t.Fatalf("pending = %+v, want deactivate_done only", pending)
	}

	var out bytes.Buffer
	if err := deleteAlias(context.Background(), fake, dir, "a@privaterelay.appleid.com", &out); err != nil {
		t.Fatalf("resumed delete failed: %v", err)
	}
	if deactivations != 1 {
		t.Fatalf("deactivate calls = %d, want 1 (the finished step must be skipped)", deactivations)
	}
	if deletions != 2 {
		t.Fatalf("delete calls = %d, want 2", deletions)
	}
	if !strings.Contains(out.String(), "pending=resumed") || !strings.Contains(out.String(), "stage=deactivate_done") {
		t.Errorf("stdout = %q, want the resumed stage line", out.String())
	}
	if _, statErr := os.Stat(pendingPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("pending record survived the successful delete (err=%v)", statErr)
	}
}

func TestDeleteSkipsEveryFinishedStage(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, &pendingOp{
		OperationID: "op-2", Action: "delete", Address: "b@privaterelay.appleid.com",
		DeactivateDone: true, DeleteDone: true,
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	fake := &fakeHME{
		setActive: func(string, bool) error { t.Error("a recorded deactivate must not repeat"); return nil },
		delete:    func(string) error { t.Error("a recorded delete must not repeat"); return nil },
	}
	var out bytes.Buffer
	if err := deleteAlias(context.Background(), fake, dir, "b@privaterelay.appleid.com", &out); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out.String(), "stage=delete_done") || !strings.Contains(out.String(), "deleted=b@privaterelay.appleid.com") {
		t.Errorf("stdout = %q, want both finished stages reported", out.String())
	}
	if _, statErr := os.Stat(pendingPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("pending record survived (err=%v)", statErr)
	}
}

func TestPendingOfAnotherOperationIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, &pendingOp{
		OperationID: "op-3", Action: "reserve", Address: "held@privaterelay.appleid.com", Label: "held",
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	fake := &fakeHME{
		setActive: func(string, bool) error { t.Error("SetActive must not run"); return nil },
		delete:    func(string) error { t.Error("Delete must not run"); return nil },
	}
	err := deleteAlias(context.Background(), fake, dir, "held@privaterelay.appleid.com", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unfinished reserve operation") {
		t.Fatalf("error = %v, want a cross-operation rejection", err)
	}
}

func TestSessionDirMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "session.json"), `{"appleId":"first@example.com"}`)
	sessionPath := filepath.Join(t.TempDir(), "icloud-web-session.json")
	writeFile(t, sessionPath, `{"apple_id":"other@example.com","cookies":[{"name":"a","value":"b"}]}`)

	if _, _, _, err := accountBinding(dir, sessionPath); err == nil ||
		!strings.Contains(err.Error(), "refusing before any remote call") {
		t.Fatalf("error = %v, want a cross-check rejection", err)
	}
}

func TestSessionDirMatchAcceptsCaseDifference(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "session.json"), `{"appleId":"First@Example.com"}`)
	sessionPath := filepath.Join(t.TempDir(), "icloud-web-session.json")
	writeFile(t, sessionPath, `{"apple_id":"first@example.com","cookies":[{"name":"a","value":"b"}]}`)

	appleID, _, cookies, err := accountBinding(dir, sessionPath)
	if err != nil {
		t.Fatalf("accountBinding: %v", err)
	}
	if appleID != "first@example.com" || len(cookies) != 1 {
		t.Fatalf("binding = %q cookies=%d, want the session file's account", appleID, len(cookies))
	}
}

func TestSessionDirSnakeCaseBindingIsCompared(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "session.json"), `{"apple_id":"dir@example.com"}`)
	sessionPath := filepath.Join(t.TempDir(), "icloud-web-session.json")
	writeFile(t, sessionPath, `{"apple_id":"session@example.com","cookies":[{"name":"a","value":"b"}]}`)

	if _, _, _, err := accountBinding(dir, sessionPath); err == nil ||
		!strings.Contains(err.Error(), "refusing before any remote call") {
		t.Fatalf("error = %v, want the apple_id binding to be compared", err)
	}
}

func TestDirectoryWithoutSessionBindingAllowsAdoption(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(t.TempDir(), "icloud-web-session.json")
	writeFile(t, sessionPath, `{"apple_id":"new@example.com","cookies":[{"name":"a","value":"b"}]}`)

	appleID, _, _, err := accountBinding(dir, sessionPath)
	if err != nil {
		t.Fatalf("accountBinding: %v", err)
	}
	if appleID != "new@example.com" {
		t.Fatalf("appleID = %q, want new@example.com", appleID)
	}
}

func TestPendingRecordIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, &pendingOp{OperationID: "op-4", Action: "reserve", Address: "x@privaterelay.appleid.com", Label: "l"}); err != nil {
		t.Fatalf("writePending: %v", err)
	}
	data, err := os.ReadFile(pendingPath(dir))
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	var decoded pendingOp
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("pending is not valid JSON: %v", err)
	}
	if decoded.OperationID != "op-4" {
		t.Errorf("decoded = %+v, want the seeded record", decoded)
	}
	if _, statErr := os.Stat(filepath.Join(dir, pendingFileName)); statErr != nil {
		t.Errorf("pending file missing: %v", statErr)
	}
}
