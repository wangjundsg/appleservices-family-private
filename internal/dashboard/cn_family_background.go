package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/Laky-64/appleservices/internal/accountguard"
)

type cnFamilyJob struct {
	Account   string    `json:"account"`
	Organizer string    `json:"organizer"`
	State     string    `json:"state"`
	Step      string    `json:"step"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
type cnFamilyPolicy struct {
	Enabled   bool     `json:"enabled"`
	Organizer string   `json:"organizer"`
	Existing  []string `json:"existing,omitempty"`
}

func (s *Server) cnFamilyPolicy() cnFamilyPolicy {
	var p cnFamilyPolicy
	_ = readRegisterJSON(filepath.Join(s.config.Dir, "cn-family-policy.json"), &p)
	return p
}
func readCNFamilyJob(account *Account) *cnFamilyJob {
	if account == nil {
		return nil
	}
	var job cnFamilyJob
	if readRegisterJSON(filepath.Join(account.Dir, "cn-family-job.json"), &job) != nil || job.Account != account.ID || job.Organizer == "" || job.Organizer == account.ID {
		return nil
	}
	return &job
}
func saveCNFamilyJob(account *Account, job *cnFamilyJob) error {
	job.UpdatedAt = time.Now()
	return writeRegisterJSON(filepath.Join(account.Dir, "cn-family-job.json"), job)
}

// Authorization and account-directory IDs only. Passwords remain in worker memory.
func (s *Server) queueCNFamily(account *Account, organizer string, future bool) map[string]any {
	if account != nil && accountguard.Check(account.Dir) != nil {
		return map[string]any{"ok": false, "reason": "account_protected", "message": accountguard.Message}
	}
	mother := s.lookup(organizer)
	if account == nil || mother == nil || mother.ID == account.ID || s.registrationInProgress(mother) || !acceptedCheckpointReady(mother.Dir, mother.AppleID) {
		return map[string]any{"ok": false, "reason": "family_organizer_not_ready"}
	}
	job := &cnFamilyJob{Account: account.ID, Organizer: organizer, State: "waiting", Step: "family-join"}
	if old := readCNFamilyJob(account); old != nil && old.Organizer == organizer {
		if old.State == "completed" {
			job = old
		} else if old.State == "running" {
			job.Step = "family-verify"
		}
	}
	if err := saveCNFamilyJob(account, job); err != nil {
		return map[string]any{"ok": false, "reason": "family_job_save_failed"}
	}
	if future {
		policy := s.cnFamilyPolicy()
		if !policy.Enabled || policy.Organizer != organizer {
			policy = cnFamilyPolicy{Enabled: true, Organizer: organizer}
			s.accountsMu.Lock()
			for _, a := range s.accounts {
				policy.Existing = append(policy.Existing, a.ID)
			}
			s.accountsMu.Unlock()
		}
		if writeRegisterJSON(filepath.Join(s.config.Dir, "cn-family-policy.json"), policy) != nil {
			return map[string]any{"ok": false, "reason": "family_job_save_failed"}
		}
	}
	return map[string]any{"ok": true, "job": job}
}
func advanceCNFamilyJob(ctx context.Context, account *Account, job *cnFamilyJob, call func(context.Context, string) map[string]any) {
	if job.State == "completed" || job.State == "blocked" || job.State == "canceled" {
		return
	}
	if job.State == "running" {
		job.Step = "family-verify"
	}
	job.State = "running"
	job.Reason = ""
	if saveCNFamilyJob(account, job) != nil {
		return
	}
	result := call(ctx, job.Step)
	if result["ok"] != true {
		job.State = "blocked"
		job.Reason, _ = result["reason"].(string)
		if job.Reason == "" {
			job.Reason = "family_readback_required"
		}
	} else {
		switch job.Step {
		case "family-join":
			job.Step = "family-join-storage"
			job.State = "waiting"
		case "family-join-storage":
			job.Step = "family-verify"
			job.State = "waiting"
		case "family-verify":
			job.State = "completed"
		default:
			job.State = "blocked"
			job.Reason = "family_action_invalid"
		}
	}
	_ = saveCNFamilyJob(account, job)
}
func (s *Server) maintainCNFamily(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.runCNFamilyJobs(ctx)
	}
}
func (s *Server) runCNFamilyJobs(ctx context.Context) {
	// Serialize with manual steps so children cannot simultaneously claim the last slot.
	if !s.handoffMu.TryLock() {
		return
	}
	defer s.handoffMu.Unlock()
	policy := s.cnFamilyPolicy()
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	for _, account := range accounts {
		if ctx.Err() != nil {
			return
		}
		if accountguard.Check(account.Dir) != nil || s.registrationInProgress(account) || s.pipelineAccount(account) {
			continue
		}
		job := readCNFamilyJob(account)
		if job == nil && policy.Enabled && account.ID != policy.Organizer && !slices.Contains(policy.Existing, account.ID) {
			_ = s.queueCNFamily(account, policy.Organizer, false)
			job = readCNFamilyJob(account)
		}
		if job == nil || job.State == "blocked" || job.State == "completed" || job.State == "canceled" {
			continue
		}
		if !acceptedCheckpointReady(account.Dir, account.AppleID) {
			continue
		}
		status := s.accountStatus(account)
		if !status.Running || !status.Authenticated || status.VerificationRequired {
			continue
		}
		stepCtx, cancel := context.WithTimeout(ctx, 235*time.Second)
		input := handoffInput{Email: account.AppleID, Family: registrationFamily{Role: "member", Region: "CN", Organizer: job.Organizer}}
		advanceCNFamilyJob(stepCtx, account, job, func(c context.Context, step string) map[string]any {
			return s.registrationFamilyAction(c, account, input, step, "")
		})
		if job.State == "completed" {
			_ = s.familyAction(stepCtx, account, false)
		}
		cancel()
	}
}
func (s *Server) serveCNFamilyBackground(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account, Organizer, Step string
		AcceptTerms              bool `json:"accept_terms"`
		AutoFuture               bool `json:"auto_future"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	result := map[string]any{"ok": false, "reason": "family_binding_invalid"}
	account := s.lookup(body.Account)
	if account != nil && body.Account != "" && !s.registrationInProgress(account) {
		if s.handoffMu.TryLock() {
			defer s.handoffMu.Unlock()
			switch body.Step {
			case "enqueue":
				if body.AcceptTerms {
					result = s.queueCNFamily(account, body.Organizer, body.AutoFuture)
				} else {
					result = map[string]any{"ok": false, "reason": "family_consent_required"}
				}
			case "cancel-job":
				result = map[string]any{"ok": true}
				if job := readCNFamilyJob(account); job != nil {
					job.State = "canceled"
					if saveCNFamilyJob(account, job) != nil {
						result = map[string]any{"ok": false, "reason": "family_job_save_failed"}
					}
				}
			case "disable-default":
				p := s.cnFamilyPolicy()
				p.Enabled = false
				result = map[string]any{"ok": true}
				if writeRegisterJSON(filepath.Join(s.config.Dir, "cn-family-policy.json"), p) != nil {
					result = map[string]any{"ok": false, "reason": "family_job_save_failed"}
				}
			}
		} else {
			result = map[string]any{"ok": false, "reason": "family_busy"}
		}
	}
	if reason, ok := result["reason"].(string); ok {
		result["message"] = registerMessages[reason]
	}
	json.NewEncoder(w).Encode(result)
}
