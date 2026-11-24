package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) waitingCNPayment(account *Account) bool {
	if account == nil {
		return false
	}
	var origin struct {
		JobID string `json:"job_id"`
	}
	if readRegisterJSON(filepath.Join(account.Dir, "registration-origin.json"), &origin) != nil || !registerIDPattern.MatchString(origin.JobID) {
		return false
	}
	var receipt pipelineReceipt
	var input handoffInput
	folder := s.registerFolder(origin.JobID)
	return readRegisterJSON(filepath.Join(folder, "result.json"), &receipt) == nil && readRegisterJSON(filepath.Join(folder, "input.json"), &input) == nil && !receipt.Active && receipt.AccountCreated && receipt.Status == "waiting_input" && receipt.Stage == "subscription" && receipt.Reason == "subscription_payment_required" && input.Family.Role == "organizer" && input.Family.Region == "CN"
}

func cnPaymentAction(action string) bool {
	switch action {
	case "storage-info", "storage-plan", "storage-skill-auth-local", "storage-skill-auth-file", "storage-skill-purchase-cn", "storage-subscription", "code":
		return true
	}
	return false
}

func (s *Server) resumeCNPayment(w http.ResponseWriter, r *http.Request, id string) {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	folder := s.registerFolder(id)
	var receipt pipelineReceipt
	var input handoffInput
	if readRegisterJSON(filepath.Join(folder, "result.json"), &receipt) != nil || readRegisterJSON(filepath.Join(folder, "input.json"), &input) != nil || receipt.Active || !receipt.AccountCreated || receipt.Stage != "subscription" || receipt.Reason != "subscription_payment_required" || input.Family.Region != "CN" || input.Family.Role != "organizer" {
		http.Error(w, `{"message":"当前任务不在等待国区付款阶段"}`, http.StatusConflict)
		return
	}
	if _, err := os.Stat(filepath.Join(folder, "cancel.request")); !os.IsNotExist(err) {
		http.Error(w, `{"message":"任务已请求取消"}`, http.StatusConflict)
		return
	}
	// Refresh only the local panel bridge after a dashboard restart.
	var manifest map[string]any
	if readRegisterJSON(filepath.Join(folder, "input.json"), &manifest) != nil {
		http.Error(w, `{"message":"任务参数不可用"}`, 500)
		return
	}
	manifest["panel_bridge"] = map[string]string{"url": s.URL() + "/api/register/" + id + "/handoff", "token": s.token}
	if writeRegisterJSON(filepath.Join(folder, "input.json"), manifest) != nil {
		http.Error(w, `{"message":"任务连接更新失败"}`, 500)
		return
	}
	command := s.registerCommand("resume", "--job", id)
	if command.Start() != nil {
		http.Error(w, `{"message":"续跑启动失败"}`, 500)
		return
	}
	go func() {
		_ = command.Wait()
		s.registerMu.Lock()
		defer s.registerMu.Unlock()
		var meta registerMeta
		_ = readRegisterJSON(filepath.Join(folder, "panel.json"), &meta)
		code := command.ProcessState.ExitCode()
		now := time.Now().UTC()
		meta.ExitCode, meta.FinishedAt = &code, &now
		_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
	}()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "正在核验付款容量并继续；不会重复下单"})
}
