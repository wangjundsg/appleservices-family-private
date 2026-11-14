package dashboard

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The panel supervises the existing pipeline. Credentials stay in private files;
// neither subprocess output nor upstream responses are sent to the browser.
type registerRequest struct {
	Email           string    `json:"email"`
	Password        string    `json:"password"`
	Phone           string    `json:"phone"`
	PhoneCC         string    `json:"phonecc"`
	Platform        string    `json:"platform"`
	Activation      string    `json:"activation"`
	ExpiresAt       time.Time `json:"expiresAt"`
	AcceptTerms     bool      `json:"acceptTerms"`
	CaptchaProvider string    `json:"captchaProvider"`
	RetryOf         string    `json:"retryOf,omitempty"`
	Egress          struct {
		Mode      string `json:"mode"`
		ProxyPort int    `json:"proxyPort"`
	} `json:"egress"`
}

type registerSettings struct {
	Roxy map[string]any `json:"roxy"`
}

type registerMeta struct {
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	ExitCode   *int       `json:"exitCode"`
	Imported   bool       `json:"imported"`
}

type pipelineReceipt struct {
	Stage                string     `json:"stage"`
	Status               string     `json:"status"`
	Reason               string     `json:"reason"`
	Active               bool       `json:"active"`
	HeartbeatAt          time.Time  `json:"heartbeat_at"`
	AccountCreated       bool       `json:"account_created"`
	RegistrationStarted  bool       `json:"registration_started"`
	CreationRejected     bool       `json:"creation_rejected"`
	RegistrationAccepted bool       `json:"registration_accepted"`
	RefundPending        bool       `json:"refund_pending"`
	RefundDeferred       bool       `json:"refund_deferred"`
	RefundConfirmed      bool       `json:"refund_confirmed"`
	CleanupPending       bool       `json:"cleanup_pending"`
	CooldownUntil        *time.Time `json:"cooldown_until"`
}

var registerIDPattern = regexp.MustCompile(`^reg-[0-9]{8}-[a-f0-9]{16}$`)
var registerEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var registerPhonePattern = regexp.MustCompile(`^[0-9]{5,15}$`)
var activationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
var registerMessages = map[string]string{
	"browser_preflight_failed":               "浏览器出口国家、语言或时区预检未通过，请检查 Forge 与代理配置。",
	"registration_rejected":                  "注册表单被明确拒绝，请查看截图。",
	"code_rate_limited":                      "Apple 限制验证码请求，已停止并进入冷却。",
	"web_login_unconfirmed":                  "网页登录未得到成功确认，请先核实状态。",
	"gsa_login_unconfirmed":                  "账户验证未得到成功确认，请先核实状态。",
	"awaiting_sms":                           "正在等待平台返回新的短信验证码。",
	"awaiting_registration_code":             "正在原页面等待邮箱或短信验证码。",
	"activation_expired":                     "号码已到期，不能继续发送验证码。",
	"account_exists_activation_expired":      "密码认证已确认账号存在，但号码已到期。",
	"expired_activation_account_unconfirmed": "号码已到期，账号是否建成尚未确认。",
	"device_registration_unconfirmed":        "设备登记尚未得到确认。",
	"session_validation_failed":              "网页会话验证失败，已保留本地会话。",
	"code_request_budget_exhausted":          "本号码的登录发码预算已用完。",
	"interrupted_requires_readback":          "任务进程已中断，请核实状态后再处理。",
	"stage_timeout":                          "当前阶段超时，结果需要核实。",
	"browser_create_rejected":                "Forge 拒绝创建浏览器，请检查本地配置。",
	"browser_open_rejected":                  "Forge 拒绝打开浏览器，请检查本地服务。",
	"transport_result_unknown":               "服务请求未取得确认，不能自动重放。",
}
var registrationCountries = map[string][2]string{
	"GB": {"en-GB", "Europe/London"}, "US": {"en-US", "America/New_York"},
	"CA": {"en-CA", "America/Toronto"}, "AU": {"en-AU", "Australia/Sydney"},
	"CN": {"zh-CN", "Asia/Shanghai"}, "DE": {"de-DE", "Europe/Berlin"},
	"FR": {"fr-FR", "Europe/Paris"}, "SG": {"en-SG", "Asia/Singapore"},
	"HK": {"zh-HK", "Asia/Hong_Kong"}, "JP": {"ja-JP", "Asia/Tokyo"},
}

func strongRegistrationPassword(value string) bool {
	if len(value) < 8 || len(value) > 256 {
		return false
	}
	for _, pattern := range []string{`[A-Z]`, `[a-z]`, `[0-9]`, `[^A-Za-z0-9]`} {
		if !regexp.MustCompile(pattern).MatchString(value) {
			return false
		}
	}
	return true
}

func validateRegistration(body registerRequest) error {
	if len(body.Email) > 254 || !registerEmailPattern.MatchString(body.Email) {
		return errors.New("请输入有效邮箱")
	}
	domain := strings.ToLower(strings.SplitN(body.Email, "@", 2)[1])
	if domain == "outlook.com" || domain == "icloud.com" {
		return errors.New("当前建号流程不支持该邮箱域名")
	}
	if !strongRegistrationPassword(body.Password) {
		return errors.New("密码需至少 8 位，包含大小写字母、数字和特殊字符")
	}
	if _, ok := registrationCountries[body.PhoneCC]; !ok {
		return errors.New("不支持的号码国家")
	}
	if !registerPhonePattern.MatchString(body.Phone) || !activationPattern.MatchString(body.Activation) {
		return errors.New("请填写号码和有效的激活 ID")
	}
	if !body.ExpiresAt.After(time.Now()) {
		return errors.New("请填写接码平台显示的号码到期时间")
	}
	if body.Platform != "grizzly" && body.Platform != "hero" {
		return errors.New("不支持的接码平台")
	}
	if body.Egress.Mode != "direct" && body.Egress.Mode != "roxy" {
		return errors.New("请选择直连或 Roxy 代理")
	}
	if body.Egress.Mode == "roxy" && (body.Egress.ProxyPort < 1 || body.Egress.ProxyPort > 65535) {
		return errors.New("代理端口必须在 1–65535 之间")
	}
	if body.CaptchaProvider != "yes" {
		return errors.New("当前面板使用 YesCaptcha")
	}
	if !body.AcceptTerms {
		return errors.New("需要确认同意 Apple 和 iCloud 条款")
	}
	if body.RetryOf != "" && !registerIDPattern.MatchString(body.RetryOf) {
		return errors.New("重试任务无效")
	}
	return nil
}

func readRegisterJSON(file string, value any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, 2<<20)).Decode(value)
}

func writeRegisterJSON(file string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".register-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, file)
}

func (s *Server) registerRoot() string            { return filepath.Join(s.config.RepoRoot, ".cache", "panel") }
func (s *Server) registerFolder(id string) string { return filepath.Join(s.registerRoot(), id) }
func (s *Server) pipelineAccount(account *Account) bool {
	return account != nil && registerIDPattern.MatchString(account.ID) && filepath.Clean(account.Dir) == s.registerFolder(account.ID)
}

func (s *Server) registerSettings() (registerSettings, error) {
	var settings registerSettings
	file := os.Getenv("APPLESERVICES_REGISTER_CONFIG")
	if file == "" {
		file = filepath.Join(s.config.Dir, "registration-config.json")
	}
	if readRegisterJSON(file, &settings) != nil {
		return settings, errors.New("服务端尚未配置 registration-config.json")
	}
	for _, key := range []string{"token", "workspaceId", "projectId"} {
		if value, ok := settings.Roxy[key].(string); !ok || strings.TrimSpace(value) == "" {
			return settings, errors.New("服务端 Forge 配置不完整")
		}
	}
	return settings, nil
}

func (s *Server) registerCommand(args ...string) *exec.Cmd {
	all := append([]string{filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")}, args...)
	command := exec.Command(s.config.Python, all...)
	command.Dir = s.config.RepoRoot
	command.Env = append(os.Environ(), "APPLESERVICES_PIPELINE_STATE="+s.registerRoot())
	configureProcess(command)
	return command
}

func retryableReceipt(receipt pipelineReceipt) bool {
	return !receipt.Active && !receipt.AccountCreated && (receipt.CreationRejected || (!receipt.RegistrationStarted && (receipt.Status == "failed" || receipt.Status == "usage" || receipt.Status == "cancelled")))
}

func (s *Server) startRegistration(body registerRequest) (string, error) {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if err := validateRegistration(body); err != nil {
		return "", err
	}
	settings, err := s.registerSettings()
	if err != nil {
		return "", err
	}
	key := os.Getenv(strings.ToUpper(body.Platform) + "_API_KEY")
	if key == "" {
		return "", errors.New("服务端缺少所选接码平台的 API_KEY 环境变量")
	}
	if os.Getenv("YESCAPTCHA_KEY") == "" {
		return "", errors.New("服务端缺少 YESCAPTCHA_KEY 环境变量")
	}
	if _, err := exec.LookPath(s.config.Python); err != nil {
		return "", errors.New("无法找到 Python")
	}
	if _, err := exec.LookPath("node"); err != nil {
		return "", errors.New("无法找到 Node.js")
	}
	for _, binary := range []string{"login.exe", "appleweb.exe", "trusted-device.exe"} {
		if _, err := os.Stat(filepath.Join(s.config.RepoRoot, "bin", binary)); err != nil {
			return "", errors.New("请先构建产线所需的三个程序")
		}
	}
	s.accountsMu.Lock()
	for _, account := range s.accounts {
		if strings.EqualFold(account.AppleID, body.Email) {
			s.accountsMu.Unlock()
			return "", errors.New("账号已在列表中，无需重新注册")
		}
	}
	s.accountsMu.Unlock()
	// A retry must use a new activation and a proven rejection, never an unknown write.
	if body.RetryOf != "" {
		var receipt pipelineReceipt
		var prior struct {
			Email string `json:"email"`
			SMS   struct {
				Activation string `json:"activation"`
			} `json:"sms"`
		}
		folder := s.registerFolder(body.RetryOf)
		if readRegisterJSON(filepath.Join(folder, "result.json"), &receipt) != nil || !retryableReceipt(receipt) || readRegisterJSON(filepath.Join(folder, "input.json"), &prior) != nil || !strings.EqualFold(prior.Email, body.Email) || prior.SMS.Activation == body.Activation {
			return "", errors.New("仅明确失败的同一账号可用新号码重试；未知结果需先核实")
		}
	}
	// Also reject duplicate clicks before the child has created its reservation.
	files, _ := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "request.json"))
	for _, file := range files {
		var prior struct {
			Email string `json:"email"`
			SMS   struct {
				Activation string `json:"activation"`
				API        string `json:"api"`
			} `json:"sms"`
		}
		if readRegisterJSON(file, &prior) != nil {
			return "", errors.New("历史任务不可读，请先检查本地状态")
		}
		if strings.EqualFold(prior.Email, body.Email) && filepath.Base(filepath.Dir(file)) != body.RetryOf {
			var previous pipelineReceipt
			if body.RetryOf == "" || readRegisterJSON(filepath.Join(filepath.Dir(file), "result.json"), &previous) != nil || !retryableReceipt(previous) {
				return "", errors.New("该邮箱已有任务，请查看原任务")
			}
		}
		if prior.SMS.Activation == body.Activation && strings.Contains(prior.SMS.API, body.Platform) {
			return "", errors.New("该激活 ID 已用于任务，请使用新的号码")
		}
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("无法创建任务标识")
	}
	id := "reg-" + time.Now().Format("20060102") + "-" + hex.EncodeToString(random)
	folder := s.registerFolder(id)
	roxy := settings.Roxy
	locale := registrationCountries[body.PhoneCC]
	roxy["locale"], roxy["timeZone"] = locale[0], locale[1]
	if body.Egress.Mode == "roxy" {
		proxy, ok := roxy["proxyInfo"].(map[string]any)
		if !ok || proxy["host"] == nil || proxy["host"] == "" {
			return "", errors.New("服务端未配置 SOCKS5 代理地址")
		}
		proxy["protocol"], proxy["port"] = "SOCKS5", body.Egress.ProxyPort
		if username, ok := proxy["proxyUserName"].(string); ok {
			proxy["proxyUserName"] = strings.ReplaceAll(username, "{country}", strings.ToLower(body.PhoneCC))
		}
	} else {
		delete(roxy, "proxyInfo")
	}
	api := "https://api.grizzlysms.com/stubs/handler_api.php"
	if body.Platform == "hero" {
		api = "https://hero-sms.com/stubs/handler_api.php"
	}
	manifest := map[string]any{"job_id": id, "email": body.Email, "password": body.Password, "accept_terms": true,
		"email_code_file": filepath.Join(folder, "email-code.txt"), "captcha_solver": "yes", "yescaptcha_key": os.Getenv("YESCAPTCHA_KEY"),
		"egress_mode": body.Egress.Mode, "roxy": roxy,
		"sms": map[string]any{"api": api, "key": key, "activation": body.Activation, "phone": body.Phone, "phonecc": body.PhoneCC, "expires_at": body.ExpiresAt.Format(time.RFC3339)}}
	if body.RetryOf != "" {
		manifest["retry_of"] = body.RetryOf
	}
	if err := os.MkdirAll(folder, 0700); err != nil {
		return "", errors.New("无法创建任务目录")
	}
	if err := writeRegisterJSON(filepath.Join(folder, "request.json"), manifest); err != nil {
		return "", errors.New("无法保存私有任务参数")
	}
	meta := registerMeta{StartedAt: time.Now().UTC()}
	if err := writeRegisterJSON(filepath.Join(folder, "panel.json"), meta); err != nil {
		return "", errors.New("无法保存任务进度")
	}
	command := s.registerCommand("run", "--input", filepath.Join(folder, "request.json"))
	if err := command.Start(); err != nil {
		code := -1
		now := time.Now().UTC()
		meta.ExitCode, meta.FinishedAt = &code, &now
		_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
		return "", errors.New("产线启动失败，请检查 Python 配置")
	}
	go func() {
		_ = command.Wait()
		code := command.ProcessState.ExitCode()
		now := time.Now().UTC()
		s.registerMu.Lock()
		_ = readRegisterJSON(filepath.Join(folder, "panel.json"), &meta)
		meta.ExitCode, meta.FinishedAt = &code, &now
		_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
		s.registerMu.Unlock()
	}()
	return id, nil
}

func (s *Server) registerView(id string) map[string]any {
	var meta registerMeta
	var receipt pipelineReceipt
	folder := s.registerFolder(id)
	_ = readRegisterJSON(filepath.Join(folder, "panel.json"), &meta)
	err := readRegisterJSON(filepath.Join(folder, "result.json"), &receipt)
	if err != nil {
		receipt.Stage, receipt.Status = "create", "queued"
		if meta.ExitCode != nil || time.Since(meta.StartedAt) > 30*time.Second {
			receipt.Status = "needs_review"
		}
	}
	active := receipt.Active && (receipt.HeartbeatAt.IsZero() || time.Since(receipt.HeartbeatAt) < 45*time.Second)
	state, stage := receipt.Status, receipt.Stage
	if stage == "create" {
		stage = "creating"
	}
	if stage == "gsa" {
		stage = "twofa"
	}
	if state == "ready" {
		stage = "done"
	}
	if state == "queued" && err != nil {
		stage = "queued"
	}
	if receipt.Active && !active {
		state = "needs_review"
	}
	end := time.Now()
	if meta.FinishedAt != nil {
		end = *meta.FinishedAt
	}
	return map[string]any{"jobId": id, "state": state, "stage": stage, "startedAt": meta.StartedAt, "elapsedMs": end.Sub(meta.StartedAt).Milliseconds(),
		"message":  registerMessages[receipt.Reason],
		"exitCode": meta.ExitCode, "active": active, "canRetry": retryableReceipt(receipt), "imported": meta.Imported,
		"refundPending": receipt.RefundPending, "refundDeferred": receipt.RefundDeferred, "refundConfirmed": receipt.RefundConfirmed,
		"cleanupPending": receipt.CleanupPending, "cooldownUntil": receipt.CooldownUntil}
}

func (s *Server) serveRegistration(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/register" {
		switch r.Method {
		case "POST":
			var body registerRequest
			if !readJSON(w, r, &body) {
				return
			}
			body.Email = strings.TrimSpace(body.Email)
			id, err := s.startRegistration(body)
			if err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
				return
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]string{"jobId": id})
		case "GET":
			files, _ := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "panel.json"))
			sort.Sort(sort.Reverse(sort.StringSlice(files)))
			jobs := make([]map[string]any, 0)
			for _, file := range files {
				id := filepath.Base(filepath.Dir(file))
				if registerIDPattern.MatchString(id) {
					jobs = append(jobs, s.registerView(id))
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
		default:
			w.WriteHeader(405)
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/register/"), "/")
	if len(parts) < 1 || !registerIDPattern.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	folder := s.registerFolder(id)
	if _, err := os.Stat(filepath.Join(folder, "panel.json")); err != nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == "GET" {
		json.NewEncoder(w).Encode(s.registerView(id))
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch {
	case parts[1] == "cancel" && r.Method == "POST":
		var receipt pipelineReceipt
		_ = readRegisterJSON(filepath.Join(folder, "result.json"), &receipt)
		if receipt.Status == "ready" {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"message": "任务已完成，不退回已使用的号码"})
			return
		}
		if os.WriteFile(filepath.Join(folder, "cancel.request"), []byte("cancel"), 0600) != nil {
			http.Error(w, `{"message":"取消请求未保存"}`, 500)
			return
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"message": "已请求取消；未知结果先保留号码，到期后核实并结算"})
	case parts[1] == "input" && r.Method == "POST":
		var body struct {
			Code string `json:"code"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(body.Code) {
			http.Error(w, `{"message":"请输入 4–8 位邮箱验证码"}`, 400)
			return
		}
		var receipt pipelineReceipt
		_ = readRegisterJSON(filepath.Join(folder, "result.json"), &receipt)
		if !receipt.Active || receipt.Stage != "create" {
			http.Error(w, `{"message":"当前任务没有等待注册验证码"}`, 409)
			return
		}
		if os.WriteFile(filepath.Join(folder, "email-code.txt"), []byte(body.Code), 0600) != nil {
			http.Error(w, `{"message":"验证码保存失败"}`, 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case parts[1] == "log" && r.Method == "GET":
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail < 1 || tail > 200 {
			tail = 200
		}
		rows := make([]map[string]string, 0)
		if f, err := os.Open(filepath.Join(folder, "events.jsonl")); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(io.LimitReader(f, 8<<20))
			for scanner.Scan() {
				var row struct {
					Stage     string    `json:"stage"`
					Status    string    `json:"status"`
					UpdatedAt time.Time `json:"updated_at"`
				}
				if json.Unmarshal(scanner.Bytes(), &row) != nil {
					continue
				}
				// Only known enums cross the UI boundary, never reason/output strings.
				if !strings.Contains("|create|login|terms|gsa|device|keepalive|", "|"+row.Stage+"|") || !strings.Contains("|queued|running|waiting_input|ready|failed|usage|unknown|needs_review|rate_limited|needs_login|cancelled|", "|"+row.Status+"|") {
					continue
				}
				rows = append(rows, map[string]string{"stage": row.Stage, "state": row.Status, "at": row.UpdatedAt.Format(time.RFC3339)})
				if len(rows) > tail {
					rows = rows[1:]
				}
			}
		}
		json.NewEncoder(w).Encode(rows)
	case parts[1] == "evidence" && r.Method == "GET":
		name := r.URL.Query().Get("name")
		if name == "" {
			names := make([]string, 0)
			files, _ := os.ReadDir(folder)
			for _, file := range files {
				if !file.IsDir() && safeEvidenceName(file.Name()) {
					names = append(names, file.Name())
				}
			}
			json.NewEncoder(w).Encode(names)
			return
		}
		if !safeEvidenceName(name) {
			http.NotFound(w, r)
			return
		}
		info, err := os.Lstat(filepath.Join(folder, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 20<<20 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		http.ServeFile(w, r, filepath.Join(folder, name))
	default:
		w.WriteHeader(405)
	}
}

func safeEvidenceName(name string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9_-]+\.png$`).MatchString(name)
}

func (s *Server) importRegistration(id string) {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	folder := s.registerFolder(id)
	var meta registerMeta
	var receipt pipelineReceipt
	if readRegisterJSON(filepath.Join(folder, "panel.json"), &meta) != nil || meta.Imported || readRegisterJSON(filepath.Join(folder, "result.json"), &receipt) != nil || receipt.Status != "ready" || !receipt.RegistrationAccepted {
		return
	}
	var input struct {
		Email string `json:"email"`
	}
	if readRegisterJSON(filepath.Join(folder, "input.json"), &input) != nil || !acceptedCheckpointReady(folder, input.Email) || !verifiedGSACheckpointReady(folder, input.Email) {
		return
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	if s.registryNote != "" {
		return
	}
	for _, account := range s.accounts {
		if strings.EqualFold(account.AppleID, input.Email) {
			if account.ID == id {
				meta.Imported = true
				_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
			}
			return
		}
	}
	entries := make([]*registryEntry, 0, len(s.accounts)+1)
	for _, a := range s.accounts {
		entries = append(entries, &registryEntry{ID: a.ID, Name: a.Name, AppleID: a.AppleID, Dir: a.Dir, CreatedAt: a.CreatedAt})
	}
	name := "Apple " + id[len(id)-6:]
	entries = append(entries, &registryEntry{ID: id, Name: name, AppleID: input.Email, Dir: folder, CreatedAt: meta.StartedAt.Format(time.RFC3339)})
	if saveRegistry(s.config.Dir, entries) != nil {
		return
	}
	s.accounts = append(s.accounts, &Account{ID: id, Name: name, AppleID: input.Email, Dir: folder, CreatedAt: meta.StartedAt.Format(time.RFC3339), message: "产线已完成；网页会话每 10 分钟验证，设备受信任状态尚未证实"})
	meta.Imported = true
	_ = writeRegisterJSON(filepath.Join(folder, "panel.json"), meta)
}

func (s *Server) maintainRegistrations(ctx context.Context) {
	if s.config.RepoRoot == "" {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		files, _ := filepath.Glob(filepath.Join(s.registerRoot(), "reg-*", "panel.json"))
		if len(files) > 0 {
			// Per-job Python locks exclude active children and other panel instances.
			command := s.registerCommand("maintain-once")
			_ = command.Run()
			for _, file := range files {
				id := filepath.Base(filepath.Dir(file))
				if registerIDPattern.MatchString(id) {
					s.importRegistration(id)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
