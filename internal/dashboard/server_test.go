package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/control"
)

func TestDashboardRejectsCrossOriginAndMissingCapability(t *testing.T) {
	server, err := New(Config{Dir: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.listener.Close()
	for _, test := range []struct{ name, token, host, origin string }{
		{"missing-token", "", "", ""}, {"wrong-token", "wrong", "", ""},
		{"foreign-host", server.token, "attacker.example", ""}, {"cross-origin", server.token, "", "http://attacker.example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", server.URL()+"/api/view", nil)
			request.Header.Set("X-App-Token", test.token)
			if test.host != "" {
				request.Host = test.host
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			if recorder.Code != 403 {
				t.Fatal("cross-origin dashboard access accepted")
			}
		})
	}
	request := httptest.NewRequest("GET", server.URL()+"/api/view", nil)
	request.Header.Set("X-App-Token", server.token)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authorized dashboard request failed")
	}
}

func TestDashboardFiltersRuntimeSecretsAndBindsDaemonPID(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Dir: dir, Worker: executable, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.listener.Close()
	controller, err := control.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "running": true, "authenticated": true, "state": "online", "private_password": "do-not-disclose"})
	if os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600) != nil {
		t.Fatal("state write failed")
	}
	controller.SetDevices([]control.Device{{Name: "test-device", Installation: true}})
	result := server.snapshot(context.Background())
	encoded, _ := json.Marshal(result)
	if !result.ControlReady || len(result.Devices) != 1 || strings.Contains(string(encoded), "do-not-disclose") {
		t.Fatal("public state leaked or live daemon was not attached")
	}
	record := controller.Record
	record.PID++
	data, _ = json.Marshal(record)
	os.WriteFile(filepath.Join(dir, "control.json"), data, 0600)
	if _, err := server.record(); err == nil {
		t.Fatal("stale daemon capability accepted")
	}
	// Restore the capability so the fixture can remove it on shutdown.
	data, _ = json.Marshal(controller.Record)
	os.WriteFile(filepath.Join(dir, "control.json"), data, 0600)
}

func TestDashboardControlAddressCannotReachExternalHosts(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Config{Dir: dir, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.listener.Close()
	for _, target := range []string{"https://127.0.0.1:8080", "http://attacker.example:8080", "http://127.0.0.1:8080/path", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080?secret=yes"} {
		data, _ := json.Marshal(control.Record{PID: os.Getpid(), URL: target, Token: strings.Repeat("a", 64)})
		os.WriteFile(filepath.Join(dir, "control.json"), data, 0600)
		if _, err := server.record(); err == nil {
			t.Fatal("unsafe control destination accepted")
		}
	}
	parsed, _ := url.Parse(server.URL())
	if parsed.Hostname() != "127.0.0.1" {
		t.Fatal("dashboard listener was public")
	}
}

func TestAccountPrivateControlKeepsRenamedLiveWorkerOnline(t *testing.T) {
	root := t.TempDir()
	accountDir := filepath.Join(root, "accounts", "04-account")
	if err := os.MkdirAll(accountDir, 0700); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Dir: root, Worker: filepath.Join(root, "bin", "replacement-worker.exe"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.listener.Close()
	controller, err := control.New(accountDir)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	data, _ := json.Marshal(Status{PID: os.Getpid(), Running: true, Authenticated: true, PushConnected: true, State: "online", LastDeviceQueryAt: time.Now()})
	if err := os.WriteFile(filepath.Join(accountDir, "runtime.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	status := server.readStatusFrom(accountDir)
	if !status.Running || !status.Authenticated || !status.PushConnected || status.State != "online" {
		t.Fatalf("account-private live control was ignored: %#v", status)
	}
}

func TestDeadSavedPIDDoesNotClaimOnlineOrSkipLogin(t *testing.T) {
	dir := t.TempDir()
	server, err := New(Config{Dir: dir, Worker: filepath.Join(dir, "worker.exe"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer server.listener.Close()
	const deadPID = 2147483647
	data, _ := json.Marshal(Status{PID: deadPID, Running: true, Authenticated: true, PushConnected: true, State: "online"})
	os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600)
	data, _ = json.Marshal(control.Record{PID: deadPID, URL: "http://127.0.0.1:1", Token: strings.Repeat("a", 64)})
	os.WriteFile(filepath.Join(dir, "control.json"), data, 0600)
	status := server.readStatus()
	if status.Running || status.Authenticated || status.PushConnected || status.State == "online" {
		t.Fatal("dead saved PID presented as an online worker")
	}
	result := server.Start(context.Background(), credentials{})
	if result.OK || result.Message == "已连接到现有后台" {
		t.Fatal("stale capability prevented a new login")
	}
}

func TestLastProcessErrorDoesNotExposeLogSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err")
	if err := os.WriteFile(path, []byte("first\n\n  second   failure\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := lastProcessError(path); got == "" || strings.Contains(got, "second") {
		t.Fatalf("got %q", got)
	}
}

func TestLastProcessErrorMapsRejectedPasswordWithoutExposingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err")
	line := "trusted-device: gsa: complete rejected (protocol=s2k, ec=-22406) - server said: Enter the correct password for this Apple Account."
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := lastProcessError(path)
	if !strings.Contains(got, "Apple 拒绝了当前密码") {
		t.Fatalf("unexpected message %q", got)
	}
	if strings.Contains(got, "-22406") || strings.Contains(got, "trusted-device") {
		t.Fatalf("raw diagnostic leaked in %q", got)
	}
}

func TestLastProcessErrorMapsSMSRateLimitWithoutExposingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err")
	line := "trusted-device: gsa: fetch boot_args page: status 423; ec=-22981; tooManyCodesSent=true; noTrustedDevices=true"
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := lastProcessError(path)
	if !strings.Contains(got, "限制该账号继续发送短信验证码") || !strings.Contains(got, "没有可用受信任设备") {
		t.Fatalf("unexpected message %q", got)
	}
	for _, secret := range []string{"-22981", "tooManyCodesSent", "noTrustedDevices", "trusted-device"} {
		if strings.Contains(got, secret) {
			t.Fatalf("raw diagnostic %q leaked in %q", secret, got)
		}
	}
}

func TestLastProcessErrorSeparatesAcceptedCodeFromDelegateRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.err")
	line := "trusted-device: appleservices: iCloud login: icloud: mobileme delegate refused status=1"
	if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := lastProcessError(path)
	if !strings.Contains(got, "验证码已经通过") || !strings.Contains(got, "已保留验证会话") {
		t.Fatalf("unexpected message %q", got)
	}
	if strings.Contains(got, "mobileme") || strings.Contains(got, "status=1") {
		t.Fatalf("raw delegate diagnostic leaked in %q", got)
	}
}

func TestParseFamilyStatusUsesOnlyMarkedRedactedJSON(t *testing.T) {
	output := "helper diagnostic\n" + familyStatusPrefix + `{"member":true,"members":3,"organizers":1,"self":1,"storage_known":true,"storage_participating":true,"storage_available":true,"storage_enabled":true,"storage_bytes":53687091200}` + "\n"
	status, err := parseFamilyStatus(output)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Member || status.Members != 3 || status.Self != 1 || !status.StorageParticipating || status.StorageBytes != 53687091200 {
		t.Fatalf("unexpected family status: %#v", status)
	}
	if _, err := parseFamilyStatus(`{"member":true}`); err == nil {
		t.Fatal("unmarked helper output accepted")
	}
}

func TestUniquePoolOrganizerRequiresOneMatchingFamily(t *testing.T) {
	current := &control.FamilyInfo{Member: true, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}
	organizer := &Account{ID: "04-account", Name: "primary"}
	member := &Account{ID: "03-account", Name: "member"}
	candidates := []familyCandidate{
		{account: organizer, family: &control.FamilyInfo{Member: true, Organizer: true, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}},
		{account: member, family: &control.FamilyInfo{Member: true, Organizer: false, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}},
	}
	if got := uniquePoolOrganizer(current, candidates); got != organizer {
		t.Fatalf("unexpected organizer: %#v", got)
	}
	candidates = append(candidates, familyCandidate{account: &Account{ID: "other"}, family: &control.FamilyInfo{Member: true, Organizer: true, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}})
	if got := uniquePoolOrganizer(current, candidates); got != nil {
		t.Fatalf("ambiguous organizer accepted: %#v", got)
	}
}

func TestFamilyRemovalTargetRequiresExactNonOrganizerAccount(t *testing.T) {
	organizer := &Account{ID: "04-account"}
	target := &Account{ID: "01-primary"}
	accounts := []*Account{target, organizer}
	got, err := familyRemovalTarget(organizer, "01-primary", accounts)
	if err != nil || got != target {
		t.Fatalf("unexpected target=%#v err=%v", got, err)
	}
	for _, invalid := range []string{"", "04-account", "../01-primary", "missing"} {
		if got, err := familyRemovalTarget(organizer, invalid, accounts); err == nil || got != nil {
			t.Fatalf("invalid target %q accepted as %#v", invalid, got)
		}
	}
}

func TestSameFamilySummaryRequiresCurrentMatchingMember(t *testing.T) {
	organizer := &control.FamilyInfo{Member: true, Organizer: true, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}
	member := &control.FamilyInfo{Member: true, Members: 3, Organizers: 1, StorageKnown: true, StorageBytes: 50 << 30}
	if !sameFamilySummary(organizer, member) {
		t.Fatal("matching member was not bound to organizer")
	}
	member.Members = 2
	if sameFamilySummary(organizer, member) {
		t.Fatal("different family size was accepted")
	}
	member.Members = 3
	member.StorageBytes = 200 << 30
	if sameFamilySummary(organizer, member) {
		t.Fatal("different shared storage was accepted")
	}
}

func TestWorkerHTTPErrorDistinguishesOldWorkerFromBusy(t *testing.T) {
	old := workerHTTPError(http.StatusBadRequest, []byte(`{"message":"unknown action"}`), control.Action{Action: "hme-list"})
	if old == nil || !strings.Contains(old.Error(), "版本不支持隐私邮箱") || strings.Contains(old.Error(), "忙") {
		t.Fatalf("old worker was misreported: %v", old)
	}
	busy := workerHTTPError(http.StatusGatewayTimeout, nil, control.Action{Action: "hme-list"})
	if busy == nil || !strings.Contains(busy.Error(), "正在处理其他操作") {
		t.Fatalf("busy worker was misreported: %v", busy)
	}
}

func TestReconnectRejectsDifferentAccountBeforeStartingWorker(t *testing.T) {
	s := &Server{}
	a := &Account{AppleID: "owner@example.test", Dir: t.TempDir()}
	result := s.startAccount(context.Background(), a, credentials{AppleID: "other@example.test", Password: "test"})
	if result.OK || !strings.Contains(result.Message, "不一致") {
		t.Fatal("different account accepted")
	}
	entries, _ := os.ReadDir(a.Dir)
	if len(entries) != 0 {
		t.Fatal("rejected login changed account state")
	}
}
