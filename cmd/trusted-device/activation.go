package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type activationCommand func(context.Context, string, ...string) ([]byte, error)

func activationTermsTuple(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		var p struct {
			Plan   bool   `json:"terms-plan"`
			Region string `json:"region"`
			ICloud string `json:"icloud-version"`
			Media  string `json:"media-version"`
		}
		if json.Unmarshal([]byte(line), &p) == nil && p.Plan && p.Region != "" && p.ICloud != "" {
			return p.Region + ":" + p.ICloud + ":" + p.Media
		}
	}
	return ""
}

func activationAccepted(output []byte, err error) bool {
	return err == nil && strings.Contains(string(output), "delegate accepted; mme-token-present=true") && strings.Contains(string(output), "quota accepted;")
}

// The first exchange is read for its explicit terms refusal. Consent is used
// only for that branch, never for unrelated device errors or ambiguous writes.
func activateWithNAC(ctx context.Context, run activationCommand, helper, dir, work string) error {
	base := []string{"-dir", dir, "-resume", filepath.Join(dir, "session.json")}
	checkpoint := filepath.Join(work, "icloud-accepted.json")
	exchange := func(accepted bool) ([]byte, error) {
		args := append(append([]string{}, base...), "-delegate-only", "-nac-validation", filepath.Join(work, "nac-validation.bin"), "-icloud-checkpoint", checkpoint)
		name := "delegate.plist"
		if accepted {
			args = append(args, "-terms-accepted")
			name = "delegate-after-terms.plist"
		}
		return run(ctx, helper, append(args, "-dump", filepath.Join(work, name))...)
	}
	output, err := exchange(false)
	if activationAccepted(output, err) {
		return nil
	}
	if !strings.Contains(string(output), "ErrorID=MOBILEME_TERMS_OF_SERVICE_UPDATE") {
		return errors.New("automatic_activation_delegate_refused")
	}
	output, err = run(ctx, helper, append(append([]string{}, base...), "-dump", filepath.Join(work, "setup.html"))...)
	tuple := activationTermsTuple(output)
	if err != nil || tuple == "" {
		return errors.New("automatic_activation_terms_unavailable")
	}
	receiptPath := filepath.Join(work, "terms.html")
	// This command may report a later delegate failure; the separate terms
	// acknowledgement decides whether we can attach termsAccepted to the NAC call.
	_, _ = run(ctx, helper, append(append([]string{}, base...), "-accept-terms", "-expected-terms", tuple, "-dump", receiptPath)...)
	var receipt struct {
		Success bool `json:"success"`
	}
	data, readErr := os.ReadFile(receiptPath + ".update.json")
	if readErr != nil || json.Unmarshal(data, &receipt) != nil || !receipt.Success {
		return errors.New("automatic_activation_terms_unconfirmed")
	}
	output, err = exchange(true)
	if !activationAccepted(output, err) {
		return errors.New("automatic_activation_delegate_refused")
	}
	return nil
}

func copyActivationRuntime(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("activation_runtime_symlink")
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destination, relative), 0700)
		}
		// Generated hardware and validation artifacts are never copied between accounts.
		if !entry.Type().IsRegular() || strings.HasSuffix(entry.Name(), ".plist") || strings.HasSuffix(entry.Name(), ".pyc") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destination, relative), data, 0600)
	})
}

func autoActivate(ctx context.Context, dir, account, python string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(executable))
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp(filepath.Join(root, ".cache"), "worker-activation-")
	if err != nil {
		return err
	}
	if err = protectActivationPath(work, true); err != nil {
		return err
	}
	// Preserve private receipts for diagnosing unknown outcomes; never replay them.
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, name, args...)
		hideActivationProcess(command)
		command.Dir = root
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if strings.EqualFold(key, "APPLESERVICES_PASSWORD") || strings.EqualFold(key, "APPLESERVICES_APPLEID") || strings.EqualFold(key, "APPLESERVICES_DELEGATE_PATH") || strings.EqualFold(key, "APPLESERVICES_DELEGATE_DUMP") {
				continue
			}
			command.Env = append(command.Env, value)
		}
		command.Env = append(command.Env, "APPLESERVICES_APPLEID="+account, "APPLESERVICES_DELEGATE_PATH=v2")
		return command.CombinedOutput()
	}
	runtime := filepath.Join(work, "nac-runtime")
	if err = copyActivationRuntime(filepath.Join(root, ".cache", "mac-nac-runtime"), runtime); err != nil {
		return errors.New("automatic_activation_runtime_unavailable")
	}
	output, err := run(ctx, python, filepath.Join(root, "tools", "mac-nac-check.py"), "--profile", filepath.Join(dir, "native-adi", "hw_info.plist"), "--runtime", runtime, "--encrypt-dll", filepath.Join(root, ".cache", "activation-source-review", "mac-io-encrypt.dll"), "--validation-out", filepath.Join(work, "nac-validation.bin"), "--synthetic-missing-board-fields", "--transport", filepath.Join(root, "bin", "nacexchange.exe"))
	if err != nil || !strings.Contains(string(output), `"stage": "NACSign"`) {
		return errors.New("automatic_activation_nac_failed")
	}
	if err = activateWithNAC(ctx, run, filepath.Join(root, "bin", "accountsetupcheck.exe"), dir, work); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(work, "icloud-accepted.json"))
	if err != nil {
		return errors.New("automatic_activation_checkpoint_missing")
	}
	var checkpoint struct {
		Session struct {
			AppleID string
			DSID    string
			Tokens  map[string]string
		}
		ADSID string `json:"adsid"`
	}
	saved, err := (fileStore{dir: dir}).LoadSession()
	if err != nil || saved == nil || json.Unmarshal(data, &checkpoint) != nil || !strings.EqualFold(checkpoint.Session.AppleID, account) || !strings.EqualFold(saved.AppleID, account) || checkpoint.ADSID != saved.DSID || checkpoint.Session.DSID == "" || checkpoint.Session.Tokens["mmeAuthToken"] == "" {
		return errors.New("automatic_activation_checkpoint_binding_failed")
	}
	destination := filepath.Join(dir, "icloud-accepted.json")
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("automatic_activation_checkpoint_exists_or_unwritable")
	}
	if err = protectActivationPath(destination, false); err != nil {
		file.Close()
		return err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	return errors.Join(err, closeErr)
}
