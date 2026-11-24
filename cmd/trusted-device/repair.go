package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/appleweb"
	"github.com/Laky-64/appleservices/internal/control"
)

type repairAuth interface {
	Start(context.Context, appleweb.AuthFlow, string, string, string, string, string) (appleweb.AuthStartResult, error)
	Submit2FA(context.Context, string, string, json.RawMessage) (appleweb.ICloudSession, error)
	Terms(context.Context, string) (appleweb.ICloudTerms, error)
	AcceptTerms(context.Context, string, string, bool) (appleweb.ICloudSession, error)
}

// Only an explicitly identified web terms challenge is accepted. Other repair
// requirements remain failures; an HTTP success alone is not a repaired account.
func repairAccount(ctx context.Context, auth repairAuth, account, password string, verify func(func(string) error) error) error {
	start, err := auth.Start(ctx, appleweb.AuthFlowICloudWeb, account, password, "www.icloud.com", "", "sms")
	if err != nil {
		return errors.New("account_repair_web_login_failed")
	}
	session := start.Session
	needsTerms := start.NeedsTerms
	if start.Needs2FA {
		err = verify(func(code string) error {
			var submitErr error
			session, submitErr = auth.Submit2FA(ctx, start.PendingID, code, nil)
			if name, _, _ := appleweb.ErrorDetails(submitErr); name == "icloud_terms_update_needed" {
				needsTerms = true
				return nil
			}
			return submitErr
		})
		if err != nil {
			return errors.New("account_repair_verification_incomplete")
		}
	}
	if needsTerms {
		terms, e := auth.Terms(ctx, start.PendingID)
		if e != nil {
			return errors.New("account_repair_terms_unavailable")
		}
		session, err = auth.AcceptTerms(ctx, start.PendingID, terms.Version, true)
		if err != nil {
			return errors.New("account_repair_terms_not_accepted")
		}
	}
	if !strings.EqualFold(session.AppleID, account) || session.DSID == "" || len(session.Cookies) == 0 {
		return errors.New("account_repair_session_unconfirmed")
	}
	return nil
}

func repairPanelCode(ctx context.Context, commands <-chan control.Command, status *runtimeStatus, dir string, verify func(string) error) error {
	status.State, status.VerificationRequired = "awaiting_repair_sms", true
	if err := status.save(dir); err != nil {
		return err
	}
	defer func() { status.VerificationRequired = false }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case command := <-commands:
			if command.Context.Err() != nil {
				continue
			}
			if command.Action.Action != "submit-sms" {
				command.Reply <- control.Result{Message: "请先输入本次账号设置验证短信；不会自动重发"}
				continue
			}
			if err := verify(command.Code); err != nil {
				command.Reply <- control.Result{Message: "账号设置验证未完成，请重新连接后核对 Apple 提示"}
				return errors.New("account_repair_verification_failed")
			}
			command.Reply <- control.Result{OK: true, Message: "账号设置验证码已通过，正在继续登录"}
			status.State = "repairing_account"
			return status.save(dir)
		}
	}
}

func runAccountRepair(ctx context.Context, account, password, dir string, commands <-chan control.Command, status *runtimeStatus) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	status.State = "repairing_account"
	if err := status.save(dir); err != nil {
		return err
	}
	return repairAccount(ctx, appleweb.NewAuthFacade(), account, password, func(verify func(string) error) error {
		return repairPanelCode(ctx, commands, status, dir, verify)
	})
}
