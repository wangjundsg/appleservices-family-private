package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/internal/control"
)

func refreshFamilyAuthentication(parent context.Context, dir, appleID, password string) control.Result {
	if password == "" {
		return control.Result{Stage: "reconnect-required", Message: "请带密码重新连接母号以刷新家庭认证；密码不会写入文件"}
	}
	executable, err := os.Executable()
	if err != nil {
		return control.Result{Stage: "family-auth", Message: "家庭认证组件不可用"}
	}
	runDir, err := os.MkdirTemp(dir, ".family-auth-")
	if err != nil {
		return control.Result{Stage: "family-auth", Message: "无法创建家庭认证工作区"}
	}
	defer os.RemoveAll(runDir)
	if protectActivationPath(runDir, true) != nil {
		return control.Result{Stage: "family-auth", Message: "无法保护私有认证目录"}
	}
	ctx, cancel := context.WithTimeout(parent, 150*time.Second)
	defer cancel()
	checkpoint := filepath.Join(runDir, "gsa.json")
	_, err = runFamilyHelper(ctx, filepath.Join(filepath.Dir(executable), "accountsetupcheck.exe"),
		[]string{"-dir", dir, "-local-trusted-code", "-checkpoint", checkpoint, "-dump", filepath.Join(runDir, "setup.html")},
		[]string{"APPLESERVICES_APPLEID=" + appleID, "APPLESERVICES_PASSWORD=" + password})
	var session appleservices.Session
	var saved appleservices.Session
	savedData, savedErr := os.ReadFile(filepath.Join(dir, "session.json"))
	data, readErr := os.ReadFile(checkpoint)
	if savedErr != nil || json.Unmarshal(savedData, &saved) != nil || err != nil || readErr != nil || json.Unmarshal(data, &session) != nil || !strings.EqualFold(session.AppleID, appleID) || session.DSID == "" || session.DSID != saved.DSID || session.Tokens["com.apple.gs.icloud.family.auth"] == "" || session.Tokens["com.apple.gs.icloud.auth"] == "" {
		return control.Result{Stage: "family-auth", Message: "家庭认证刷新未完成，请核实当前账号验证状态"}
	}
	if os.Rename(checkpoint, filepath.Join(dir, "family-gsa.json")) != nil {
		return control.Result{Stage: "family-auth", Message: "家庭认证已通过，但私有会话保存失败"}
	}
	return control.Result{OK: true, Stage: "family-auth-ready", Message: "家庭认证已刷新；未变更家庭成员"}
}
