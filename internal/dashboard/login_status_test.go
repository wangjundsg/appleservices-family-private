package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeadLoginWorkerDoesNotKeepWaitingForCredentials(t *testing.T) {
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.config.Dir, "runtime.json")
	for _, stage := range []string{"signing_in", "awaiting_sms"} {
		if err := writeRegisterJSON(path, Status{State: stage, Running: true, VerificationRequired: true, PID: 0}); err != nil {
			t.Fatal(err)
		}
		got := s.snapshot(context.Background()).Status
		if got.Running || got.VerificationRequired || got.State != "interrupted" || got.FailureStage != stage {
			t.Fatal(got)
		}
	}
}

func TestRepairDiagnosticSurvivesPanelRestart(t *testing.T) {
	s := registerTestServer(t)
	if err := os.MkdirAll(s.config.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRegisterJSON(filepath.Join(s.config.Dir, "runtime.json"), Status{State: "failed", FailureReason: "gsa_account_repair_required"}); err != nil {
		t.Fatal(err)
	}
	if got := s.snapshot(context.Background()).Status.FailureReason; got != "gsa_account_repair_required" {
		t.Fatal(got)
	}
	path := filepath.Join(t.TempDir(), "test.log")
	for _, message := range []string{"gsa_account_repair_required", `unsupported authentication continuation "repair"`} {
		if err := os.WriteFile(path, []byte("private-marker "+message), 0600); err != nil {
			t.Fatal(err)
		}
		if got := lastProcessError(path); !strings.Contains(got, "账号设置") || strings.Contains(got, "private-marker") {
			t.Fatal(got)
		}
	}
}
