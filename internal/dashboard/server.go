package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/control"
)

//go:embed web/*
var assets embed.FS

type Config struct {
	Dir, Worker, Library, Python, MacPushSetup, RepoRoot string
	Port                                                 int
}
type Server struct {
	config     Config
	listener   net.Listener
	http       *http.Server
	token      string
	launchMu   sync.Mutex
	registerMu sync.Mutex
	mu         sync.Mutex
	launching  bool
	message    string
	// accounts holds every registered account. The legacy single-account
	// fields below address the account whose directory is Config.Dir, so the
	// original one-account behaviour and its tests keep working unchanged.
	accounts   []*Account
	accountsMu sync.Mutex
	// registryNote reports why the account list could not be loaded, so a
	// failure is surfaced instead of silently presenting an empty list.
	registryNote string
	// provisionMacPush is replaceable only by package tests. Production uses
	// the pinned native Mac APS bootstrap configured by cmd/dashboard.
	provisionMacPush func(context.Context, string) error
}
type Status struct {
	PID                  int       `json:"pid"`
	Running              bool      `json:"running"`
	PushConnected        bool      `json:"push_connected"`
	Authenticated        bool      `json:"authenticated"`
	VerificationRequired bool      `json:"verification_required"`
	State                string    `json:"state"`
	UpdatedAt            time.Time `json:"updated_at"`
	LastRefreshAt        time.Time `json:"last_refresh_at"`
	LastDeviceQueryAt    time.Time `json:"last_device_query_at"`
	DeviceCount          int       `json:"device_count"`
	InstallationVisible  bool      `json:"installation_visible"`
	RefreshCount         int       `json:"refresh_count"`
	PairingUnsupported   bool      `json:"pairing_unsupported"`
	PairingState         string    `json:"pairing_state,omitempty"`
	PushReconnectCount   int       `json:"push_reconnect_count"`
	PushIssue            string    `json:"push_issue,omitempty"`
	FailureStage         string    `json:"failure_stage,omitempty"`
}
type view struct {
	PipelineManaged      bool                    `json:"pipeline_managed,omitempty"`
	PipelineState        string                  `json:"pipeline_state,omitempty"`
	Status               Status                  `json:"status"`
	Devices              []control.Device        `json:"devices"`
	Pending              *control.Pending        `json:"pending"`
	CodeReady            bool                    `json:"code_ready"`
	ControlReady         bool                    `json:"control_ready"`
	Launching            bool                    `json:"launching"`
	Message              string                  `json:"message"`
	Account              string                  `json:"account"`
	LoginAccount         string                  `json:"login_account,omitempty"`
	Accounts             []AccountView           `json:"accounts,omitempty"`
	Selected             string                  `json:"selected,omitempty"`
	ICloudActivated      bool                    `json:"icloud_activated"`
	Family               *control.FamilyInfo     `json:"family,omitempty"`
	FamilyCheckedAt      string                  `json:"family_checked_at,omitempty"`
	FamilyStale          bool                    `json:"family_stale"`
	Storage              *icloud.StorageInfo     `json:"storage,omitempty"`
	StorageCheckedAt     string                  `json:"storage_checked_at,omitempty"`
	StorageStale         bool                    `json:"storage_stale"`
	StoragePlan          *icloud.StoragePlanInfo `json:"storage_plan,omitempty"`
	StoragePlanCheckedAt string                  `json:"storage_plan_checked_at,omitempty"`
	StoragePlanStale     bool                    `json:"storage_plan_stale"`
	HideMyEmailAvailable bool                    `json:"hide_my_email_available"`
	ActivationReady      bool                    `json:"activation_ready"`
}
type credentials struct {
	AppleID   string `json:"apple_id"`
	Password  string `json:"password"`
	TwoFactor string `json:"twofactor,omitempty"`
}

func New(config Config) (*Server, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", intString(config.Port)))
	if err != nil {
		return nil, errors.New("面板端口已被使用，请打开已运行的面板")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		listener.Close()
		return nil, err
	}
	s := &Server{config: config, listener: listener, token: hex.EncodeToString(key)}
	s.provisionMacPush = s.runMacPushProvisioner
	if entries, err := loadRegistry(config.Dir, config.Dir); err != nil {
		// Surfaced rather than swallowed: an unreadable registry previously
		// made the panel look like it had no accounts at all.
		s.registryNote = err.Error()
	} else {
		for _, entry := range entries {
			s.accounts = append(s.accounts, &Account{ID: entry.ID, Name: entry.Name, AppleID: entry.AppleID, Dir: entry.Dir, CreatedAt: entry.CreatedAt})
		}
	}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 4 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	return s, nil
}

// lookup resolves one account by id. An empty id selects the account whose
// directory is the server's own, falling back to the first registered account
// (a migrated install no longer has any account living in the root directory).
func (s *Server) lookup(id string) *Account {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	if id == "" {
		for _, account := range s.accounts {
			if filepath.Clean(account.Dir) == filepath.Clean(s.config.Dir) {
				return account
			}
		}
		if len(s.accounts) > 0 {
			return s.accounts[0]
		}
		return nil
	}
	for _, account := range s.accounts {
		if account.ID == id {
			return account
		}
	}
	return nil
}

// listAccounts returns every registered account with its live status.
func (s *Server) listAccounts(ctx context.Context) []AccountView {
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	out := make([]AccountView, 0, len(accounts))
	for _, account := range accounts {
		status := s.accountStatus(account)
		item := AccountView{ID: account.ID, Name: account.Name, Account: maskAccount(account.AppleID),
			State: status.State, Running: status.Running, Authenticated: status.Authenticated,
			ICloudActivated: acceptedCheckpointReady(account.Dir, account.AppleID)}
		if family, checkedAt, stale := readFamilyCache(account); family != nil {
			s.resolveCachedFamilyOrganizer(account, family)
			item.FamilyMember = family.Member
			item.FamilyOrganizer = family.Organizer
			item.FamilyOrganizerAccount = family.OrganizerAccount
			item.FamilyStale = stale
			item.FamilyCheckedAt = checkedAt.Format(time.RFC3339)
		}
		account.mu.Lock()
		if account.message != "" {
			item.Message = account.message
		}
		account.mu.Unlock()
		out = append(out, item)
	}
	return out
}

// addAccount registers a new account in its own directory. It never reuses or
// overwrites an existing account's directory or credentials.
func (s *Server) addAccount(ctx context.Context, creds credentials) (control.Result, string) {
	if strings.TrimSpace(creds.AppleID) == "" || creds.Password == "" {
		return control.Result{Message: "请输入 Apple 账户和密码"}, ""
	}
	s.accountsMu.Lock()
	requested := strings.TrimSpace(creds.AppleID)
	requestedName := strings.SplitN(requested, "@", 2)[0]
	for _, account := range s.accounts {
		sameID := account.AppleID != "" && strings.EqualFold(account.AppleID, requested)
		// A migrated account may carry only a display name, so match on that
		// too rather than registering the same login twice under a new slug.
		sameName := account.AppleID == "" && account.Name != "" && strings.EqualFold(account.Name, requestedName)
		if sameID || sameName {
			id := account.ID
			s.accountsMu.Unlock()
			return control.Result{Message: "该账号已在列表中，请直接选择它"}, id
		}
	}
	entries := make([]*registryEntry, 0, len(s.accounts)+1)
	for _, account := range s.accounts {
		entries = append(entries, &registryEntry{ID: account.ID, Name: account.Name, AppleID: account.AppleID, Dir: account.Dir, CreatedAt: account.CreatedAt})
	}
	id, dir, err := newAccountDir(s.config.Dir, entries)
	if err != nil {
		s.accountsMu.Unlock()
		return control.Result{Message: err.Error()}, ""
	}
	// Each account gets isolated identity and provisioning state. An
	// environment-mismatch response alone does not prove permanent damage.
	if err := provisionIdentity(dir); err != nil {
		os.RemoveAll(dir)
		s.accountsMu.Unlock()
		return control.Result{Message: err.Error()}, ""
	}
	entry := &registryEntry{ID: id, Name: strings.SplitN(creds.AppleID, "@", 2)[0], AppleID: strings.TrimSpace(creds.AppleID), Dir: dir}
	entries = append(entries, entry)
	if err := saveRegistry(s.config.Dir, entries); err != nil {
		s.accountsMu.Unlock()
		return control.Result{Message: "无法保存账号列表"}, ""
	}
	account := &Account{ID: entry.ID, Name: entry.Name, AppleID: entry.AppleID, Dir: entry.Dir}
	s.accounts = append(s.accounts, account)
	s.accountsMu.Unlock()
	result := s.startAccount(ctx, account, creds)
	return result, id
}

// removeAccount stops one account and unregisters it. Its directory is kept on
// disk so its device identity is not destroyed by an accidental click.
func (s *Server) removeAccount(ctx context.Context, id string) control.Result {
	s.accountsMu.Lock()
	var target *Account
	remaining := make([]*registryEntry, 0, len(s.accounts))
	for _, account := range s.accounts {
		if account.ID == id {
			target = account
			continue
		}
		remaining = append(remaining, &registryEntry{ID: account.ID, Name: account.Name, AppleID: account.AppleID, Dir: account.Dir, CreatedAt: account.CreatedAt})
	}
	s.accountsMu.Unlock()
	if target == nil {
		return control.Result{Message: "账号不存在"}
	}
	if err := s.stopIn(ctx, target.Dir); err != nil {
		return control.Result{Message: "请先停止该账号的后台"}
	}
	if err := saveRegistry(s.config.Dir, remaining); err != nil {
		return control.Result{Message: "无法保存账号列表"}
	}
	s.accountsMu.Lock()
	s.accounts = nil
	for _, entry := range remaining {
		s.accounts = append(s.accounts, &Account{ID: entry.ID, Name: entry.Name, AppleID: entry.AppleID, Dir: entry.Dir, CreatedAt: entry.CreatedAt})
	}
	s.accountsMu.Unlock()
	return control.Result{OK: true, Message: "已从列表移除，设备身份仍保留在 accounts/" + target.ID}
}

func maskAccount(appleID string) string {
	pieces := strings.SplitN(appleID, "@", 2)
	if len(pieces) != 2 || pieces[0] == "" {
		return appleID
	}
	return pieces[0][:1] + "***@" + pieces[1]
}

func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }
func (s *Server) Serve(ctx context.Context) error {
	go s.maintainRegistrations(ctx)
	go func() { <-ctx.Done(); s.http.Close() }()
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// accountStatus reads one account's worker state. Each account has its own
// runtime.json in its own directory, so one account's worker can never be
// mistaken for another's.
func (s *Server) accountStatus(account *Account) Status {
	var status Status
	if account == nil {
		return status
	}
	return s.readStatusFrom(account.Dir)
}

// readStatus reads the server's own state directory.
func (s *Server) readStatus() Status { return s.readStatusFrom(s.config.Dir) }

func (s *Server) readStatusFrom(dir string) Status {
	var status Status
	data, _ := os.ReadFile(filepath.Join(dir, "runtime.json"))
	json.Unmarshal(data, &status)
	if !status.Running || !s.ownsIn(status.PID, dir) {
		status.Running = false
		status.Authenticated = false
		status.PushConnected = false
		if status.State == "online" {
			status.State = "stopped"
		}
	}
	return status
}
func (s *Server) owns(pid int) bool { return s.ownsIn(pid, s.config.Dir) }

func (s *Server) ownsIn(pid int, dir string) bool {
	path := processPath(pid)
	// A live account-private control reply is required when Windows reports an
	// inaccessible or renamed image during worker binary replacement.
	if path == "" {
		return s.controlAliveIn(pid, dir)
	}
	cleanPath := filepath.Clean(path)
	// Windows may retain the previous image under a temporary '~' suffix when
	// the worker binary is rebuilt while that worker is still running.
	cleanPath = strings.TrimRight(cleanPath, "~")
	if strings.EqualFold(cleanPath, filepath.Clean(s.config.Worker)) || strings.EqualFold(cleanPath, filepath.Join(filepath.Dir(s.config.Worker), "trusted-device.exe")) {
		return true
	}
	return s.controlAliveIn(pid, dir)
}
func (s *Server) controlAlive(pid int) bool { return s.controlAliveIn(pid, s.config.Dir) }

func (s *Server) controlAliveIn(pid int, dir string) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil {
		return false
	}
	var record control.Record
	if json.Unmarshal(data, &record) != nil || record.PID != pid {
		return false
	}
	parsed, err := url.Parse(record.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	key, err := hex.DecodeString(record.Token)
	if err != nil || len(key) != 32 {
		return false
	}
	request, err := http.NewRequest("GET", record.URL+"/view", nil)
	if err != nil {
		return false
	}
	request.Header.Set("X-Control-Token", record.Token)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	var view control.View
	if json.NewDecoder(io.LimitReader(response.Body, 128*1024)).Decode(&view) != nil {
		return false
	}
	var status struct {
		PID     int  `json:"pid"`
		Running bool `json:"running"`
	}
	return json.Unmarshal(view.Status, &status) == nil && status.PID == pid && status.Running
}
func (s *Server) record() (control.Record, error) { return s.recordFor(s.config.Dir) }

func (s *Server) recordFor(dir string) (control.Record, error) {
	var record control.Record
	data, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil {
		return record, errors.New("后台尚未就绪")
	}
	if json.Unmarshal(data, &record) != nil {
		return record, errors.New("后台尚未就绪")
	}
	parsed, err := url.Parse(record.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return control.Record{}, errors.New("无效的本机控制地址")
	}
	if key, err := hex.DecodeString(record.Token); err != nil || len(key) != 32 || record.PID != s.readStatusFrom(dir).PID || !s.ownsIn(record.PID, dir) {
		return control.Record{}, errors.New("后台尚未就绪")
	}
	return record, nil
}
func (s *Server) call(ctx context.Context, path string, body any, result any) error {
	return s.callFor(ctx, s.config.Dir, path, body, result)
}

func workerHTTPError(status int, data []byte, body any) error {
	if status == http.StatusBadRequest {
		if action, ok := body.(control.Action); ok && strings.HasPrefix(action.Action, "hme-") {
			return errors.New("当前账号后台版本不支持隐私邮箱，请重新连接此账号完成升级")
		}
		if bytes.Contains(data, []byte("unknown action")) {
			return errors.New("当前账号后台版本不支持此操作，请重新连接后重试")
		}
		return errors.New("后台拒绝了无效操作")
	}
	if status == http.StatusGatewayTimeout || status == http.StatusServiceUnavailable {
		return errors.New("后台正在处理其他操作，请稍后重试")
	}
	return errors.New("后台操作未完成，请检查运行状态")
}

func (s *Server) callFor(ctx context.Context, dir, path string, body any, result any) error {
	record, err := s.recordFor(dir)
	if err != nil {
		return err
	}
	method := "GET"
	var reader io.Reader
	if body != nil {
		method = "POST"
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, record.URL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("X-Control-Token", record.Token)
	request.Header.Set("Content-Type", "application/json")
	timeout := 95 * time.Second
	if action, ok := body.(control.Action); ok {
		if action.Action == "storage-skill-auth-file" {
			timeout = 3*time.Minute + 5*time.Second
		} else if action.Action == "family-add-self" || strings.HasPrefix(action.Action, "hme-") {
			timeout = 2*time.Minute + 5*time.Second
		}
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("无法连接后台，请检查运行状态")
	}
	defer response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return workerHTTPError(response.StatusCode, data, body)
	}
	if json.NewDecoder(io.LimitReader(response.Body, 128*1024)).Decode(result) != nil {
		return errors.New("后台返回了无效数据")
	}
	return nil
}

const familyStatusPrefix = "FAMILY_STATUS_JSON="

func parseFamilyStatus(output string) (*control.FamilyInfo, error) {
	lines := strings.Split(output, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, familyStatusPrefix) {
			continue
		}
		var status control.FamilyInfo
		if json.Unmarshal([]byte(strings.TrimPrefix(line, familyStatusPrefix)), &status) != nil {
			return nil, errors.New("家庭工具返回了无效状态")
		}
		return &status, nil
	}
	return nil, errors.New("家庭工具没有返回状态")
}

func runFamilyStatus(ctx context.Context, helper string, account *Account, joinStorage bool) (*control.FamilyInfo, error) {
	args := []string{"-dir", account.Dir, "-safe-json"}
	if joinStorage {
		args = append(args, "-join-shared-storage")
	}
	command := exec.CommandContext(ctx, helper, args...)
	command.Env = os.Environ()
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return nil, errors.New("家庭操作未完成，请刷新状态后核对")
	}
	return parseFamilyStatus(output.String())
}

type familyCandidate struct {
	account *Account
	family  *control.FamilyInfo
}

func uniquePoolOrganizer(current *control.FamilyInfo, candidates []familyCandidate) *Account {
	if current == nil || !current.Member {
		return nil
	}
	var match *Account
	for _, candidate := range candidates {
		family := candidate.family
		if candidate.account == nil || family == nil || !family.Member || !family.Organizer || family.Organizers != 1 || family.Members != current.Members {
			continue
		}
		if current.StorageKnown && family.StorageKnown && current.StorageBytes > 0 && family.StorageBytes > 0 && current.StorageBytes != family.StorageBytes {
			continue
		}
		if match != nil && match.ID != candidate.account.ID {
			return nil
		}
		match = candidate.account
	}
	return match
}

func sameFamilySummary(organizer, member *control.FamilyInfo) bool {
	if organizer == nil || member == nil || !organizer.Member || !organizer.Organizer || !member.Member || member.Organizer {
		return false
	}
	if organizer.Organizers != 1 || member.Organizers != 1 || organizer.Members != member.Members {
		return false
	}
	return !organizer.StorageKnown || !member.StorageKnown || organizer.StorageBytes <= 0 || member.StorageBytes <= 0 || organizer.StorageBytes == member.StorageBytes
}

func (s *Server) refreshPoolFamilyMembers(ctx context.Context, helper string, organizer *Account, current *control.FamilyInfo) {
	if organizer == nil || current == nil || !current.Organizer {
		return
	}
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	var wait sync.WaitGroup
	for _, candidate := range accounts {
		if candidate == organizer || !acceptedCheckpointReady(candidate.Dir, candidate.AppleID) {
			continue
		}
		wait.Add(1)
		go func(account *Account) {
			defer wait.Done()
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			family, err := runFamilyStatus(readCtx, helper, account, false)
			if err != nil {
				return
			}
			if sameFamilySummary(current, family) {
				family.OrganizerAccount, family.OrganizerName = organizer.ID, organizer.Name
			}
			_, _ = writeFamilyCache(account, family)
		}(candidate)
	}
	wait.Wait()
}

func (s *Server) resolveFamilyOrganizer(ctx context.Context, helper string, currentAccount *Account, current *control.FamilyInfo) {
	if current == nil || !current.Member {
		return
	}
	if current.Organizer {
		current.OrganizerAccount, current.OrganizerName = currentAccount.ID, currentAccount.Name
		return
	}
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	results := make(chan familyCandidate, len(accounts))
	var wait sync.WaitGroup
	for _, candidate := range accounts {
		if candidate == currentAccount || !acceptedCheckpointReady(candidate.Dir, candidate.AppleID) {
			continue
		}
		wait.Add(1)
		go func(account *Account) {
			defer wait.Done()
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			family, err := runFamilyStatus(readCtx, helper, account, false)
			if err == nil {
				if family.Organizer {
					family.OrganizerAccount, family.OrganizerName = account.ID, account.Name
				}
				_, _ = writeFamilyCache(account, family)
				results <- familyCandidate{account: account, family: family}
			}
		}(candidate)
	}
	wait.Wait()
	close(results)
	candidates := make([]familyCandidate, 0, len(accounts))
	for candidate := range results {
		candidates = append(candidates, candidate)
	}
	if organizer := uniquePoolOrganizer(current, candidates); organizer != nil {
		current.OrganizerAccount, current.OrganizerName = organizer.ID, organizer.Name
	}
}

func (s *Server) resolveCachedFamilyOrganizer(currentAccount *Account, current *control.FamilyInfo) {
	if current == nil || !current.Member || current.Organizer || current.OrganizerAccount != "" {
		return
	}
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	candidates := make([]familyCandidate, 0, len(accounts))
	for _, account := range accounts {
		if account == currentAccount {
			continue
		}
		if family, _, _ := readFamilyCache(account); family != nil {
			candidates = append(candidates, familyCandidate{account: account, family: family})
		}
	}
	if organizer := uniquePoolOrganizer(current, candidates); organizer != nil {
		current.OrganizerAccount, current.OrganizerName = organizer.ID, organizer.Name
	}
}

func (s *Server) familyAction(ctx context.Context, account *Account, joinStorage bool) control.Result {
	if account == nil || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return control.Result{Message: "当前账号尚未完成 iCloud 激活", Stage: "family-preflight"}
	}
	helper := filepath.Join(filepath.Dir(s.config.Worker), "familyinspect.exe")
	if info, err := os.Stat(helper); err != nil || !info.Mode().IsRegular() {
		return control.Result{Message: "家庭工具不可用，请重新构建本地后台", Stage: "family-worker"}
	}
	actionCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	family, err := runFamilyStatus(actionCtx, helper, account, joinStorage)
	if err != nil {
		return control.Result{Message: err.Error(), Stage: "family-action"}
	}
	if family.Organizer {
		s.refreshPoolFamilyMembers(actionCtx, helper, account, family)
	} else {
		s.resolveFamilyOrganizer(actionCtx, helper, account, family)
	}
	checkedAt, cacheErr := writeFamilyCache(account, family)
	if joinStorage && (!family.StorageKnown || !family.StorageParticipating || !family.StorageAvailable || !family.StorageEnabled) {
		return control.Result{Message: "家庭共享储存尚未完整生效", Stage: "family-storage-readback", Family: family}
	}
	message, stage := "Apple 已返回当前家庭状态", "family-info"
	if joinStorage {
		message, stage = "已加入家庭共享的 iCloud+ 储存空间", "family-storage-joined"
	}
	if cacheErr != nil {
		message += "；本地缓存未保存，请稍后重新读取"
		stage = "family-cache-warning"
	} else {
		message += " · 已保存 " + checkedAt.Local().Format("15:04")
	}
	return control.Result{OK: true, Message: message, Stage: stage, Family: family}
}

func familyRemovalTarget(organizer *Account, targetID string, accounts []*Account) (*Account, error) {
	targetID = strings.TrimSpace(targetID)
	if organizer == nil || targetID == "" || !idPattern.MatchString(targetID) || targetID == organizer.ID {
		return nil, errors.New("家庭成员绑定无效")
	}
	var target *Account
	for _, candidate := range accounts {
		if candidate == nil || candidate.ID != targetID {
			continue
		}
		if target != nil {
			return nil, errors.New("家庭成员绑定不唯一")
		}
		target = candidate
	}
	if target == nil {
		return nil, errors.New("家庭成员不存在")
	}
	return target, nil
}

func (s *Server) removeFamilyMember(ctx context.Context, organizer *Account, targetID string) control.Result {
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	target, err := familyRemovalTarget(organizer, targetID, accounts)
	if err != nil {
		return control.Result{Message: err.Error(), Stage: "family-binding"}
	}
	if !acceptedCheckpointReady(organizer.Dir, organizer.AppleID) || !acceptedCheckpointReady(target.Dir, target.AppleID) {
		return control.Result{Message: "母号或目标成员尚未完成 iCloud 激活", Stage: "family-preflight"}
	}
	helper := filepath.Join(filepath.Dir(s.config.Worker), "familyinspect.exe")
	if info, statErr := os.Stat(helper); statErr != nil || !info.Mode().IsRegular() {
		return control.Result{Message: "家庭工具不可用，请重新构建本地后台", Stage: "family-worker"}
	}
	actionCtx, cancel := context.WithTimeout(ctx, 105*time.Second)
	defer cancel()
	command := exec.CommandContext(actionCtx, helper, "-dir", organizer.Dir, "-remove-pool-member-dir", target.Dir, "-safe-json")
	command.Env = os.Environ()
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if runErr := command.Run(); runErr != nil {
		return control.Result{Message: "成员移除未完成；请先重新读取双方状态，不要重复提交", Stage: "family-remove"}
	}
	organizerFamily, parseErr := parseFamilyStatus(output.String())
	if parseErr != nil || !organizerFamily.Member || !organizerFamily.Organizer || organizerFamily.Organizers != 1 || organizerFamily.Self != 1 {
		return control.Result{Message: "Apple 已响应，但母号状态回读不完整；不要重复提交", Stage: "family-remove-readback"}
	}
	organizerFamily.OrganizerAccount, organizerFamily.OrganizerName = organizer.ID, organizer.Name
	_, organizerCacheErr := writeFamilyCache(organizer, organizerFamily)

	targetCtx, targetCancel := context.WithTimeout(actionCtx, 35*time.Second)
	targetFamily, targetErr := runFamilyStatus(targetCtx, helper, target, false)
	targetCancel()
	if targetErr != nil || targetFamily.Member {
		return control.Result{Message: "母号已确认成员移除，但目标账号回读未完成；请刷新目标账号，不要重复移除", Stage: "family-remove-target-readback", Family: organizerFamily}
	}
	_, targetCacheErr := writeFamilyCache(target, targetFamily)
	message := "成员已移出家庭，并通过母号与目标账号双边回读"
	stage := "family-member-removed"
	if organizerCacheErr != nil || targetCacheErr != nil {
		message += "；本地缓存未完整保存，请重新读取状态"
		stage = "family-remove-cache-warning"
	}
	return control.Result{OK: true, Message: message, Stage: stage, Family: organizerFamily}
}

func cacheSafeActionResult(account *Account, result *control.Result) {
	if account == nil || result == nil {
		return
	}
	if result.Storage != nil {
		_, _ = writeStorageCache(account, result.Storage)
	}
	if result.StoragePlan != nil {
		_, _ = writePlanCache(account, result.StoragePlan)
	}
}

func (s *Server) snapshot(ctx context.Context) view { return s.snapshotFor(ctx, "") }

// snapshotFor renders one account. An empty id selects the server's own
// directory, preserving the single-account behaviour.
func (s *Server) snapshotFor(ctx context.Context, id string) view {
	s.accountsMu.Lock()
	accounts := append([]*Account(nil), s.accounts...)
	s.accountsMu.Unlock()
	dir := s.config.Dir
	var account *Account
	for _, candidate := range accounts {
		if (id == "" && filepath.Clean(candidate.Dir) == filepath.Clean(s.config.Dir)) || candidate.ID == id {
			account, dir = candidate, candidate.Dir
			break
		}
	}
	// A migrated install keeps no account in the root directory, so fall back
	// to the first registered account instead of rendering an empty panel.
	if account == nil && id == "" && len(accounts) > 0 && filepath.Clean(accounts[0].Dir) != filepath.Clean(s.config.Dir) {
		account, dir = accounts[0], accounts[0].Dir
	}
	result := view{Status: s.readStatusFrom(dir), Devices: []control.Device{}}
	if s.pipelineAccount(account) {
		result.PipelineManaged = true
		var receipt pipelineReceipt
		if readRegisterJSON(filepath.Join(dir, "result.json"), &receipt) == nil {
			result.PipelineState = receipt.Status
		}
	}
	if account == nil {
		s.mu.Lock()
		result.Launching, result.Message = s.launching, s.message
		s.mu.Unlock()
	} else {
		account.mu.Lock()
		result.Launching, result.Message = account.launching, account.message
		account.mu.Unlock()
	}
	result.Accounts = s.listAccounts(ctx)
	result.Selected = id
	if account != nil {
		result.Selected = account.ID
		result.LoginAccount = account.AppleID
		result.ICloudActivated = acceptedCheckpointReady(account.Dir, account.AppleID)
		result.ActivationReady = !result.ICloudActivated && verifiedGSACheckpointReady(account.Dir, account.AppleID)
		if family, checkedAt, stale := readFamilyCache(account); family != nil {
			s.resolveCachedFamilyOrganizer(account, family)
			result.Family = family
			result.FamilyCheckedAt = checkedAt.Format(time.RFC3339)
			result.FamilyStale = stale
		}
		if cached, stale := readStorageCache(account, storageCacheFile); cached != nil {
			result.Storage = cached.Storage
			result.StorageCheckedAt = cached.CheckedAt.Format(time.RFC3339)
			result.StorageStale = stale
		}
		if cached, stale := readStorageCache(account, planCacheFile); cached != nil {
			result.StoragePlan = cached.Plans
			result.StoragePlanCheckedAt = cached.CheckedAt.Format(time.RFC3339)
			result.StoragePlanStale = stale
		}
	}
	var session struct {
		AppleID string `json:"appleId"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	json.Unmarshal(data, &session)
	if session.AppleID != "" {
		result.Account = maskAccount(session.AppleID)
	}
	if result.Status.Running {
		var live control.View
		if s.callFor(ctx, dir, "/view", nil, &live) == nil && json.Unmarshal(live.Status, &result.Status) == nil {
			result.Devices = live.Devices
			result.Pending = live.Pending
			result.CodeReady = live.CodeReady
			result.HideMyEmailAvailable = slices.Contains(live.Capabilities, "hide-my-email")
			result.ControlReady = true
		}
	}
	return result
}
func (s *Server) Start(ctx context.Context, creds credentials) control.Result {
	// Start acts on the account that owns the server's own directory. A
	// registered account is started through startAccount so one account's
	// credentials can never land in another account's directory.
	if account := s.lookup(""); account != nil {
		return s.startAccount(ctx, account, creds)
	}
	return s.startIn(ctx, s.config.Dir, creds, nil)
}

// startAccount starts one registered account inside its own directory.
func (s *Server) startAccount(ctx context.Context, account *Account, creds credentials) control.Result {
	if account == nil {
		return control.Result{Message: "账号不存在"}
	}
	if creds.AppleID == "" {
		creds.AppleID = account.AppleID
	}
	if !strings.EqualFold(strings.TrimSpace(creds.AppleID), account.AppleID) {
		return control.Result{Message: "登录账号与所选工作区不一致，请使用添加账号"}
	}
	return s.startIn(ctx, account.Dir, creds, account)
}

func (s *Server) startIn(ctx context.Context, dir string, creds credentials, account *Account) control.Result {
	return s.startInMode(ctx, dir, creds, account, false)
}

func (s *Server) startSavedAccount(ctx context.Context, account *Account) control.Result {
	if account == nil || !verifiedGSACheckpointReady(account.Dir, account.AppleID) || !acceptedCheckpointReady(account.Dir, account.AppleID) {
		return control.Result{Message: "已保存登录会话不可用，请输入当前密码重新连接"}
	}
	return s.startInMode(ctx, account.Dir, credentials{AppleID: account.AppleID}, account, true)
}

func (s *Server) startInMode(ctx context.Context, dir string, creds credentials, account *Account, resume bool) control.Result {
	if s.pipelineAccount(account) {
		return control.Result{Message: "此账号由建号产线维护网页会话；请在新建 Apple ID 卡片查看进度。设备受信任状态尚未证实"}
	}
	if !s.launchMu.TryLock() {
		return control.Result{Message: "正在启动，请稍等"}
	}
	defer s.launchMu.Unlock()
	s.setMessage(account, "")
	current := s.readStatusFrom(dir)
	if current.Running {
		// A password supplied for an already-running account is an explicit
		// reconnect request. Do not silently report the old passwordless worker
		// as connected; stop it and start a fresh authenticated session below.
		if creds.Password == "" {
			if _, err := s.recordFor(dir); err == nil {
				return control.Result{OK: true, Message: "已连接到现有后台"}
			}
		}
		if creds.Password == "" {
			return control.Result{OK: true, Message: "已连接到现有后台"}
		}
	}
	if strings.TrimSpace(creds.AppleID) == "" || (!resume && creds.Password == "") {
		return control.Result{Message: "请输入 Apple 账户和密码"}
	}
	if creds.TwoFactor == "" {
		creds.TwoFactor = "sms"
	}
	if creds.TwoFactor != "sms" && creds.TwoFactor != "sms-existing" && creds.TwoFactor != "circle" && creds.TwoFactor != "trusted-device" {
		return control.Result{Message: "不支持的验证方式"}
	}
	s.setLaunching(account, true)
	defer s.setLaunching(account, false)
	if current.Running {
		if err := s.stopIn(ctx, dir); err != nil {
			return control.Result{Message: err.Error()}
		}
	}
	if _, err := os.Stat(s.config.Worker); err != nil {
		return control.Result{Message: "缺少后台程序，请重新运行启动脚本"}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return control.Result{Message: "无法写入本机状态目录"}
	}
	// A new account has its own device.json but no Mac APS certificate yet.
	// The worker must not start until that account-private identity exists;
	// otherwise it exits before Apple credential authentication begins.
	if err := s.ensureMacPushState(ctx, dir); err != nil {
		return control.Result{Message: "无法初始化该账号的独立 Mac 推送身份，请检查本机依赖后重试"}
	}
	output, err := os.OpenFile(filepath.Join(dir, "daemon.out.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return control.Result{Message: "无法创建运行日志"}
	}
	defer output.Close()
	diagnostic, err := os.OpenFile(filepath.Join(dir, "daemon.err.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return control.Result{Message: "无法创建运行日志"}
	}
	defer diagnostic.Close()
	command := exec.Command(s.config.Worker, "-native-adi", "-background", "-control-api", "-gui-auth", "-dir", dir, "-adi-dll", s.config.Library, "-python", s.config.Python)
	command.Args = append(command.Args, "-twofactor", creds.TwoFactor)
	if resume {
		command.Args = append(command.Args, "-resume-session")
	}
	configureProcess(command)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, "APPLESERVICES_APPLEID") && !strings.EqualFold(key, "APPLESERVICES_PASSWORD") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "APPLESERVICES_APPLEID="+strings.TrimSpace(creds.AppleID))
	if !resume {
		command.Env = append(command.Env, "APPLESERVICES_PASSWORD="+creds.Password)
	}
	command.Stdout, command.Stderr = output, diagnostic
	if command.Start() != nil {
		return control.Result{Message: "后台启动失败，请检查本机依赖"}
	}
	go func() {
		if err := command.Wait(); err != nil {
			message := "后台异常停止，请重新连接"
			if detail := lastProcessError(diagnostic.Name()); detail != "" {
				message += "：" + detail
			}
			s.setMessage(account, message)
		}
	}()
	if resume {
		return control.Result{OK: true, Message: "正在恢复已保存的 Apple 登录会话"}
	}
	return control.Result{OK: true, Message: "正在连接 Apple，请稍等"}
}

func lastProcessError(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// Never forward arbitrary stderr (which can contain credentials).
		switch {
		case strings.Contains(line, "a provisioned device state is required"):
			return "所选目录缺少有效设备身份，请选择已有账号，或通过添加账号建立身份"
		case strings.Contains(line, "APPLESERVICES_APPLEID and APPLESERVICES_PASSWORD are required"):
			return "请重新输入当前账号密码"
		case strings.Contains(line, "saved login session cannot be restored"):
			return "Apple 已保存会话失效，请输入当前账号密码重新连接"
		case strings.Contains(line, "ec=-22406") && strings.Contains(line, "correct password"):
			return "Apple 拒绝了当前密码，请确认使用的是这个账号的最新密码，并关闭密码管理器的旧自动填充"
		case strings.Contains(line, "ec=-22981") && strings.Contains(line, "tooManyCodesSent=true"):
			if strings.Contains(line, "noTrustedDevices=true") {
				return "Apple 已限制该账号继续发送短信验证码，且没有可用受信任设备；请停止重试，等待 Apple 解除限制后再连接"
			}
			return "Apple 已限制该账号继续发送短信验证码；请停止重试，等待 Apple 解除限制后再连接"
		case strings.Contains(line, "mobileme delegate refused status=1"):
			return "验证码已经通过，但 Apple 拒绝了 iCloud 服务会话；已保留验证会话，请继续处理账号激活或条款状态"
		case strings.Contains(line, "client push certificate/key are unavailable"):
			return "该账号的独立 Mac 推送身份尚未初始化，请重新连接"
		default:
			return "请检查该账号的本机诊断日志；设备身份仍保留"
		}
	}
	return ""
}

// setLaunching and setMessage record progress on the account when there is one,
// and on the server itself for the legacy single-account path.
func (s *Server) setLaunching(account *Account, value bool) {
	if account == nil {
		s.mu.Lock()
		s.launching = value
		s.mu.Unlock()
		return
	}
	account.mu.Lock()
	account.launching = value
	account.mu.Unlock()
}

func (s *Server) setMessage(account *Account, message string) {
	if account == nil {
		s.mu.Lock()
		s.message = message
		s.mu.Unlock()
		return
	}
	account.mu.Lock()
	account.message = message
	account.mu.Unlock()
}

func (s *Server) Bootstrap(ctx context.Context, appleID, password string) {
	if appleID != "" && password != "" {
		go func() {
			result := s.Start(ctx, credentials{AppleID: appleID, Password: password})
			if !result.OK {
				s.mu.Lock()
				s.message = result.Message
				s.mu.Unlock()
			}
		}()
	}
	go func() {
		s.accountsMu.Lock()
		accounts := append([]*Account(nil), s.accounts...)
		s.accountsMu.Unlock()
		for _, account := range accounts {
			if s.pipelineAccount(account) {
				continue
			}
			if ctx.Err() != nil || s.accountStatus(account).Running || !acceptedCheckpointReady(account.Dir, account.AppleID) || !verifiedGSACheckpointReady(account.Dir, account.AppleID) {
				continue
			}
			result := s.startSavedAccount(ctx, account)
			if !result.OK && result.Message != "正在启动，请稍等" {
				s.setMessage(account, result.Message)
			}
		}
	}()
}
func (s *Server) stop(ctx context.Context) error { return s.stopIn(ctx, s.config.Dir) }

// stopIn stops the worker that owns one account directory. Each account has its
// own worker, so stopping one leaves every other account running.
func (s *Server) stopIn(ctx context.Context, dir string) error {
	status := s.readStatusFrom(dir)
	if !status.Running {
		return nil
	}
	data, _ := json.Marshal(map[string]int{"pid": status.PID})
	if os.WriteFile(filepath.Join(dir, "stop-request.json"), data, 0600) != nil {
		return errors.New("无法停止后台")
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ticker.C:
			if !s.ownsIn(status.PID, dir) {
				return nil
			}
		case <-timer.C:
			return errors.New("后台仍在清理连接，请稍后查看")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func readJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "JSON required", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid input", 400)
		return false
	}
	return true
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-AppleServices-UI", "1")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Host != s.listener.Addr().String() {
		http.Error(w, "local host required", 403)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		origin := r.Header.Get("Origin")
		if (origin != "" && origin != s.URL()) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-App-Token")), []byte(s.token)) != 1 {
			http.Error(w, `{"message":"请刷新面板后重试"}`, 403)
			return
		}
		switch {
		case r.URL.Path == "/api/register" || strings.HasPrefix(r.URL.Path, "/api/register/"):
			s.serveRegistration(w, r)
			return
		case r.Method == "GET" && r.URL.Path == "/api/view":
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			json.NewEncoder(w).Encode(s.snapshotFor(ctx, r.URL.Query().Get("account")))
			return
		case r.Method == "POST" && r.URL.Path == "/api/start":
			var creds credentials
			if !readJSON(w, r, &creds) {
				return
			}
			if account := r.URL.Query().Get("account"); account != "" {
				json.NewEncoder(w).Encode(s.startAccount(r.Context(), s.lookup(account), creds))
				return
			}
			json.NewEncoder(w).Encode(s.Start(r.Context(), creds))
			return
		case r.Method == "POST" && r.URL.Path == "/api/accounts/add":
			// A new account always gets its own directory, so an existing
			// account's session is never overwritten.
			var creds credentials
			if !readJSON(w, r, &creds) {
				return
			}
			result, id := s.addAccount(r.Context(), creds)
			if id != "" {
				result.Message = strings.TrimSpace(result.Message)
			}
			json.NewEncoder(w).Encode(result)
			return
		case r.Method == "POST" && r.URL.Path == "/api/accounts/remove":
			var body struct {
				ID string `json:"id"`
			}
			if !readJSON(w, r, &body) {
				return
			}
			json.NewEncoder(w).Encode(s.removeAccount(r.Context(), body.ID))
			return
		case r.Method == "POST" && r.URL.Path == "/api/stop":
			var body struct {
				Account string `json:"account"`
			}
			if !readJSON(w, r, &body) {
				return
			}
			if body.Account == "" {
				body.Account = r.URL.Query().Get("account")
			}
			dir := s.config.Dir
			if body.Account == "" {
				if selected := s.lookup(""); selected != nil {
					body.Account = selected.ID
				}
			}
			if body.Account != "" {
				account := s.lookup(body.Account)
				if account == nil {
					json.NewEncoder(w).Encode(control.Result{Message: "账号不存在"})
					return
				}
				dir = account.Dir
			}
			result := control.Result{OK: true, Message: "后台已停止，登录状态已保留"}
			if err := s.stopIn(r.Context(), dir); err != nil {
				result.OK = false
				result.Message = "后台停止未完成，请稍后查看"
			}
			json.NewEncoder(w).Encode(result)
			return
		case r.Method == "POST" && r.URL.Path == "/api/action":
			var action control.Action
			if !readJSON(w, r, &action) {
				return
			}
			dir := s.config.Dir
			var selected *Account
			if account := r.URL.Query().Get("account"); account != "" {
				found := s.lookup(account)
				if found == nil {
					json.NewEncoder(w).Encode(control.Result{Message: "账号不存在"})
					return
				}
				selected = found
				dir = found.Dir
			} else {
				selected = s.lookup("")
				if selected != nil {
					dir = selected.Dir
				}
			}
			var result control.Result
			if selected != nil && action.Action == "family-remove-member" {
				result = s.removeFamilyMember(r.Context(), selected, action.Code)
				json.NewEncoder(w).Encode(result)
				return
			}
			if selected != nil && (action.Action == "activation-plan" || action.Action == "activation-accept") {
				result = s.activationAction(r.Context(), selected, action.Action == "activation-accept", action.Code)
				json.NewEncoder(w).Encode(result)
				return
			}
			if selected != nil && (action.Action == "family-info" || action.Action == "family-join-storage") {
				result = s.familyAction(r.Context(), selected, action.Action == "family-join-storage")
				cacheSafeActionResult(selected, &result)
				json.NewEncoder(w).Encode(result)
				return
			}
			// Storage reads use the accepted iCloud checkpoint even while the
			// account worker is online. This keeps automatic cache maintenance
			// from occupying the same control channel as HME and other commands.
			if (action.Action == "storage-info" || action.Action == "storage-plan") && selected != nil && acceptedCheckpointReady(dir, selected.AppleID) {
				result = s.acceptedStorageAction(r.Context(), selected, action.Action == "storage-plan")
				cacheSafeActionResult(selected, &result)
				json.NewEncoder(w).Encode(result)
				return
			}
			if err := s.callFor(r.Context(), dir, "/action", action, &result); err != nil {
				result.Message = err.Error()
			}
			if selected != nil && result.Family != nil && strings.HasPrefix(action.Action, "family-") {
				helper := filepath.Join(filepath.Dir(s.config.Worker), "familyinspect.exe")
				cacheCtx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
				s.resolveFamilyOrganizer(cacheCtx, helper, selected, result.Family)
				cancel()
				if _, err := writeFamilyCache(selected, result.Family); err != nil {
					result.Message = strings.TrimSpace(result.Message + "；本地家庭状态缓存未保存")
				}
			}
			cacheSafeActionResult(selected, &result)
			json.NewEncoder(w).Encode(result)
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	file := ""
	switch r.URL.Path {
	case "/":
		file = "index.html"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case "/app.js":
		file = "app.js"
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case "/register.js":
		file = "register.js"
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case "/register.css":
		file = "register.css"
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case "/app.css":
		file = "app.css"
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("web/" + file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if file == "index.html" {
		data = bytes.ReplaceAll(data, []byte("__APP_TOKEN__"), []byte(s.token))
	}
	w.Write(data)
}
