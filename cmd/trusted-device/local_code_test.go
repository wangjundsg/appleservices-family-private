package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCodeDoesNotRequireOrChangeDeviceSession(t *testing.T) {
	dir := t.TempDir()
	state := []byte(`{"state":"refresh_failed","authenticated":false,"renewal_http_status":401}`)
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result := localCodeResult(context.Background(), dir, func(context.Context) (string, error) { calls++; return "123456", nil })
	if !result.OK || len(result.Code) != 6 || calls != 1 {
		t.Fatal("local generation failed")
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		data, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if strings.Contains(string(data), "123456") {
			t.Fatal("code persisted")
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if string(after) != string(state) {
		t.Fatal("device status changed")
	}
	if err := os.WriteFile(filepath.Join(dir, "account-guard.json"), []byte(`{"version":1,"blocked":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := localCodeResult(context.Background(), dir, func(context.Context) (string, error) { calls++; return "123456", nil })
	if blocked.OK || blocked.Code != "" || blocked.Stage != "account_protected" || calls != 1 {
		t.Fatal("protected worker invoked provider")
	}
}

func TestLocalCodeProviderFailureIsExplicitAndSanitized(t *testing.T) {
	dir := t.TempDir()
	if got := localCodeResult(context.Background(), dir, nil); got.Stage != "code_provider_unavailable" {
		t.Fatal("unready provider hidden")
	}
	for _, provider := range []func(context.Context) (string, error){
		func(context.Context) (string, error) { return "123456", errors.New("private failure 123456") },
		func(context.Context) (string, error) { return "invalid", nil },
	} {
		got := localCodeResult(context.Background(), dir, provider)
		if got.OK || got.Code != "" || got.Stage != "code_generation_failed" || strings.Contains(got.Message, "123456") {
			t.Fatal("provider failure leaked or accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := localCodeResult(ctx, dir, func(context.Context) (string, error) { t.Fatal("canceled request invoked provider"); return "", nil })
	if got.Stage != "canceled" {
		t.Fatal("cancellation lost")
	}
}
