package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Laky-64/appleservices/nativeadi"
)

// ensureMacPushState establishes the account-private Mac APS certificate,
// key and token before the trusted-device worker starts. It never receives
// Apple account credentials and refuses to accept a partial result.
func (s *Server) ensureMacPushState(ctx context.Context, dir string) error {
	if macPushStateReady(dir) {
		return nil
	}
	if s.provisionMacPush == nil {
		return errors.New("Mac APS provisioner unavailable")
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = s.provisionMacPush(ctx, dir)
		if macPushStateReady(dir) {
			return nil
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("Mac APS provisioner returned incomplete state")
}

func macPushStateReady(dir string) bool {
	for _, path := range []string{
		filepath.Join(dir, "device.json"),
		filepath.Join(dir, "native-adi", "hw_info.plist"),
		filepath.Join(dir, "push.json"),
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func (s *Server) runMacPushProvisioner(ctx context.Context, dir string) error {
	if s.config.Python == "" || s.config.MacPushSetup == "" || s.config.Library == "" {
		return errors.New("Mac APS provisioner is not configured")
	}
	if _, err := os.Stat(s.config.MacPushSetup); err != nil {
		return errors.New("Mac APS provisioner is unavailable")
	}
	if err := nativeadi.ValidateLibrary(s.config.Library); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, s.config.Python, s.config.MacPushSetup,
		"--profile", filepath.Join(dir, "native-adi"),
		"--device", filepath.Join(dir, "device.json"),
		"--library", s.config.Library,
		"--go-state-out", filepath.Join(dir, "push.json"))
	configureProcess(command)
	// The bootstrap has no reason to receive credentials or proxy overrides.
	// This also keeps a direct Apple APS path when the desktop environment has
	// unrelated proxy variables configured.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch {
		case strings.EqualFold(key, "APPLESERVICES_APPLEID"),
			strings.EqualFold(key, "APPLESERVICES_PASSWORD"),
			strings.EqualFold(key, "APPLESERVICES_STORE_CODE"),
			strings.EqualFold(key, "HTTP_PROXY"),
			strings.EqualFold(key, "HTTPS_PROXY"),
			strings.EqualFold(key, "ALL_PROXY"),
			strings.EqualFold(key, "APPLESERVICES_NATIVE_HTTP_PROXY"),
			strings.EqualFold(key, "APPLESERVICES_SOCKS_PROXY"):
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "NO_PROXY=*")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return macPushDiagnostic(output.Bytes())
	}
	return nil
}

type macPushFailure struct{ phase string }

func (e *macPushFailure) Error() string { return "Mac APS bootstrap failed: " + e.phase }

func macPushDiagnostic(output []byte) error {
	for _, line := range bytes.Split(output, []byte("\n")) {
		var record struct{ Stage, Phase string }
		if json.Unmarshal(line, &record) != nil || record.Stage != "Mac APS setup" {
			continue
		}
		switch record.Phase {
		case "initialize", "read-hardware", "read-config-and-identity", "connect-APS", "save-APS":
			return &macPushFailure{phase: record.Phase}
		}
	}
	return errors.New("Mac APS bootstrap failed")
}

func macPushFailureMessage(err error) string {
	var failure *macPushFailure
	if errors.As(err, &failure) {
		phase := map[string]string{"initialize": "初始化本机模块", "read-hardware": "读取设备身份", "read-config-and-identity": "校验设备配置", "connect-APS": "连接 Apple 推送服务", "save-APS": "保存推送证书"}[failure.phase]
		return "Mac 推送初始化失败，阶段：" + phase + "。账号目录已保留，尚未启动账号登录，请修复后选择该账号连接。"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Mac 推送初始化已取消或超时。账号目录已保留，尚未启动账号登录。"
	}
	return "Mac 推送初始化失败，请检查本机模块和网络。账号目录已保留，尚未启动账号登录；未获取具体失败阶段。"
}
