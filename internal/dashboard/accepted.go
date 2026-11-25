package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	appleservices "github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/accountguard"
	"github.com/Laky-64/appleservices/internal/control"
	"github.com/Laky-64/appleservices/nativeadi"
	"howett.net/plist"
)

type acceptedICloudCheckpoint struct {
	Session     appleservices.Session `json:"session"`
	ADSID       string                `json:"adsid"`
	ServiceData map[string]any        `json:"service-data"`
}

func loadAcceptedCheckpoint(dir, appleID string) (appleservices.Session, acceptedICloudCheckpoint, error) {
	var saved appleservices.Session
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || !strings.EqualFold(saved.AppleID, appleID) || saved.DSID == "" {
		return appleservices.Session{}, acceptedICloudCheckpoint{}, errors.New("账号会话绑定无效")
	}
	var accepted acceptedICloudCheckpoint
	data, err = os.ReadFile(filepath.Join(dir, "icloud-accepted.json"))
	if err != nil || len(data) > 2<<20 || json.Unmarshal(data, &accepted) != nil ||
		!strings.EqualFold(accepted.Session.AppleID, saved.AppleID) || accepted.ADSID != saved.DSID ||
		accepted.Session.DSID == "" || accepted.Session.Tokens["mmeAuthToken"] == "" || accepted.ServiceData == nil {
		return appleservices.Session{}, acceptedICloudCheckpoint{}, errors.New("iCloud 验收会话不可用")
	}
	return saved, accepted, nil
}

func acceptedCheckpointReady(dir, appleID string) bool {
	_, _, err := loadAcceptedCheckpoint(dir, appleID)
	return err == nil
}

func (s *Server) acceptedStorageAction(parent context.Context, account *Account, includePlans bool) control.Result {
	if account != nil && accountguard.Check(account.Dir) != nil {
		return protectedResult()
	}
	if account == nil {
		return control.Result{Message: "账号不存在", Stage: "accepted-checkpoint"}
	}
	saved, accepted, err := loadAcceptedCheckpoint(account.Dir, account.AppleID)
	if err != nil {
		return control.Result{Message: err.Error(), Stage: "accepted-checkpoint"}
	}
	var device appleservices.Device
	data, err := os.ReadFile(filepath.Join(account.Dir, "device.json"))
	if err != nil || json.Unmarshal(data, &device) != nil || len(device.Identifier) != 16 || device.SerialNumber == "" {
		return control.Result{Message: "账号设备身份不可用", Stage: "accepted-checkpoint"}
	}
	var hardware struct {
		Push struct {
			Token []byte `plist:"token"`
		} `plist:"push"`
	}
	profile := filepath.Join(account.Dir, "native-adi")
	data, err = os.ReadFile(filepath.Join(profile, "hw_info.plist"))
	if err != nil {
		return control.Result{Message: "账号 Mac 身份不可用", Stage: "accepted-checkpoint"}
	}
	if _, err = plist.Unmarshal(data, &hardware); err != nil || len(hardware.Push.Token) != 32 {
		return control.Result{Message: "账号 APS 身份不可用", Stage: "accepted-checkpoint"}
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	client, err := nativeadi.NewClient(nativeadi.Config{LibraryPath: s.config.Library, ProfileDir: profile, Python: s.config.Python, Identifier: device.Identifier, Serial: device.SerialNumber, PushToken: hardware.Push.Token})
	if err != nil {
		return control.Result{Message: "无法加载账号 Mac 身份", Stage: "accepted-checkpoint"}
	}
	if err := client.Start(ctx); err != nil {
		return control.Result{Message: "无法启动账号 Mac 身份", Stage: "accepted-checkpoint"}
	}
	defer client.Close()
	headers, err := client.Headers()
	if err != nil {
		return control.Result{Message: "无法生成 Apple 验证头", Stage: "accepted-checkpoint"}
	}
	info, err := icloud.FetchStorageInfo(ctx, accepted.ServiceData, account.AppleID, accepted.Session.DSID, accepted.ADSID, accepted.Session.Tokens["mmeAuthToken"], headers)
	if err != nil {
		return control.Result{Message: "iCloud 会话实时验证失败", Stage: "accepted-checkpoint"}
	}
	result := control.Result{OK: true, Storage: info, Message: "已用 Apple 验收会话读取当前配额", Stage: "accepted-checkpoint"}
	if includePlans {
		plans, err := icloud.FetchStorefrontPlanInfoWithStorageToken(ctx, account.AppleID, accepted.Session.DSID, accepted.ADSID, accepted.Session.Tokens["mmeAuthToken"], saved.Tokens["com.apple.gs.icloud.storage.buy"], headers)
		if err != nil {
			return control.Result{Message: "可购方案实时查询失败", Storage: info, Stage: "accepted-checkpoint"}
		}
		result.StoragePlan = plans
		result.Message = "已用 Apple 验收会话读取当前配额与可购方案"
	}
	return result
}
