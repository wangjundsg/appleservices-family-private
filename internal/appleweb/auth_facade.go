package appleweb

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type AuthFlow string

const (
	AuthFlowICloudWeb    AuthFlow = "icloud_web"
	AuthFlowAppleAccount AuthFlow = "apple_account"
)

type AuthStartResult struct {
	Session    ICloudSession `json:"-"`
	PendingID  string        `json:"pending_id,omitempty"`
	Needs2FA   bool          `json:"needs_2fa"`
	NeedsTerms bool          `json:"needs_terms"`
	Message    string        `json:"message"`
	AppleID    string        `json:"apple_id"`
	ExpiresAt  time.Time     `json:"expires_at,omitempty"`
}

type AuthFacade struct {
	client  *AppleAuthClient
	pending *appleAuthPendingStore
}

func NewAuthFacade() *AuthFacade {
	return &AuthFacade{client: NewAppleAuthClient(), pending: newAppleAuthPendingStore()}
}

func (f *AuthFacade) Start(ctx context.Context, flow AuthFlow, appleID, password, defaultHost, clientID, twoFactorMethod string) (AuthStartResult, error) {
	if flow != AuthFlowICloudWeb {
		return AuthStartResult{}, errCode("unsupported_flow", "only iCloud web login is supported", false)
	}
	result, err := f.client.StartLogin(ctx, appleID, password, defaultHost, clientID, f.pending, twoFactorMethod)
	if err != nil {
		return AuthStartResult{}, err
	}
	return AuthStartResult{
		Session:    result.Session,
		PendingID:  result.PendingID,
		Needs2FA:   result.Needs2FA,
		NeedsTerms: result.NeedsTerms,
		Message:    result.Message,
		AppleID:    result.AppleID,
		ExpiresAt:  result.ExpiresAt,
	}, nil
}

func (f *AuthFacade) Submit2FA(ctx context.Context, pendingID, code string, phoneNumber json.RawMessage) (ICloudSession, error) {
	pendingID = strings.TrimSpace(pendingID)
	pending, ok := f.pending.get(pendingID)
	if !ok {
		return ICloudSession{}, errCode("apple_login_pending_expired", "Apple 登录已过期，请重新输入账号密码发起登录", true)
	}
	pending.Session.mu.Lock()
	defer pending.Session.mu.Unlock()
	session, err := f.client.Submit2FA(ctx, pending, code, phoneNumber)
	if err != nil {
		return ICloudSession{}, err
	}
	f.pending.delete(pendingID)
	return session, nil
}

func (f *AuthFacade) PendingTermsResult(pendingID string) (AuthStartResult, bool) {
	pending, ok := f.pending.get(pendingID)
	if !ok {
		return AuthStartResult{}, false
	}
	pending.Session.mu.Lock()
	defer pending.Session.mu.Unlock()
	if !pending.Session.TermsRequired {
		return AuthStartResult{}, false
	}
	return AuthStartResult{PendingID: pending.ID, NeedsTerms: true, AppleID: pending.Session.AppleID, ExpiresAt: pending.ExpiresAt, Message: "验证码验证已完成，请阅读并确认 iCloud 条款"}, true
}

func (f *AuthFacade) Terms(ctx context.Context, pendingID string) (ICloudTerms, error) {
	pending, ok := f.pending.get(pendingID)
	if !ok {
		return ICloudTerms{}, errCode("apple_login_pending_expired", "登录已过期，请重新发起登录", true)
	}
	pending.Session.mu.Lock()
	defer pending.Session.mu.Unlock()
	return f.client.fetchICloudTerms(ctx, pending.Session)
}

func (f *AuthFacade) AcceptTerms(ctx context.Context, pendingID, version string, accepted bool) (ICloudSession, error) {
	pending, ok := f.pending.get(pendingID)
	if !ok {
		return ICloudSession{}, errCode("apple_login_pending_expired", "登录已过期，请重新发起登录", true)
	}
	pending.Session.mu.Lock()
	defer pending.Session.mu.Unlock()
	// A duplicate request may have waited while the first one completed login.
	if _, ok := f.pending.get(pending.ID); !ok {
		return ICloudSession{}, errCode("apple_login_pending_expired", "本次登录已完成或过期，请刷新账号列表", true)
	}
	session, err := f.client.acceptICloudTerms(ctx, pending.Session, version, accepted)
	if err != nil {
		return ICloudSession{}, err
	}
	f.pending.delete(pending.ID)
	return session, nil
}
