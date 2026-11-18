package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/control"
)

var errVerificationRequired = errors.New("account verification is required; complete it in the dashboard")

func deviceListFailure(err error) control.Result {
	var requestErr *gsa.DeviceRequestError
	if errors.Is(err, appleservices.ErrDeviceIdentityRefreshRequired) || errors.As(err, &requestErr) && requestErr.HTTPStatus == 401 {
		return control.Result{Stage: "device_identity_refresh_required", Message: "设备管理登录态需要补齐：请重新连接此账号一次。成功后会保存该凭据，后续重启不再重复输入密码"}
	}
	return control.Result{Stage: "device_query_failed", Message: "设备列表获取失败，请稍后重试"}
}

func reservedAliasPresent(info *icloud.HideMyEmailInfo, candidate, label, note string) bool {
	if info == nil || strings.TrimSpace(candidate) == "" || strings.TrimSpace(label) == "" {
		return false
	}
	matches := 0
	for _, alias := range info.Aliases {
		if strings.EqualFold(strings.TrimSpace(alias.Email), strings.TrimSpace(candidate)) && alias.Label == label && alias.Note == note && alias.Active {
			matches++
		}
	}
	return matches == 1
}

func hmeForwardTargetAvailable(info *icloud.HideMyEmailInfo, target string) bool {
	if info == nil || strings.TrimSpace(target) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(info.SelectedForward), strings.TrimSpace(target)) {
		return true
	}
	for _, candidate := range info.ForwardToEmails {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func reachableVerificationApplied(verifyErr error, info *icloud.HideMyEmailInfo, target string) bool {
	return verifyErr != nil && hmeForwardTargetAvailable(info, target)
}

type maintainedLogin interface {
	NeedsTwoFactor() bool
	Client() (*appleservices.Client, error)
	RegisterDevice(appleservices.DeviceRegistration) error
	AccountDevices() ([]appleservices.AccountDevice, error)
	SaveSession() error
	RespondToSignIn(string, bool) error
	BeginAccountValidation() error
	SubmitCode(appleservices.TwoFactorMethod, string) error
}

// A renewal is adopted only after authentication, registration and device-list
// access all succeed. Callers keep the old session on a transient error.
func renewSession(begin func() (maintainedLogin, error), accountID string, registration appleservices.DeviceRegistration) (maintainedLogin, []appleservices.AccountDevice, error) {
	return renewSessionMode(begin, accountID, registration, true)
}

func renewSessionMode(begin func() (maintainedLogin, error), accountID string, registration appleservices.DeviceRegistration, register bool) (maintainedLogin, []appleservices.AccountDevice, error) {
	login, err := begin()
	if err != nil {
		return nil, nil, err
	}
	if login.NeedsTwoFactor() {
		return nil, nil, errVerificationRequired
	}
	client, err := login.Client()
	if err != nil {
		return nil, nil, err
	}
	if client == nil || client.ADSID != accountID {
		return nil, nil, errors.New("session renewal returned a different account")
	}
	if register {
		if err := login.RegisterDevice(registration); err != nil {
			return nil, nil, err
		}
	}
	devices, err := login.AccountDevices()
	if err != nil {
		return nil, nil, err
	}
	if err := login.SaveSession(); err != nil {
		return nil, nil, err
	}
	return login, devices, nil
}

func validationFailure(err error) control.Result {
	switch {
	case errors.Is(err, appleservices.ErrValidationPasswordRequired):
		return control.Result{Stage: "reconnect-required", Message: "当前会话缺少重新验证所需的密码，请手动连接此账号。"}
	case errors.Is(err, gsa.ErrAccountRepairRequired):
		return control.Result{Stage: gsa.ErrAccountRepairRequired.Error(), Message: "Apple 要求先完成账号设置。请在官方账户页面或受信任设备上完成提示，再手动连接。"}
	default:
		return control.Result{Stage: "account_validation_unconfirmed", Message: "账户验证未完成，已保留当前会话。"}
	}
}

// Verification failures are public results. Never echo a transport error or
// service body: both can contain the submitted code or session material.
func verificationFailure(err error) (control.Result, bool) {
	var failure *gsa.SecondFactorError
	switch {
	case errors.Is(err, appleservices.ErrVerificationCodeFormat):
		return control.Result{Stage: "verification_input_invalid", Message: "请输入六位数字；验证码尚未提交。"}, false
	case errors.Is(err, gsa.ErrAccountRepairRequired):
		return validationFailure(gsa.ErrAccountRepairRequired), true
	case errors.Is(err, appleservices.ErrVerifiedSessionIncomplete):
		return control.Result{Stage: "verified_session_incomplete", Message: "验证码提交已获接受，后续账号会话未能确认或保存。已停止本次验证。"}, true
	case errors.As(err, &failure):
		stop := failure.HTTPStatus == 412 || failure.HTTPStatus == 423 || failure.HTTPStatus == 429 || failure.SecurityCodeLocked || failure.TooManyCodesValidated || failure.TooManyCodesSent
		if stop {
			return control.Result{Stage: "verification_blocked", HTTPStatus: failure.HTTPStatus, Message: "Apple 未允许本次验证继续，已停止提交。"}, true
		}
		return control.Result{Stage: "verification_unconfirmed", HTTPStatus: failure.HTTPStatus, Message: "本次验证码未取得成功回执，已停止提交。"}, true
	default:
		return control.Result{Stage: "verification_unconfirmed", Message: "本次验证码提交结果未确认，已停止以免重复提交。"}, true
	}
}

type runtimeStatus struct {
	PID                     int       `json:"pid"`
	Running                 bool      `json:"running"`
	PushConnected           bool      `json:"push_connected"`
	Authenticated           bool      `json:"authenticated"`
	VerificationRequired    bool      `json:"verification_required"`
	State                   string    `json:"state"`
	UpdatedAt               time.Time `json:"updated_at"`
	LastRefreshAt           time.Time `json:"last_refresh_at"`
	LastDeviceQueryAt       time.Time `json:"last_device_query_at"`
	DeviceCount             int       `json:"device_count"`
	InstallationVisible     bool      `json:"installation_visible"`
	RefreshCount            int       `json:"refresh_count"`
	PairingUnsupported      bool      `json:"pairing_unsupported"`
	PairingState            string    `json:"pairing_state,omitempty"`
	PushReconnectCount      int       `json:"push_reconnect_count"`
	UnsupportedNoticeCount  int       `json:"unsupported_notice_count"`
	LastUnsupportedNoticeAt time.Time `json:"last_unsupported_notice_at,omitempty"`
	PushIssue               string    `json:"push_issue,omitempty"`
	FailureStage            string    `json:"failure_stage,omitempty"`
	FailureReason           string    `json:"failure_reason,omitempty"`
}

func (s *runtimeStatus) setDevices(devices []appleservices.AccountDevice, name string) {
	s.DeviceCount = len(devices)
	s.InstallationVisible = false
	for _, device := range devices {
		s.InstallationVisible = s.InstallationVisible || device.Name == name
	}
	s.LastDeviceQueryAt = time.Now().UTC()
}

func writeJSONFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (s *runtimeStatus) save(dir string) error {
	s.UpdatedAt = time.Now().UTC()
	if err := writeJSONFile(filepath.Join(dir, "runtime.json"), s); err != nil {
		return errors.New("could not save local runtime status")
	}
	return nil
}

func stopRequested(dir string, pid int) bool {
	path := filepath.Join(dir, "stop-request.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var request struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(data, &request) != nil || request.PID != pid {
		return false
	}
	os.Remove(path)
	return true
}

func watchStop(ctx context.Context, cancel context.CancelFunc, dir string, pid int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if stopRequested(dir, pid) {
				cancel()
				return
			}
		}
	}
}
