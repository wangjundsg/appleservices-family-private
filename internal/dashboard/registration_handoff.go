package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

// Adapter only: Python decides when to advance; these actions reuse existing
// account login and activation code. Unknown writes are never retried here.
type handoffInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Handoff  string `json:"handoff"`
}

type registrationContextKey struct{}

type handoffReceipt struct {
	Digest string         `json:"digest"`
	State  string         `json:"state"`
	Result map[string]any `json:"result,omitempty"`
}

func (s *Server) handoffAccount(input handoffInput) *Account {
	key := sha256.Sum256([]byte(normalizedRegistrationEmail(input.Email)))
	return s.lookup("auto-" + hex.EncodeToString(key[:12]))
}

func (s *Server) attachRegistration(id string, input handoffInput) (*Account, error) {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	key := sha256.Sum256([]byte(normalizedRegistrationEmail(input.Email)))
	accountID := "auto-" + hex.EncodeToString(key[:12])
	for _, account := range s.accounts {
		if normalizedRegistrationEmail(account.AppleID) == normalizedRegistrationEmail(input.Email) {
			if account.ID != accountID {
				return nil, os.ErrExist
			}
			var origin struct {
				JobID string `json:"job_id"`
			}
			if readRegisterJSON(filepath.Join(account.Dir, "registration-origin.json"), &origin) != nil || origin.JobID != id {
				return nil, os.ErrExist
			}
			return account, nil
		}
	}
	if s.registryNote != "" {
		return nil, os.ErrInvalid
	}
	dir := filepath.Join(accountsDir(s.config.Dir), accountID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "device.json")); os.IsNotExist(err) {
		if err = provisionIdentity(dir); err != nil {
			return nil, err
		}
	}
	if err := writeRegisterJSON(filepath.Join(dir, "registration-origin.json"), map[string]string{"job_id": id}); err != nil {
		return nil, err
	}
	account := &Account{ID: accountID, Name: "Apple " + id[len(id)-6:], AppleID: input.Email, Dir: dir, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	entries := make([]*registryEntry, 0, len(s.accounts)+1)
	for _, a := range append(append([]*Account(nil), s.accounts...), account) {
		entries = append(entries, &registryEntry{ID: a.ID, Name: a.Name, AppleID: a.AppleID, Dir: a.Dir, CreatedAt: a.CreatedAt})
	}
	if err := saveRegistry(s.config.Dir, entries); err != nil {
		return nil, err
	}
	s.accounts = append(s.accounts, account)
	return account, nil
}

func (s *Server) serveRegistrationHandoff(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Action string `json:"action"`
		Code   string `json:"code"`
		Terms  string `json:"terms"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var input handoffInput
	folder := s.registerFolder(id)
	if readRegisterJSON(filepath.Join(folder, "input.json"), &input) != nil || input.Handoff != "dashboard" {
		http.Error(w, `{"state":"unknown"}`, 409)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), registrationContextKey{}, true), 195*time.Second)
	defer cancel()
	account := s.handoffAccount(input)
	if body.Action == "observe" {
		value := map[string]any{"ok": true, "attached": account != nil}
		if account != nil {
			status := s.accountStatus(account)
			value["gsa_verified"] = verifiedGSACheckpointReady(account.Dir, account.AppleID)
			value["activated"] = acceptedCheckpointReady(account.Dir, account.AppleID)
			value["waiting_sms"] = status.Running && status.State == "awaiting_sms"
			value["online"] = status.Running && status.Authenticated
			value["failed"] = !status.Running && status.State == "failed"
			// Existing worker diagnostics stay private; only recognize the fixed rate-limit marker.
			if workerRateLimited(account.Dir) {
				value["state"] = "rate_limited"
			}
		}
		json.NewEncoder(w).Encode(value)
		return
	}
	if !s.handoffMu.TryLock() {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "state": "unknown"})
		return
	}
	defer s.handoffMu.Unlock()
	mutating := body.Action == "attach" || body.Action == "start" || body.Action == "sms" || body.Action == "activate" || body.Action == "resume"
	receiptFile := filepath.Join(folder, "handoff-actions.json")
	receipts := map[string]handoffReceipt{}
	encoded, _ := json.Marshal(body)
	digest := sha256.Sum256(encoded)
	intent := handoffReceipt{Digest: hex.EncodeToString(digest[:]), State: "intent"}
	if mutating {
		if err := readRegisterJSON(receiptFile, &receipts); err != nil && !os.IsNotExist(err) {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "state": "unknown"})
			return
		}
		if prior, exists := receipts[body.Action]; exists {
			if prior.State == "succeeded" && prior.Digest == intent.Digest {
				json.NewEncoder(w).Encode(prior.Result)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "state": "unknown"})
			}
			return
		}
		receipts[body.Action] = intent
		if writeRegisterJSON(receiptFile, receipts) != nil {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "state": "unknown"})
			return
		}
	}
	var result control.Result
	switch body.Action {
	case "attach":
		var err error
		account, err = s.attachRegistration(id, input)
		result.OK = err == nil
	case "start":
		if account != nil {
			result = s.startAccount(ctx, account, credentials{AppleID: input.Email, Password: input.Password, TwoFactor: "sms"})
		}
	case "sms":
		if account != nil && registrationOTPPattern.MatchString(body.Code) {
			if s.callFor(ctx, account.Dir, "/action", control.Action{Action: "submit-sms", Code: body.Code}, &result) != nil {
				result.OK = false
			}
		}
	case "plan":
		result = s.activationAction(ctx, account, false, "")
	case "activate":
		result = s.activationAction(ctx, account, true, body.Terms)
	case "resume":
		if account != nil {
			if s.accountStatus(account).Running && s.accountStatus(account).Authenticated {
				result.OK = true
			} else {
				result = s.startSavedAccount(ctx, account)
			}
		}
	case "stop":
		if account != nil {
			result.OK = s.stopIn(ctx, account.Dir) == nil
		}
	default:
		http.Error(w, `{"state":"unknown"}`, 400)
		return
	}
	value := map[string]any{"ok": result.OK}
	if account != nil {
		value["account_id"] = account.ID
	}
	if result.Activation != nil {
		value["terms"] = activationTuple(result.Activation)
	}
	if !result.OK {
		value["state"] = "unknown"
	}
	if mutating && result.OK {
		intent.State, intent.Result = "succeeded", value
		receipts[body.Action] = intent
		if writeRegisterJSON(receiptFile, receipts) != nil {
			value = map[string]any{"ok": false, "state": "unknown"}
		}
	}
	json.NewEncoder(w).Encode(value)
}

func workerRateLimited(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "daemon.err.log"))
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	start := info.Size() - 65536
	if start < 0 {
		start = 0
	}
	_, _ = f.Seek(start, 0)
	buf := make([]byte, 65536)
	n, _ := f.Read(buf)
	return registrationRatePattern.Match(buf[:n])
}
