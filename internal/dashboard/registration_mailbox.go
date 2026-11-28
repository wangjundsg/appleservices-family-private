package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) resumeMailbox(w http.ResponseWriter, r *http.Request, id string) {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	folder := s.registerFolder(id)
	var receipt pipelineReceipt
	var manifest map[string]any
	if !s.config.AllowPurchase || readRegisterJSON(filepath.Join(folder, "result.json"), &receipt) != nil ||
		readRegisterJSON(filepath.Join(folder, "input.json"), &manifest) != nil || receipt.Active || receipt.AccountCreated ||
		receipt.RegistrationStarted || receipt.Stage != "mailbox" || receipt.Status == "cancelled" || manifest["mail_purchase"] == nil {
		http.Error(w, `{"message":"仅开启购买开关且停在购邮阶段的原任务可以继续"}`, http.StatusConflict)
		return
	}
	if _, err := os.Stat(filepath.Join(folder, "cancel.request")); !os.IsNotExist(err) {
		http.Error(w, `{"message":"任务已请求取消"}`, http.StatusConflict)
		return
	}
	// Only refresh the panel connection; resource and purchase settings remain frozen.
	manifest["panel_bridge"] = map[string]string{"url": s.URL() + "/api/register/" + id + "/handoff", "token": s.token}
	if writeRegisterJSON(filepath.Join(folder, "input.json"), manifest) != nil {
		http.Error(w, `{"message":"任务连接更新失败"}`, 500)
		return
	}
	command := s.registerCommand("resume", "--job", id, "--allow-purchase")
	for _, pair := range [][2]string{{"SMS_API_KEY_HERO", "hero"}, {"SMS_API_KEY", "grizzly"}} {
		if key := smsKey(pair[1]); key != "" {
			command.Env = append(command.Env, pair[0]+"="+key)
		}
	}
	// Reserve this resume before launching; a second click must not rewrite input.json.
	var running map[string]any
	if readRegisterJSON(filepath.Join(folder, "result.json"), &running) != nil || running == nil {
		http.Error(w, `{"message":"购邮任务状态不可用"}`, 500)
		return
	}
	running["active"], running["heartbeat_at"] = true, time.Now().UTC()
	if writeRegisterJSON(filepath.Join(folder, "result.json"), running) != nil {
		http.Error(w, `{"message":"购邮任务状态未保存"}`, 500)
		return
	}
	if command.Start() != nil {
		running["active"] = false
		_ = writeRegisterJSON(filepath.Join(folder, "result.json"), running)
		http.Error(w, `{"message":"购邮续跑启动失败"}`, 500)
		return
	}
	go func() {
		_ = command.Wait()
		s.registerMu.Lock()
		defer s.registerMu.Unlock()
		var meta registerMeta
		_ = readRegisterJSON(filepath.Join(folder, "panel.json"), &meta)
		code, now := command.ProcessState.ExitCode(), time.Now().UTC()
		meta.ExitCode, meta.FinishedAt = &code, &now
		_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
	}()
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "继续原任务，先核对原邮箱订单"})
}
