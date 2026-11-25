package control

import (
	"encoding/json"
	"net/http"
)

// BridgeCredentials is private loopback IPC, never part of View or Result.
type BridgeCredentials struct {
	AppleID  string `json:"apple_id"`
	Password string `json:"password"`
}

func (s *Server) SetBridgeCredentials(appleID, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bridgeCredentials = BridgeCredentials{AppleID: appleID, Password: password}
}

func (s *Server) serveBridgeCredentials(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	credentials, ready := s.bridgeCredentials, s.ready
	s.mu.Unlock()
	if !ready || credentials.AppleID == "" || credentials.Password == "" {
		http.Error(w, `{"message":"bridge credentials unavailable"}`, http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(credentials)
}
