package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

type mailBridgeAccount struct {
	ID      string `json:"id"`
	AppleID string `json:"apple_id"`
	Ready   bool   `json:"ready"`
}

func (s *Server) mailBridgeReady(account *Account) bool {
	if account == nil || s.registrationInProgress(account) || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return false
	}
	status := s.accountStatus(account)
	if !status.Running || !status.Authenticated {
		return false
	}
	family, _, stale := readFamilyCache(account)
	if family != nil && !stale {
		return sharedFamilyReady(family)
	}
	// These durable receipts are written only after the full family readback.
	if job := readCNFamilyJob(account); job != nil && job.State == "completed" {
		return true
	}
	return s.readyFamilyOrganizer(account.ID)
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
		items = append(items, mailBridgeAccount{ID: account.ID, AppleID: account.AppleID, Ready: s.mailBridgeReady(account)})
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
