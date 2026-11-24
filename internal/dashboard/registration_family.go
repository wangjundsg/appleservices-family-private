package dashboard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/control"
)

var errFamilyCredentials = errors.New("family credentials required")

type registrationFamily struct {
	Role               string `json:"role"`
	Region             string `json:"region,omitempty"`
	Organizer          string `json:"organizer,omitempty"`
	AcceptSubscription bool   `json:"acceptSubscription,omitempty"`
}

func (f registrationFamily) validate() error {
	switch f.Role {
	case "", "none":
		if f.Organizer != "" || f.AcceptSubscription || f.Region != "" {
			return errors.New("仅激活任务不能携带家庭或订阅授权")
		}
	case "organizer":
		if f.Organizer != "" || !f.AcceptSubscription || (f.Region != "US" && f.Region != "CN") {
			return errors.New("母号需确认现有限定的 50GB 月付订阅及自动续费")
		}
	case "member":
		if f.Organizer == "" || len(f.Organizer) > 100 || f.AcceptSubscription || (f.Region != "" && f.Region != "US" && f.Region != "CN") {
			return errors.New("成员需绑定母号，不能授权购买订阅")
		}
	default:
		return errors.New("不支持的家庭用途")
	}
	return nil
}

type registrationFamilyReady struct {
	Account string `json:"account"`
	Region  string `json:"region"`
}

const registrationPlanBytes uint64 = 50 * 1024 * 1024 * 1024

// Same targets as the existing dashboard purchase buttons; never choose a larger offer.
func registrationPlanAction(plans *icloud.StoragePlanInfo) string {
	if plans == nil {
		return ""
	}
	selected := ""
	for _, plan := range plans.Plans {
		if plan.Bytes != registrationPlanBytes || !plan.Available || plan.Period != "month" {
			continue
		}
		action := ""
		if plan.Currency == "CNY" && plan.Price == 600 {
			action = "storage-skill-purchase-cn"
		}
		if plan.Currency == "USD" && plan.Price == 99 {
			action = "storage-skill-purchase-us"
		}
		if action != "" {
			if selected != "" && selected != action {
				return ""
			}
			selected = action
		}
	}
	return selected
}

func sharedFamilyReady(f *control.FamilyInfo) bool {
	return f != nil && f.Member && f.Self == 1 && f.Organizers == 1 && f.StorageKnown && f.StorageParticipating && f.StorageAvailable && f.StorageEnabled && f.StorageBytes >= int64(registrationPlanBytes)
}

func (s *Server) readyFamilyOrganizer(id string) bool {
	account := s.lookup(id)
	if account == nil || s.registrationInProgress(account) {
		return false
	}
	var receipt registrationFamilyReady
	return readRegisterJSON(filepath.Join(account.Dir, "family-ready.json"), &receipt) == nil && receipt.Account == id && (receipt.Region == "US" || receipt.Region == "CN") && acceptedCheckpointReady(account.Dir, account.AppleID)
}

func (s *Server) familyOrganizers() []map[string]string {
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	result := make([]map[string]string, 0)
	for _, account := range accounts {
		if s.readyFamilyOrganizer(account.ID) {
			result = append(result, map[string]string{"id": account.ID, "name": account.Name})
		}
	}
	return result
}

// The adapters are also the offline test boundary; tests never start real helpers.
type registrationFamilyFlow struct {
	allowSubscription bool
	account           *Account
	organizer         *Account
	input             handoffInput
	worker            func(string) control.Result
	family            func(*Account, ...string) (*control.FamilyInfo, error)
}

func (f registrationFamilyFlow) run(action, selectedPlan string) map[string]any {
	fail := func(reason string) map[string]any {
		return map[string]any{"ok": false, "state": "unknown", "reason": reason}
	}
	ok := map[string]any{"ok": true}
	if f.account == nil || f.input.Family.validate() != nil {
		return fail("family_binding_invalid")
	}
	role := f.input.Family.Role
	organizerAction := strings.HasPrefix(action, "subscription-") || action == "family-create" || action == "family-share"
	if organizerAction && (role != "organizer" || !f.input.Family.AcceptSubscription) {
		return fail("family_role_mismatch")
	}
	if strings.HasPrefix(action, "subscription-") && action != "subscription-plan" && action != "subscription-verify" && action != "subscription-cancel" && !f.allowSubscription {
		return fail("subscription_disabled")
	}
	paid := func() bool {
		r := f.worker("storage-info")
		return r.OK && r.Storage != nil && r.Storage.PaidBytes >= registrationPlanBytes
	}
	switch action {
	case "subscription-plan":
		current, err := f.family(f.account)
		if err != nil || current == nil || (current.Member && !current.Organizer) {
			return fail("family_binding_invalid")
		}
		storage := f.worker("storage-info")
		if !storage.OK || storage.Storage == nil {
			return fail("subscription_readback_required")
		}
		if storage.Storage.PaidBytes >= registrationPlanBytes {
			return map[string]any{"ok": true, "existing": true}
		}
		if storage.Storage.PaidBytes != 0 {
			return fail("subscription_offer_unsupported")
		}
		plans := f.worker("storage-plan")
		if !plans.OK {
			return fail("subscription_offer_unsupported")
		}
		selected := registrationPlanAction(plans.StoragePlan)
		if selected == "" || !f.matchesRegion(selected) {
			return fail("subscription_offer_unsupported")
		}
		return map[string]any{"ok": true, "purchase_action": selected}
	case "subscription-auth":
		if selectedPlan != "storage-skill-purchase-us" && selectedPlan != "storage-skill-purchase-cn" {
			return fail("subscription_offer_unsupported")
		}
		if !f.worker("storage-skill-auth-local").OK {
			return fail("subscription_auth_required")
		}
	case "subscription-buy":
		storage := f.worker("storage-info")
		if !storage.OK || storage.Storage == nil {
			return fail("subscription_readback_required")
		}
		if storage.Storage.PaidBytes >= registrationPlanBytes {
			return ok
		}
		if storage.Storage.PaidBytes != 0 {
			return fail("subscription_offer_unsupported")
		}
		plans := f.worker("storage-plan")
		if !plans.OK || selectedPlan == "" || registrationPlanAction(plans.StoragePlan) != selectedPlan || !f.matchesRegion(selectedPlan) {
			return fail("subscription_offer_unsupported")
		}
		result := f.worker(selectedPlan)
		if !result.OK || result.Purchase == nil || (result.Purchase.State != "completed" && result.Purchase.State != "existing-paid-plan") {
			return fail("subscription_readback_required")
		}
	case "subscription-verify":
		if !paid() {
			return fail("subscription_readback_required")
		}
	case "subscription-cancel":
		if f.input.Family.Region != "CN" {
			return fail("subscription_offer_unsupported")
		}
		result := f.worker("storage-cancel-auto-renew")
		if !result.OK || result.Subscription == nil || !result.Subscription.Present || !result.Subscription.CN50GB || result.Subscription.AutoRenew {
			failure := fail("subscription_renewal_unconfirmed")
			if !result.OK && result.Message != "" {
				failure["message"] = result.Message
			}
			return failure
		}
	case "family-create":
		if !paid() {
			return fail("subscription_readback_required")
		}
		current, err := f.family(f.account)
		if err != nil || current == nil {
			return fail("family_readback_required")
		}
		if current.Member && !current.Organizer {
			return fail("family_binding_invalid")
		}
		if !current.Member {
			current, err = f.family(f.account, "-create-family")
		}
		if err != nil || current == nil || !current.Organizer || !current.Member || current.Self != 1 || current.Organizers != 1 {
			return fail("family_readback_required")
		}
	case "family-share":
		if !paid() {
			return fail("subscription_readback_required")
		}
		current, err := f.family(f.account)
		if err != nil || current == nil || !current.Organizer {
			return fail("family_binding_invalid")
		}
		if !sharedFamilyReady(current) {
			current, err = f.family(f.account, "-share-icloud-storage")
		}
		if err != nil || !sharedFamilyReady(current) || !current.Organizer {
			return fail("family_readback_required")
		}
	case "family-join":
		if role != "member" || f.organizer == nil || f.organizer.ID != f.input.Family.Organizer || f.organizer.ID == f.account.ID {
			return fail("family_binding_invalid")
		}
		mother, err := f.family(f.organizer)
		if err != nil || !sharedFamilyReady(mother) || !mother.Organizer {
			return fail("family_organizer_not_ready")
		}
		current, err := f.family(f.account)
		if err != nil || current == nil {
			return fail("family_readback_required")
		}
		if current.Member {
			if current.Organizer {
				return fail("family_binding_invalid")
			}
			if _, err := f.family(f.organizer, "-verify-pool-member-dir", f.account.Dir); err != nil {
				return fail("family_binding_invalid")
			}
			return ok
		}
		if mother.Members+mother.Pending+mother.Invitations >= 6 {
			return fail("family_full")
		}
		// This existing helper verifies the exact organizer and target on both sides.
		if _, err = f.family(f.organizer, "-add-pool-member-dir", f.account.Dir, "-target-password-env"); err != nil {
			if errors.Is(err, errFamilyCredentials) {
				return fail("family_credentials_required")
			}
			return fail("family_readback_required")
		}
	case "family-join-storage":
		if role != "member" || f.organizer == nil {
			return fail("family_binding_invalid")
		}
		if _, err := f.family(f.organizer, "-verify-pool-member-dir", f.account.Dir); err != nil {
			return fail("family_binding_invalid")
		}
		current, err := f.family(f.account)
		if err != nil || current == nil || !current.Member || current.Organizer {
			return fail("family_binding_invalid")
		}
		if !sharedFamilyReady(current) {
			current, err = f.family(f.account, "-join-shared-storage")
		}
		if err != nil || !sharedFamilyReady(current) {
			return fail("family_readback_required")
		}
	case "family-verify":
		if role != "member" && role != "organizer" {
			return fail("family_role_mismatch")
		}
		current, err := f.family(f.account)
		if err != nil || !sharedFamilyReady(current) || current.Organizer != (role == "organizer") {
			return fail("family_readback_required")
		}
		if role == "member" {
			if f.organizer == nil {
				return fail("family_binding_invalid")
			}
			if _, err := f.family(f.organizer, "-verify-pool-member-dir", f.account.Dir); err != nil {
				return fail("family_binding_invalid")
			}
		}
		if role == "organizer" {
			if writeRegisterJSON(filepath.Join(f.account.Dir, "family-ready.json"), registrationFamilyReady{Account: f.account.ID, Region: f.input.Family.Region}) != nil {
				return fail("family_readback_required")
			}
		}
	default:
		return fail("family_action_invalid")
	}
	return ok
}

func (f registrationFamilyFlow) matchesRegion(action string) bool {
	return (f.input.Family.Region == "CN" && action == "storage-skill-purchase-cn") || (f.input.Family.Region == "US" && action == "storage-skill-purchase-us")
}

func (s *Server) registrationFamilyAction(ctx context.Context, account *Account, input handoffInput, action, plan string) map[string]any {
	if account == nil || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return map[string]any{"ok": false, "state": "unknown", "reason": "family_binding_invalid"}
	}
	f := registrationFamilyFlow{allowSubscription: s.config.AllowSubscription, account: account, input: input}
	if input.Family.Role == "member" {
		candidate := s.lookup(input.Family.Organizer)
		if candidate != nil && candidate.ID != account.ID && !s.registrationInProgress(candidate) && acceptedCheckpointReady(candidate.Dir, candidate.AppleID) {
			f.organizer = candidate
		}
	}
	f.worker = func(action string) control.Result {
		var result control.Result
		if err := s.callFor(ctx, account.Dir, "/action", control.Action{Action: action}, &result); err != nil {
			return control.Result{Message: err.Error()}
		}
		return result
	}
	f.family = func(target *Account, extra ...string) (*control.FamilyInfo, error) {
		if len(extra) > 0 && extra[0] != "-verify-pool-member-dir" && extra[0] != "-join-shared-storage" {
			var refresh control.Result
			if s.callFor(ctx, target.Dir, "/action", control.Action{Action: "family-refresh-auth"}, &refresh) != nil || !refresh.OK {
				if refresh.Stage == "reconnect-required" {
					return nil, errFamilyCredentials
				}
				return nil, errors.New("family authentication refresh required")
			}
		}
		if len(extra) > 0 && extra[0] == "-add-pool-member-dir" && input.Password == "" {
			var result control.Result
			if s.callFor(ctx, account.Dir, "/action", control.Action{Action: "family-add-self", Code: target.ID}, &result) != nil || !result.OK || result.Family == nil {
				if result.Stage == "reconnect-required" {
					return nil, errFamilyCredentials
				}
				return nil, errors.New("target password or membership verification required")
			}
			return result.Family, nil
		}
		childCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		defer cancel()
		args := append([]string{"-dir", target.Dir, "-safe-json"}, extra...)
		command := exec.CommandContext(childCtx, filepath.Join(filepath.Dir(s.config.Worker), "familyinspect.exe"), args...)
		configureProcess(command)
		command.Env = os.Environ()
		if len(extra) > 0 && extra[0] == "-add-pool-member-dir" {
			command.Env = append(command.Env, "APPLESERVICES_TARGET_PASSWORD="+input.Password)
		}
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if command.Run() != nil {
			return nil, errors.New("family helper unconfirmed")
		}
		return parseFamilyStatus(output.String())
	}
	return f.run(action, plan)
}
