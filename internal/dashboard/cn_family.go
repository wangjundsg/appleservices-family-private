package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

// Existing accounts reuse the same activation and family adapters as newly
// registered accounts. Each request performs one step; paid setup remains human attended.
func (s *Server) serveCNFamily(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account     string `json:"account"`
		Role        string `json:"role"`
		Organizer   string `json:"organizer"`
		Step        string `json:"step"`
		Password    string `json:"password,omitempty"`
		AcceptTerms bool   `json:"accept_terms,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	account := s.lookup(body.Account)
	family := registrationFamily{Role: body.Role, Region: "CN", Organizer: body.Organizer, AcceptSubscription: body.Role == "organizer"}
	respond := func(value any) {
		if result, ok := value.(map[string]any); ok {
			if reason, ok := result["reason"].(string); ok {
				if message := registerMessages[reason]; message != "" && result["message"] == nil {
					result["message"] = message
				}
			}
		}
		json.NewEncoder(w).Encode(value)
	}
	if body.Account == "" || account == nil || family.validate() != nil || s.registrationInProgress(account) {
		respond(map[string]any{"ok": false, "reason": "family_binding_invalid"})
		return
	}
	if !s.handoffMu.TryLock() {
		respond(map[string]any{"ok": false, "reason": "family_busy"})
		return
	}
	defer s.handoffMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 235*time.Second)
	defer cancel()
	if body.Step == "connect" {
		respond(s.startInMode(ctx, account.Dir, credentials{AppleID: account.AppleID, Password: body.Password, TwoFactor: "trusted-device"}, account, acceptedCheckpointReady(account.Dir, account.AppleID)))
		return
	}
	if body.Step == "observe" {
		status := s.accountStatus(account)
		respond(map[string]any{"ok": true, "online": status.Running && status.Authenticated, "verification_required": status.VerificationRequired, "activation_ready": verifiedGSACheckpointReady(account.Dir, account.AppleID), "activated": acceptedCheckpointReady(account.Dir, account.AppleID)})
		return
	}
	if body.Step == "activate" {
		result := s.activationAction(ctx, account, false, "")
		if result.OK && result.Activation != nil && result.Activation.Required && body.AcceptTerms {
			if result.Activation.Region != "CN" {
				respond(map[string]any{"ok": false, "reason": "subscription_offer_unsupported"})
				return
			}
			result = s.activationAction(ctx, account, true, activationTuple(result.Activation))
		}
		respond(result)
		return
	}
	switch body.Step {
	case "subscription-plan", "subscription-verify", "subscription-cancel", "family-create", "family-share", "family-join", "family-join-storage", "family-verify":
		// No purchase or payment-binding action is callable through automation.
		respond(s.registrationFamilyAction(ctx, account, handoffInput{Email: account.AppleID, Password: body.Password, Family: family}, body.Step, ""))
	default:
		respond(control.Result{Stage: "family-step", Message: "不支持的国区流程步骤"})
	}
}
