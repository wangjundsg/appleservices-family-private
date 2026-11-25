package dashboard

import (
	"context"
	"encoding/json"
	"github.com/Laky-64/appleservices/internal/accountguard"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

type mailBridgeAccount struct {
	ID      string `json:"id"`
	AppleID string `json:"apple_id"`
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`
}

func (s *Server) mailBridgeReady(account *Account) bool {
	ready, _ := s.mailBridgeStatus(account)
	return ready
}

func (s *Server) mailBridgeStatus(account *Account) (bool, string) {
	if account == nil {
		return false, "account_missing"
	}
	if accountguard.Check(account.Dir) != nil {
		return false, "account_protected"
	}
	if s.registrationInProgress(account) || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return false, "checkpoint_unavailable"
	}
	status := s.accountStatus(account)
	if !status.Running {
		return false, "worker_stopped"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var live control.View
	if s.callFor(ctx, account.Dir, "/view", nil, &live) != nil {
		return false, "worker_control_unavailable"
	}
	if !slices.Contains(live.Capabilities, "mail-bridge-credentials") || !slices.Contains(live.Capabilities, "health-v2") {
		return false, "worker_upgrade_required"
	}
	var liveStatus Status
	if json.Unmarshal(live.Status, &liveStatus) != nil || liveStatus.PID != status.PID || !normalizeHealth(account.Dir, liveStatus).SessionVerified {
		return false, "session_unverified"
	}
	if !live.BridgeCredentialsReady {
		return false, "credentials_unavailable"
	}
	family, _, stale := readFamilyCache(account)
	if family == nil || stale || !sharedFamilyReady(family) {
		return false, "family_unverified_or_stale"
	}
	storage, stale := readStorageCache(account, storageCacheFile)
	if storage == nil || stale || storage.Storage == nil || storage.Storage.TotalBytes < registrationPlanBytes {
		return false, "storage_unverified_or_stale"
	}
	return true, ""
}

func (s *Server) serveMailBridgeAccounts(w http.ResponseWriter, _ *http.Request) {
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	items := make([]mailBridgeAccount, 0, len(accounts))
	for _, account := range accounts {
		if strings.TrimSpace(account.AppleID) == "" {
			continue
		}
		ready, reason := s.mailBridgeStatus(account)
		items = append(items, mailBridgeAccount{ID: account.ID, AppleID: account.AppleID, Ready: ready, Reason: reason})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"accounts": items})
}

func (s *Server) serveMailBridgeCredentials(w http.ResponseWriter, r *http.Request) {
	account := s.lookup(r.URL.Query().Get("account"))
	if r.URL.Query().Get("account") == "" || !s.mailBridgeReady(account) {
		http.Error(w, `{"message":"bridge account not ready"}`, http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var credentials control.BridgeCredentials
	if err := s.callFor(ctx, account.Dir, "/bridge-credentials", struct{}{}, &credentials); err != nil ||
		credentials.Password == "" || !strings.EqualFold(strings.TrimSpace(credentials.AppleID), strings.TrimSpace(account.AppleID)) || !s.mailBridgeReady(account) {
		http.Error(w, `{"message":"bridge credentials unavailable"}`, http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(credentials)
}
