package main

import (
	"context"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/accountguard"
	"github.com/Laky-64/appleservices/internal/control"
)

// Manual generation uses the local provider, not device-list authentication.
func localCodeResult(ctx context.Context, dir string, generate func(context.Context) (string, error)) control.Result {
	if accountguard.Check(dir) != nil {
		return control.Result{Stage: "account_protected", Message: accountguard.Message}
	}
	if generate == nil {
		return control.Result{Stage: "code_provider_unavailable", Message: "本机验证码模块尚未就绪"}
	}
	if ctx.Err() != nil {
		return control.Result{Stage: "canceled", Message: "验证码生成已取消"}
	}
	generationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, err := generate(generationCtx)
	if err != nil || len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return control.Result{Stage: "code_generation_failed", Message: "本机验证码生成失败；未重新登录或发送短信"}
	}
	if generationCtx.Err() != nil {
		return control.Result{Stage: "canceled", Message: "验证码生成已取消"}
	}
	if accountguard.Check(dir) != nil {
		return control.Result{Stage: "account_protected", Message: accountguard.Message}
	}
	return control.Result{OK: true, Code: code, HideAt: time.Now().Add(time.Minute).UTC(), Message: "本机验证码已生成；Apple 是否接受尚未验证"}
}
