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
	"github.com/Laky-64/appleservices/internal/accountguard"
	"github.com/Laky-64/appleservices/internal/control"
	"github.com/Laky-64/appleservices/internal/privatefs"
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

type renewalError struct {
	stage string
	err   error
}

func (e *renewalError) Error() string { return "session check failed at " + e.stage }
func (e *renewalError) Unwrap() error { return e.err }

type renewalStep struct {
	Stage  string `json:"stage"`
	Result string `json:"result"`
}

func renewSessionMode(begin func() (maintainedLogin, error), accountID string, registration appleservices.DeviceRegistration, register bool) (maintainedLogin, []appleservices.AccountDevice, error) {
	return renewSessionObserved(begin, accountID, registration, register, func(string, string) {})
}

func renewSessionObserved(begin func() (maintainedLogin, error), accountID string, registration appleservices.DeviceRegistration, register bool, observe func(string, string)) (maintainedLogin, []appleservices.AccountDevice, error) {
	fail := func(stage string, err error) (maintainedLogin, []appleservices.AccountDevice, error) {
		observe(stage, "failed")
		return nil, nil, &renewalError{stage, err}
	}
	stage := "session_restore"
	if register {
		stage = "password_authentication"
	}
	login, err := begin()
	if err != nil {
		return fail(stage, err)
	}
	observe(stage, "completed")
	if login.NeedsTwoFactor() {
		return fail("authentication", errVerificationRequired)
	}
	client, err := login.Client()
	if err != nil {
		return fail("service_session", err)
	}
	if client == nil || client.ADSID != accountID {
		return fail("account_binding", errors.New("account mismatch"))
	}
	observe("service_session", "available")
	if register {
		if err := login.RegisterDevice(registration); err != nil {
			return fail("device_registration", err)
		}
		observe("device_registration", "accepted")
	}
	devices, err := login.AccountDevices()
	if err != nil {
		return fail("device_query", err)
	}
	observe("device_query", "server_verified")
	if err := login.SaveSession(); err != nil {
		return fail("save_session", err)
	}
	observe("save_session", "saved")
	return login, devices, nil
}

// Only fixed categories and numeric status are public; never emit Apple bodies,
// transport URLs, credentials, tokens or arbitrary error text.
func renewalDiagnostic(err error) (stage, reason string, httpStatus int) {
	stage, reason = "unknown", "unconfirmed"
	var step *renewalError
	if errors.As(err, &step) {
		stage = step.stage
	}
	var deviceErr *gsa.DeviceRequestError
	var authErr *gsa.AuthenticationError
	switch {
	case errors.Is(err, accountguard.ErrBlocked):
		reason = "account_protected"
	case errors.Is(err, errVerificationRequired):
		reason = "verification_required"
	case errors.Is(err, appleservices.ErrDeviceIdentityRefreshRequired):
		reason = "device_identity_missing"
	case errors.Is(err, gsa.ErrAccountRepairRequired):
		reason = "account_repair_required"
	case errors.As(err, &deviceErr):
		reason = "device_request_rejected"
		httpStatus = deviceErr.HTTPStatus
	case errors.As(err, &authErr):
		reason = "authentication_rejected"
		httpStatus = authErr.HTTPStatus
	}
	return
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
	PID                     int           `json:"pid"`
	Running                 bool          `json:"running"`
	PushConnected           bool          `json:"push_connected"`
	Authenticated           bool          `json:"authenticated"`
	VerificationRequired    bool          `json:"verification_required"`
	State                   string        `json:"state"`
	UpdatedAt               time.Time     `json:"updated_at"`
	LastRefreshAt           time.Time     `json:"last_refresh_at"`
	LastDeviceQueryAt       time.Time     `json:"last_device_query_at"`
	DeviceCount             int           `json:"device_count"`
	InstallationVisible     bool          `json:"installation_visible"`
	RefreshCount            int           `json:"refresh_count"`
	PairingUnsupported      bool          `json:"pairing_unsupported"`
	PairingState            string        `json:"pairing_state,omitempty"`
	PushReconnectCount      int           `json:"push_reconnect_count"`
	UnsupportedNoticeCount  int           `json:"unsupported_notice_count"`
	LastUnsupportedNoticeAt time.Time     `json:"last_unsupported_notice_at,omitempty"`
	PushIssue               string        `json:"push_issue,omitempty"`
	LastAttemptAt           time.Time     `json:"last_attempt_at"`
	RenewalMode             string        `json:"renewal_mode,omitempty"`
	RenewalStage            string        `json:"renewal_stage,omitempty"`
	RenewalError            string        `json:"renewal_error,omitempty"`
	RenewalHTTPStatus       int           `json:"renewal_http_status,omitempty"`
	RenewalSteps            []renewalStep `json:"renewal_steps,omitempty"`
	AutomaticRefreshPaused  bool          `json:"automatic_refresh_paused"`
	FailureStage            string        `json:"failure_stage,omitempty"`
	FailureReason           string        `json:"failure_reason,omitempty"`
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
	if err := privatefs.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
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
	if accountguard.Check(dir) != nil {
		s.State = "account_locked"
		s.Authenticated = false
		s.FailureReason = "account_protected"
	}
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
			if accountguard.Check(dir) != nil || stopRequested(dir, pid) {
				cancel()
				return
			}
		}
	}
}
