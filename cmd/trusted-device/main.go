package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/anisette"
	"github.com/Laky-64/appleservices/aps"
	"github.com/Laky-64/appleservices/gsa"
	"github.com/Laky-64/appleservices/icloud"
	"github.com/Laky-64/appleservices/internal/circle"
	"github.com/Laky-64/appleservices/internal/control"
	"github.com/Laky-64/appleservices/nativeadi"
)

func main() {
	if err := run(); err != nil {
		// Apple's own wording decides whether a rejection means a throttled
		// account, a refused device identity, or a malformed request; the bare
		// error code alone leaves the operator guessing.
		var rejection *gsa.AuthenticationError
		if errors.As(err, &rejection) {
			fmt.Fprintln(os.Stderr, "trusted-device:", rejection.Explain())
		} else {
			fmt.Fprintln(os.Stderr, "trusted-device:", err)
		}
		os.Exit(1)
	}
}

func run() (runErr error) {
	dir := flag.String("dir", "./state", "existing device/session directory")
	name := flag.String("name", "appleservices", "name announced to Apple")
	certificatePath := flag.String("push-cert", "", "Apple-issued client push certificate PEM file")
	keyPath := flag.String("push-key", "", "matching client push private key PEM file")
	codeProvider := flag.String("code-provider", "", "absolute path to a working local ADI code-provider executable")
	server := flag.String("anisette", "https://ani.sidestore.io", "anisette server for this identity")
	method := flag.String("twofactor", "sms", "initial sign-in method: sms, sms-existing, trusted-device or circle")
	check := flag.Bool("check", false, "check local prerequisites without contacting Apple")
	native := flag.Bool("native-push", false, "use the installed iCloud for Windows APS daemon")
	probeNative := flag.Bool("probe-native-push", false, "check native token and local callback; no account registration")
	listenOnly := flag.Bool("listen-only", false, "register and observe notifications; do not approve requests or generate codes")
	listenFor := flag.Duration("listen-for", 0, "stop after this duration; 0 keeps listening")
	useNativeADI := flag.Bool("native-adi", false, "generate headers and verification codes with the verified Windows native ADI module")
	adiLibrary := flag.String("adi-dll", "./.cache/openbubbles-store/rust_lib_bluebubbles.dll", "verified OpenBubbles 1.18.800.0 native module path")
	python := flag.String("python", "python", "64-bit Python executable for the native ADI bridge")
	background := flag.Bool("background", false, "keep running with closed stdin; never approve requests or log generated codes")
	refreshEvery := flag.Duration("refresh-every", 10*time.Minute, "refresh account authentication, registration and device status at this interval")
	controlAPI := flag.Bool("control-api", false, "enable authenticated local dashboard control")
	guiAuth := flag.Bool("gui-auth", false, "complete initial SMS verification through the dashboard")
	resumeSession := flag.Bool("resume-session", false, "restore the account-bound GSA and accepted iCloud sessions without a password")
	registerOnly := flag.Bool("register-only", false, "register this device, emit a receipt, and exit without approving requests")
	flag.Parse()
	if *background {
		*controlAPI = true
	}
	if *useNativeADI {
		if *codeProvider != "" {
			return errors.New("native-adi cannot be combined with an external code-provider")
		}
		// Mac-style ADI uses the certificate/token saved for this account.
		// Windows daemon push is a separately requested diagnostic backend;
		// it must not silently make every Mac profile share one push token.
	}
	if *probeNative {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		length, err := aps.ProbeNativePush(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Native APS token length: %d. Local COM callback verified.\n", length)
		fmt.Println("Account trust and remote code delivery have not been verified.")
		return nil
	}
	if *method != "sms" && *method != "sms-existing" && *method != "trusted-device" && *method != "circle" {
		return errors.New("twofactor must be sms, sms-existing, trusted-device or circle")
	}
	if *listenFor < 0 {
		return errors.New("listen-for must not be negative")
	}
	if *refreshEvery < time.Minute {
		return errors.New("refresh-every must be at least one minute")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*dir) == "" {
		return errors.New("name and state directory are required")
	}
	if !*listenOnly && !*useNativeADI {
		if err := aps.ValidateCodeProvider(*codeProvider); err != nil {
			return err
		}
	}
	store := fileStore{dir: *dir}
	device, err := store.LoadDevice()
	if err != nil || device == nil || len(device.Identifier) != 16 || (!*useNativeADI && !(anisette.State{AdiPB: device.ProvisioningBlob}).Provisioned()) {
		return errors.New("a provisioned device state is required; complete the existing login first")
	}
	savedPath := filepath.Join(*dir, "push.json")
	var pushState aps.State
	var push pushTransport
	if *native {
		if *certificatePath != "" || *keyPath != "" {
			return errors.New("native-push cannot be combined with push-cert or push-key")
		}
		push, err = aps.NewNativeClient()
	} else {
		if *certificatePath != "" || *keyPath != "" {
			if *certificatePath == "" || *keyPath == "" {
				return errors.New("both push-cert and push-key are required")
			}
			pushState.CertificatePEM, err = os.ReadFile(*certificatePath)
			if err != nil {
				return errors.New("could not read client push certificate")
			}
			pushState.PrivateKeyPEM, err = os.ReadFile(*keyPath)
			if err != nil {
				return errors.New("could not read client push private key")
			}
		} else {
			data, err := os.ReadFile(savedPath)
			if err != nil {
				return errors.New("client push certificate/key are unavailable; provide push-cert and push-key")
			}
			if err := json.Unmarshal(data, &pushState); err != nil {
				return errors.New("invalid saved push state")
			}
		}
		push, err = aps.NewClient(pushState)
	}
	if err != nil {
		return err
	}
	if *check {
		if *useNativeADI {
			library, err := filepath.Abs(*adiLibrary)
			if err != nil {
				return errors.New("could not resolve native ADI module path")
			}
			if err := nativeadi.ValidateLibrary(library); err != nil {
				return err
			}
			if err := nativeadi.CheckRuntime(*python); err != nil {
				return err
			}
		}
		fmt.Println("Local inputs available. Apple trust and code delivery have not been verified.")
		return nil
	}
	appleID, password := os.Getenv("APPLESERVICES_APPLEID"), os.Getenv("APPLESERVICES_PASSWORD")
	if appleID == "" || (!*resumeSession && password == "") {
		return errors.New("APPLESERVICES_APPLEID and APPLESERVICES_PASSWORD are required")
	}
	os.Unsetenv("APPLESERVICES_APPLEID")
	os.Unsetenv("APPLESERVICES_PASSWORD")
	status := runtimeStatus{PID: os.Getpid(), Running: true, State: "starting"}
	if err := status.save(*dir); err != nil {
		return err
	}
	defer func() {
		status.Running, status.PushConnected = false, false
		status.Authenticated = false
		if status.State != "verification_required" {
			if runErr != nil {
				status.FailureStage, status.State = status.State, "failed"
			} else {
				status.State = "stopped"
			}
			status.VerificationRequired = false
		}
		if err := status.save(*dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	var panel *control.Server
	var panelCommands <-chan control.Command
	if *controlAPI {
		panel, err = control.New(*dir)
		if err != nil {
			return err
		}
		defer panel.Close()
		panelCommands = panel.Commands
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go watchStop(ctx, cancel, *dir, status.PID)
	if *listenFor > 0 {
		var stopListening context.CancelFunc
		ctx, stopListening = context.WithTimeout(ctx, *listenFor)
		defer stopListening()
	}
	fmt.Fprintln(os.Stderr, "Authenticating the APS connection ...")
	connectCtx, stop := context.WithTimeout(ctx, 45*time.Second)
	err = push.Connect(connectCtx)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { push.Close() }()
	status.PushConnected = true
	status.State = "initializing"
	if err := status.save(*dir); err != nil {
		return err
	}
	if !*native {
		if err := savePush(savedPath, push.State()); err != nil {
			return err
		}
	}
	events := make(chan aps.Event, 16)
	errorsCh := make(chan error, 1)
	listen := func(connection pushTransport) {
		go func() {
			errorsCh <- connection.Listen(ctx, func(event aps.Event) error {
				select {
				case events <- event:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
	}
	listen(push)
	identity := sha256.Sum256(device.Identifier)
	serial := device.SerialNumber
	if serial == "" {
		serial = "AS" + strings.ToUpper(fmt.Sprintf("%x", identity[:5]))
	}
	options := []appleservices.Option{appleservices.WithAnisetteServers(*server), appleservices.WithDeviceSerialNumber(serial), appleservices.WithPushToken(push.State().Token)}
	var localADI *nativeadi.Client
	if *useNativeADI {
		library, err := filepath.Abs(*adiLibrary)
		if err != nil {
			return errors.New("could not resolve native ADI module path")
		}
		localADI, err = nativeadi.NewClient(nativeadi.Config{LibraryPath: library, ProfileDir: filepath.Join(*dir, "native-adi"),
			Python: *python, Identifier: device.Identifier, Serial: serial, PushToken: push.State().Token})
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Starting the verified local ADI generation module ...")
		startCtx, stop := context.WithTimeout(ctx, 90*time.Second)
		err = localADI.Start(startCtx)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		defer localADI.Close()
		// APPLESERVICES_REMOTE_ANISETTE keeps the local module for six-digit
		// generation while headers come from the configured anisette server.
		// The two were once the same code path, so this isolates which of them
		// a delegate refusal is about.
		if os.Getenv("APPLESERVICES_REMOTE_ANISETTE") == "" {
			options = append(options, appleservices.WithAnisette(localADI))
		}
		fmt.Fprintln(os.Stderr, "Local six-digit generation is available for the same device identity.")
	}
	generateCode := func(codeCtx context.Context) (string, error) {
		if localADI != nil {
			return localADI.Code(codeCtx)
		}
		return aps.CodeFromProcess(codeCtx, *codeProvider, codeRequest(device))
	}
	status.State = "signing_in"
	if err := status.save(*dir); err != nil {
		return err
	}
	var login *appleservices.Login
	if *resumeSession {
		accepted, loadErr := store.LoadAcceptedICloud()
		if loadErr != nil {
			return errors.New("saved login session cannot be restored; reconnect with the current password")
		}
		login, err = appleservices.ResumeLogin(appleID, store, accepted, options...)
	} else {
		login, err = appleservices.BeginLogin(appleservices.Credentials{AppleID: appleID, Password: password}, store, options...)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	input := bufio.NewReader(os.Stdin)
	if login.NeedsTwoFactor() && *method == "circle" {
		if panel == nil || !*guiAuth {
			return errors.New("Circle login requires the dashboard control API")
		}
		if err := completeCircleLogin(ctx, login, push.State().Token, events, errorsCh, panelCommands, &status, *dir); err != nil {
			return err
		}
	}
	if login.NeedsTwoFactor() {
		if *background && (!*guiAuth || panel == nil) {
			status.VerificationRequired = true
			status.State = "verification_required"
			return errVerificationRequired
		}
		factor := appleservices.SMS
		if *method == "trusted-device" {
			factor = appleservices.TrustedDevice
		}
		if *method != "sms-existing" {
			if err := login.RequestCode(factor); err != nil {
				return err
			}
		}
		if panel != nil && *guiAuth {
			status.State, status.VerificationRequired = "awaiting_sms", true
			if factor == appleservices.TrustedDevice {
				status.State = "awaiting_device_code"
			} else if *method == "sms-existing" {
				status.State = "awaiting_existing_sms"
			}
			if err := status.save(*dir); err != nil {
				return err
			}
			lastSent := time.Now()
			verified := false
			for !verified {
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return nil
				case command := <-panelCommands:
					if command.Context.Err() != nil {
						continue
					}
					result := control.Result{}
					switch command.Action.Action {
					case "resend-sms":
						if time.Since(lastSent) < time.Minute {
							result.Message = "请间隔一分钟再重发短信"
						} else if login.RequestCode(factor) != nil {
							result.Message = "短信发送失败，请稍后重试"
						} else {
							lastSent = time.Now()
							result.OK = true
							result.Message = "短信已发送"
						}
					case "submit-sms":
						// The reason Apple gives is the only thing that
						// separates an expired code from a rejected one, so it
						// is reported instead of a fixed message.
						if err := login.SubmitCode(factor, command.Code); err != nil {
							result.Message = "验证码未被接受：" + err.Error()
						} else {
							result.OK = true
							result.Message = "验证码已验证，正在完成登录"
							verified = true
						}
					default:
						result.Message = "请先完成短信验证"
					}
					command.Reply <- result
				}
				if stopRequested(*dir, status.PID) {
					return nil
				}
			}
			status.State, status.VerificationRequired = "signing_in", false
			if err := status.save(*dir); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "Enter the initial %s verification code: ", *method)
			code, err := input.ReadString('\n')
			if err != nil {
				return errors.New("could not read initial verification code")
			}
			if err := login.SubmitCode(factor, strings.TrimSpace(code)); err != nil {
				return err
			}
		}
	}
	registration := appleservices.DeviceRegistration{Name: *name, PushToken: push.State().Token}
	if !*listenOnly {
		generationCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		_, err = generateCode(generationCtx)
		stop()
		if err != nil {
			return fmt.Errorf("code-generation prerequisite failed before account registration: %w", err)
		}
	}
	// The reference client sends GSA postdata after 2FA and before requesting
	// iCloud delegates. For the explicit NAC diagnostic, allow an isolated
	// delegate retry because accountsetupcheck does not mutate registration.
	if *registerOnly || (!*resumeSession && strings.TrimSpace(os.Getenv("APPLESERVICES_SKIP_DEVICE_REGISTRATION")) != "1") {
		if err := login.PrepareDeviceRegistration(registration); err != nil {
			return err
		}
	}
	client, err := login.Client()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	var active maintainedLogin = login
	accountID := client.ADSID
	circles := newCircleManager(accountID, client.DSID)
	if ctx.Err() != nil {
		return nil
	}
	fmt.Println("Apple accepted the registration request. Waiting for real AuthKit delivery; trusted-device reception is not yet verified.")
	devices, err := active.AccountDevices()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not verify account device status:", err)
	} else {
		status.setDevices(devices, *name)
		if panel != nil {
			panel.SetDevices(panelDevices(devices, *name))
		}
		for _, entry := range devices {
			if entry.Name == *name {
				fmt.Println("An entry with the announced name is visible in the account device list; this alone does not prove trust.")
				if entry.IsTrustedDevice != nil {
					fmt.Printf("Account-reported trusted status: %t.\n", *entry.IsTrustedDevice)
				}
			}
		}
	}
	if err := active.SaveSession(); err != nil {
		return err
	}
	if *registerOnly {
		// Registration acceptance does not prove trusted-device notification delivery.
		fmt.Println(`{"state":"success","registration_accepted":true,"trust_verified":false}`)
		return nil
	}
	status.Authenticated, status.State = true, "online"
	if err != nil {
		status.State = "device_query_failed"
	}
	status.LastRefreshAt = time.Now().UTC()
	if panel != nil {
		panel.SetReady(!*listenOnly)
	}
	if err := status.save(*dir); err != nil {
		return err
	}
	fmt.Printf("Session remains active; account renewal runs every %s.\n", *refreshEvery)
	if *background {
		fmt.Println("Background mode: closed stdin does not stop reception; requests are never approved automatically.")
	}
	if *listenOnly {
		fmt.Println("Observation mode: no codes will be generated and no sign-in requests will be approved.")
	} else if !*background {
		fmt.Println("Type code to generate a verification code, devices to query device status, or wait for a sign-in request.")
	}
	var pendingMu sync.Mutex
	var pending string
	var pendingKind string
	sendCircle := func(event aps.Event, response circle.Response) error {
		sender, ok := active.(interface {
			RespondToCircle(appleservices.CircleReply) error
		})
		if !ok {
			return errors.New("Circle sender unavailable")
		}
		return sender.RespondToCircle(appleservices.CircleReply{Transaction: event.CircleTransactionID, IDMSData: event.IDMSData, Step: response.Step, PAKE: response.PAKE, ErrorCode: response.ErrorCode, PushToken: registration.PushToken})
	}
	freshCircleCode := func() (string, error) {
		generationCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		return generateCode(generationCtx)
	}
	var reconnectTimer *time.Timer
	var reconnectTick <-chan time.Time
	reconnected := make(chan pushConnection)
	reconnectDelay := 2 * time.Second
	defer func() {
		if reconnectTimer != nil {
			reconnectTimer.Stop()
		}
	}()
	scheduleReconnect := func() {
		reconnectTimer = time.NewTimer(reconnectDelay)
		reconnectTick = reconnectTimer.C
		reconnectDelay = nextPushDelay(reconnectDelay)
	}
	var choices <-chan string
	if !*listenOnly && !*background {
		inputChoices := make(chan string)
		choices = inputChoices
		go func() {
			defer close(inputChoices)
			for {
				line, err := input.ReadString('\n')
				if err != nil {
					return
				}
				select {
				case inputChoices <- strings.TrimSpace(line):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	refresh := time.NewTicker(*refreshEvery)
	defer refresh.Stop()
	beginRenewal := func() (maintainedLogin, error) {
		if *resumeSession {
			accepted, err := store.LoadAcceptedICloud()
			if err != nil {
				return nil, err
			}
			return appleservices.ResumeLogin(appleID, store, accepted, options...)
		}
		return appleservices.BeginLogin(appleservices.Credentials{AppleID: appleID, Password: password}, store, options...)
	}
	renew := func() control.Result {
		renewed, devices, err := renewSessionMode(beginRenewal, accountID, registration, !*resumeSession)
		if err != nil {
			status.State = "refresh_failed"
			if errors.Is(err, errVerificationRequired) {
				status.State, status.VerificationRequired = "verification_required", true
			}
			status.save(*dir)
			return control.Result{Message: "续登录失败，请检查登录状态"}
		}
		active = renewed
		status.State, status.Authenticated = "online", true
		status.LastRefreshAt = time.Now().UTC()
		status.RefreshCount++
		status.setDevices(devices, *name)
		if panel != nil {
			panel.SetDevices(panelDevices(devices, *name))
		}
		if err := status.save(*dir); err != nil {
			return control.Result{Message: "无法保存登录状态"}
		}
		return control.Result{OK: true, Message: "登录已续期，设备列表已更新"}
	}
	var hmeClient *icloud.HideMyEmailClient
	var accountManagementClient *icloud.AppleAccountClient
	pendingHMECandidate := ""
	pendingReachableEmail := ""
	loadHME := func(actionCtx context.Context) (*icloud.HideMyEmailInfo, error) {
		if err := requireHMEFamilyMember(*dir, time.Now().UTC()); err != nil {
			return nil, err
		}
		if hmeClient == nil {
			var clientErr error
			hmeClient, clientErr = icloud.NewHideMyEmailClient(*dir, appleID)
			if clientErr != nil {
				return nil, clientErr
			}
		}
		info, err := hmeClient.List(actionCtx)
		if !errors.Is(err, icloud.ErrHMEAuthenticationRequired) {
			return info, err
		}
		if err := hmeClient.Authenticate(actionCtx, password, generateCode); err != nil {
			return nil, err
		}
		return hmeClient.List(actionCtx)
	}
	performAction := func(actionCtx context.Context, action, visibleRequestID, commandCode, commandLabel, commandNote string) control.Result {
		if action == "hme-accept-terms" {
			queryCtx, done := context.WithTimeout(actionCtx, 115*time.Second)
			defer done()
			if err := requireHMEFamilyMember(*dir, time.Now().UTC()); err != nil {
				return control.Result{Message: "当前账号不满足隐藏邮件地址家庭条件：" + err.Error(), Stage: "hme-family"}
			}
			var clientErr error
			hmeClient, clientErr = icloud.NewHideMyEmailClient(*dir, appleID)
			if clientErr != nil {
				return control.Result{Message: "无法初始化隐藏邮件地址客户端", Stage: "hme-session"}
			}
			err := hmeClient.AcceptPendingTerms(queryCtx)
			if errors.Is(err, icloud.ErrHMEWebRepairSessionUnavailable) {
				err = hmeClient.AuthenticateAndAcceptTerms(queryCtx, password, generateCode)
			}
			if err != nil {
				return control.Result{Message: "iCloud Web 条款修复未完成：" + err.Error(), Stage: "hme-terms"}
			}
			info, err := hmeClient.List(queryCtx)
			if errors.Is(err, icloud.ErrHMEAuthenticationRequired) {
				if err = hmeClient.Authenticate(queryCtx, password, generateCode); err == nil {
					info, err = hmeClient.List(queryCtx)
				}
			}
			if err != nil {
				return control.Result{Message: "条款已提交，但隐藏邮件地址只读验收失败：" + err.Error(), Stage: "hme-list"}
			}
			return control.Result{OK: true, Message: "iCloud Web 条款已接受，隐藏邮件地址只读验收通过", Stage: "hme-list", HideMyEmail: info}
		}
		if strings.HasPrefix(action, "hme-") {
			queryCtx, done := context.WithTimeout(actionCtx, 115*time.Second)
			defer done()
			info, err := loadHME(queryCtx)
			if err != nil {
				return control.Result{Message: "隐藏邮件地址会话未就绪：" + err.Error(), Stage: "hme-session"}
			}
			label := strings.TrimSpace(commandLabel)
			note := strings.TrimSpace(commandNote)
			alias := strings.TrimSpace(commandCode)
			if len(label) > 128 || len(note) > 1024 || len(alias) > 320 {
				return control.Result{Message: "标签、备忘录或邮箱地址过长", Stage: "hme-validation"}
			}
			switch action {
			case "hme-list":
				return control.Result{OK: true, Message: "已读取当前子号的隐藏邮件地址", Stage: "hme-list", HideMyEmail: info}
			case "hme-generate":
				candidate, generateErr := hmeClient.Generate(queryCtx, "zh-cn")
				if generateErr != nil {
					return control.Result{Message: "Apple 未返回可用的候选地址", Stage: "hme-generate"}
				}
				pendingHMECandidate = candidate
				return control.Result{OK: true, Message: "Apple 已生成候选地址；确认后才会正式创建", Stage: "hme-candidate", HideMyEmail: info, HMECandidate: candidate}
			case "hme-reserve":
				if pendingHMECandidate == "" || !strings.EqualFold(alias, pendingHMECandidate) || label == "" {
					return control.Result{Message: "候选地址或标签绑定无效，请重新生成", Stage: "hme-binding"}
				}
				if reserveErr := hmeClient.Reserve(queryCtx, pendingHMECandidate, label, note); reserveErr != nil {
					// Apple's reserve endpoint can apply the alias and still return a
					// failed/late response. Reconcile against a fresh authoritative
					// list before reporting uncertainty; never submit reserve twice.
					readbackCtx, stopReadback := context.WithTimeout(actionCtx, 25*time.Second)
					readback, readbackErr := hmeClient.List(readbackCtx)
					stopReadback()
					if readbackErr == nil && reservedAliasPresent(readback, pendingHMECandidate, label, note) {
						pendingHMECandidate = ""
						return control.Result{OK: true, Message: "隐藏邮件地址已创建；Apple 创建响应异常，但已通过实时列表回读确认", Stage: "hme-reserve-reconciled", HideMyEmail: readback}
					}
					return control.Result{Message: "候选地址未确认创建；请刷新列表后核对，不要重复提交", Stage: "hme-reserve"}
				}
				pendingHMECandidate = ""
			case "hme-forward":
				if alias == "" {
					return control.Result{Message: "请选择 Apple 返回的转发邮箱", Stage: "hme-forward-validation"}
				}
				if forwardErr := hmeClient.UpdateForwardTo(queryCtx, alias); forwardErr != nil {
					return control.Result{Message: "转发邮箱更新未完成；请重新读取候选地址", Stage: "hme-forward"}
				}
				readback, readbackErr := hmeClient.List(queryCtx)
				if readbackErr != nil || readback == nil || !strings.EqualFold(strings.TrimSpace(readback.SelectedForward), alias) {
					return control.Result{Message: "Apple 已接收更新，但默认转发邮箱尚未通过回读确认；请勿重复提交", Stage: "hme-forward-readback"}
				}
				return control.Result{OK: true, Message: "默认转发邮箱已更新，并通过 Apple 实时列表回读", Stage: "hme-forward-completed", HideMyEmail: readback}
			case "hme-reachable-start":
				if alias == "" || !strings.Contains(alias, "@") {
					return control.Result{Message: "请输入要添加并验证的转发邮箱", Stage: "hme-reachable-validation"}
				}
				var clientErr error
				accountManagementClient, clientErr = icloud.NewAppleAccountClientWithStore(*dir, appleID)
				if clientErr != nil {
					return control.Result{Message: "无法初始化 Apple 账户管理会话", Stage: "hme-reachable-session"}
				}
				if clientErr = accountManagementClient.Resume(queryCtx); clientErr != nil {
					if password == "" {
						accountManagementClient = nil
						return control.Result{Message: "没有可复用的 Apple 账户管理会话；请带密码重新连接当前子号一次", Stage: "reconnect-required"}
					}
					if clientErr = accountManagementClient.Authenticate(queryCtx, password, generateCode); clientErr != nil {
						failureStage, failureHTTP := accountManagementClient.AuthenticationFailure()
						accountManagementClient = nil
						return control.Result{Message: "Apple 账户管理认证未完成；密码未保存", Stage: "hme-reachable-auth-" + failureStage, HTTPStatus: failureHTTP}
					}
				}
				if clientErr = accountManagementClient.BeginReachableEmail(queryCtx, alias, password); clientErr != nil {
					failureStage, failureHTTP := accountManagementClient.AuthenticationFailure()
					accountManagementClient = nil
					return control.Result{Message: "Apple 未发送目标邮箱验证码；未修改账户资料", Stage: "hme-reachable-" + failureStage, HTTPStatus: failureHTTP}
				}
				pendingReachableEmail = alias
				return control.Result{OK: true, Message: "Apple 已向目标邮箱发送验证码；请输入邮件中的验证码完成添加并切换转发", Stage: "hme-reachable-code", HideMyEmail: info}
			case "hme-reachable-verify":
				if accountManagementClient == nil || pendingReachableEmail == "" {
					return control.Result{Message: "当前没有待完成的目标邮箱验证，请重新发送验证码", Stage: "hme-reachable-missing"}
				}
				target := pendingReachableEmail
				available := false
				if verifyErr := accountManagementClient.VerifyReachableEmail(queryCtx, alias); verifyErr != nil {
					// Apple can apply the reachable-email verification before its
					// account-management readback catches up. Reconcile against the
					// authoritative HME forwarding candidates before telling the user
					// that the one-time email code was rejected.
					info, err = hmeClient.List(queryCtx)
					available = reachableVerificationApplied(verifyErr, info, target)
					if err != nil || !available {
						return control.Result{Message: "邮箱验证码未被 Apple 接受；没有切换转发目标", Stage: "hme-reachable-verify"}
					}
				}
				for attempt := 0; !available && attempt < 8; attempt++ {
					info, err = hmeClient.List(queryCtx)
					if err == nil && hmeForwardTargetAvailable(info, target) {
						available = true
						break
					}
					select {
					case <-queryCtx.Done():
						attempt = 8
					case <-time.After(time.Second):
					}
				}
				if !available {
					pendingReachableEmail = ""
					return control.Result{OK: true, Message: "联系邮箱已添加并验证，但 Apple 尚未把它同步到隐私邮箱候选；请稍后读取地址再切换", Stage: "hme-reachable-added", HideMyEmail: info}
				}
				if forwardErr := hmeClient.UpdateForwardTo(queryCtx, target); forwardErr != nil {
					return control.Result{Message: "联系邮箱已添加，但转发目标更新未完成；请从候选列表手动保存", Stage: "hme-reachable-forward"}
				}
				readback, readbackErr := hmeClient.List(queryCtx)
				if readbackErr != nil || readback == nil || !strings.EqualFold(strings.TrimSpace(readback.SelectedForward), target) {
					return control.Result{Message: "联系邮箱已添加，Apple 已接收转发更新，但实时回读尚未确认；请勿重复提交", Stage: "hme-reachable-readback"}
				}
				pendingReachableEmail = ""
				return control.Result{OK: true, Message: "联系邮箱已添加并验证，所有隐私邮箱已切换到新目标并通过实时回读", Stage: "hme-reachable-completed", HideMyEmail: readback}
			case "hme-update":
				if alias == "" || label == "" {
					return control.Result{Message: "邮箱和标签不能为空", Stage: "hme-validation"}
				}
				if updateErr := hmeClient.Update(queryCtx, alias, label, note); updateErr != nil {
					return control.Result{Message: "标签或备忘录更新未完成", Stage: "hme-update"}
				}
			case "hme-deactivate":
				if setErr := hmeClient.SetActive(queryCtx, alias, false); setErr != nil {
					return control.Result{Message: "邮箱停用未完成", Stage: "hme-deactivate"}
				}
			case "hme-reactivate":
				if setErr := hmeClient.SetActive(queryCtx, alias, true); setErr != nil {
					return control.Result{Message: "邮箱恢复未完成", Stage: "hme-reactivate"}
				}
			case "hme-delete":
				if deleteErr := hmeClient.Delete(queryCtx, alias); deleteErr != nil {
					return control.Result{Message: "永久删除未完成；必须先停用并刷新状态", Stage: "hme-delete"}
				}
			default:
				return control.Result{Message: "不支持的隐藏邮件地址操作", Stage: "hme-action"}
			}
			info, err = hmeClient.List(queryCtx)
			if err != nil {
				return control.Result{Message: "操作已提交，但列表回读未完成；请勿重复操作", Stage: "hme-readback"}
			}
			messages := map[string]string{"hme-reserve": "隐藏邮件地址已创建并通过列表回读", "hme-update": "标签和备忘录已更新", "hme-deactivate": "隐藏邮件地址已停用", "hme-reactivate": "隐藏邮件地址已恢复", "hme-delete": "隐藏邮件地址已永久删除"}
			return control.Result{OK: true, Message: messages[action], Stage: action + "-completed", HideMyEmail: info}
		}
		if action == "storage-preview" {
			previewer, ok := active.(interface {
				PreviewStoragePurchase(context.Context) (*icloud.StoreFormInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持购买表单检查", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 75*time.Second)
			defer done()
			info, err := previewer.PreviewStoragePurchase(queryCtx)
			if err != nil {
				result := control.Result{Message: "表单检查未完成，未提交购买", Stage: "purchase-preview"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
				}
				return result
			}
			return control.Result{OK: true, Message: "已读取 Apple 购买表单，未提交购买", StoreForm: info}
		}
		if action == "storage-authenticate" {
			checker, ok := active.(interface {
				CheckStoragePurchaseSession(context.Context, string) (*icloud.StoreAuthenticationInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持购买会话验证", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 75*time.Second)
			defer done()
			info, err := checker.CheckStoragePurchaseSession(queryCtx, commandCode)
			if err != nil {
				result := control.Result{Message: "购买会话认证未完成，iCloud 登录会话已保留", Stage: "purchase-store-authentication"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("购买会话认证失败：%s，HTTP %d", failure.Stage, failure.HTTPStatus)
					if failure.Stage == "purchase-store-2fa" {
						result.Message = "Apple Store 需要独立的六位验证码，iCloud 登录仍保留"
					}
				}
				return result
			}
			return control.Result{OK: true, Message: "Apple Store 认证已通过，购买会话已保留；未提交购买", StoreAuthentication: info}
		}
		if action == "storage-skill-auth-local" {
			checker, ok := active.(interface {
				CheckSkillStoragePurchaseSession(context.Context, func() (string, error)) (*icloud.StoreAuthenticationInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持匹配 Store bag 的认证", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 3*time.Minute)
			defer done()
			info, err := checker.CheckSkillStoragePurchaseSession(queryCtx, func() (string, error) {
				return generateCode(queryCtx)
			})
			if err != nil {
				result := control.Result{Message: "Store 会话认证未完成，未提交购买", Stage: "purchase-store-authentication"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
				}
				return result
			}
			return control.Result{OK: true, Message: "匹配 Store bag 的认证已通过；未提交购买", StoreAuthentication: info}
		}
		if action == "storage-skill-auth-file" {
			checker, ok := active.(interface {
				CheckSkillStoragePurchaseSession(context.Context, func() (string, error)) (*icloud.StoreAuthenticationInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持实时 Store 验证码交接", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 3*time.Minute)
			defer done()
			info, err := checker.CheckSkillStoragePurchaseSession(queryCtx, func() (string, error) {
				return waitForFreshStoreCode(queryCtx, *dir)
			})
			if err != nil {
				result := control.Result{Message: "Store 会话认证未完成，未提交购买", Stage: "purchase-store-authentication"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
				}
				return result
			}
			return control.Result{OK: true, Message: "实时验证码已在同一 Store 会话中通过；未提交购买", StoreAuthentication: info}
		}
		if action == "storage-skill-purchase-us" {
			buyer, ok := active.(interface {
				SubmitSkillStoragePurchase(context.Context, appleservices.PurchaseTarget, string) (*appleservices.PurchaseResult, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持美区 iCloud+ 购买", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 90*time.Second)
			defer done()
			target := appleservices.PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 99, Currency: "USD", Period: "month"}
			purchase, err := buyer.SubmitSkillStoragePurchase(queryCtx, target, "290571")
			if err != nil {
				result := control.Result{Message: "美区购买或核验未完成；不得自动重试", Stage: "purchase"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
				}
				return result
			}
			return control.Result{OK: true, Purchase: purchase, Message: "美区 50GB 订阅和付费配额均已核验", Stage: "purchase-completed"}
		}
		if action == "family-info" || action == "family-add-self" || action == "family-join-storage" {
			executable, execErr := os.Executable()
			if execErr != nil {
				return control.Result{Message: "家庭工具不可用", Stage: "family-worker"}
			}
			helper := filepath.Join(filepath.Dir(executable), "familyinspect.exe")
			if info, statErr := os.Stat(helper); statErr != nil || !info.Mode().IsRegular() {
				return control.Result{Message: "家庭工具不可用", Stage: "family-worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 105*time.Second)
			defer done()
			args := []string{"-dir", *dir, "-safe-json"}
			env := []string(nil)
			message, stage := "Apple 已返回当前家庭状态", "family-info"
			if action == "family-join-storage" {
				args = append(args, "-join-shared-storage")
				message, stage = "已加入家庭共享的 iCloud+ 储存空间", "family-storage-joined"
			}
			if action == "family-add-self" {
				organizerID := strings.TrimSpace(commandCode)
				if organizerID == "" || organizerID == filepath.Base(filepath.Clean(*dir)) || strings.IndexFunc(organizerID, func(r rune) bool {
					return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-')
				}) >= 0 {
					return control.Result{Message: "组织者账号绑定无效", Stage: "family-binding"}
				}
				accountsRoot := filepath.Dir(filepath.Clean(*dir))
				organizerDir := filepath.Join(accountsRoot, organizerID)
				if filepath.Dir(organizerDir) != accountsRoot {
					return control.Result{Message: "组织者账号目录无效", Stage: "family-binding"}
				}
				args = []string{"-dir", organizerDir, "-add-pool-member-dir", *dir, "-target-password-env", "-safe-json"}
				env = []string{"APPLESERVICES_TARGET_PASSWORD=" + password}
				message, stage = "池内账号已加入家庭并通过双端回读", "family-added"
			}
			output, execErr := runFamilyHelper(queryCtx, helper, args, env)
			if execErr != nil {
				return control.Result{Message: "家庭操作未完成：" + lastNonEmptyLine(output), Stage: "family-action"}
			}
			if action == "family-add-self" {
				// The mutation runs under the organizer's identity; return a fresh
				// target-side view so the UI never treats an invitation or one-sided
				// response as completed membership.
				output, execErr = runFamilyHelper(queryCtx, helper, []string{"-dir", *dir, "-safe-json"}, nil)
				if execErr != nil {
					return control.Result{Message: "家庭已提交，但目标账号回读未完成：" + lastNonEmptyLine(output), Stage: "family-readback"}
				}
			}
			family, parseErr := parseFamilyStatus(output)
			if parseErr != nil {
				return control.Result{Message: "家庭状态回读无效", Stage: "family-readback"}
			}
			if action == "family-add-self" && (!family.Member || family.Self != 1) {
				return control.Result{Message: "目标账号尚未确认成为家庭成员", Stage: "family-readback", Family: family}
			}
			if action == "family-add-self" && family.StorageKnown && family.StorageAvailable && !family.StorageParticipating {
				output, execErr = runFamilyHelper(queryCtx, helper, []string{"-dir", *dir, "-join-shared-storage", "-safe-json"}, nil)
				if execErr != nil {
					return control.Result{Message: "家庭成员已加入，但共享储存加入未完成：" + lastNonEmptyLine(output), Stage: "family-storage", Family: family}
				}
				family, parseErr = parseFamilyStatus(output)
				if parseErr != nil || !family.StorageKnown || !family.StorageParticipating || !family.StorageAvailable || !family.StorageEnabled {
					return control.Result{Message: "家庭成员已加入，但共享储存回读未完整生效", Stage: "family-storage-readback", Family: family}
				}
				message = "池内账号已加入家庭并启用共享 iCloud+ 储存"
			}
			if action == "family-join-storage" && (!family.StorageKnown || !family.StorageParticipating || !family.StorageAvailable || !family.StorageEnabled) {
				return control.Result{Message: "家庭共享储存尚未完整生效", Stage: "family-storage-readback", Family: family}
			}
			return control.Result{OK: true, Message: message, Stage: stage, Family: family}
		}
		if action == "storage-info" {
			reader, ok := active.(interface {
				StorageInfo(context.Context) (*appleservices.StorageInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持储存查询，请更新后台", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(ctx, 30*time.Second)
			defer done()
			info, err := reader.StorageInfo(queryCtx)
			if err != nil {
				result := control.Result{Message: "储存查询未完成，登录会话已保留", Stage: "storage"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("储存查询失败：%s，HTTP %d。登录会话已保留", failure.Stage, failure.HTTPStatus)
				}
				return result
			}
			return control.Result{OK: true, Storage: info, Message: "Apple 已返回当前储存配额"}
		}
		if action == "storage-plan" {
			reader, ok := active.(interface {
				StoragePlanInfo(context.Context) (*appleservices.StoragePlanInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持方案查询，请更新后台", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(ctx, 30*time.Second)
			defer done()
			plans, err := reader.StoragePlanInfo(queryCtx)
			if err != nil {
				result := control.Result{Message: "可购方案查询未完成，登录会话已保留", Stage: "storage-plan"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("可购方案查询失败：%s，HTTP %d。登录会话已保留", failure.Stage, failure.HTTPStatus)
				}
				return result
			}
			return control.Result{OK: true, StoragePlan: plans, Message: "Apple 已返回可购储存方案"}
		}
		if action == "storage-probe" {
			queryCtx, done := context.WithTimeout(actionCtx, 90*time.Second)
			defer done()
			var probes []icloud.StorageEndpointProbe
			var err error
			// A local probe-urls.json (diagnostic only) replaces the built-in
			// endpoint list so probes can iterate without a worker restart.
			if specs := loadProbeSpecs(*dir); specs != nil {
				custom, ok := active.(interface {
					ProbeCustom(context.Context, []icloud.ProbeSpec, bool) ([]icloud.StorageEndpointProbe, error)
				})
				if !ok {
					return control.Result{Message: "后台版本不支持自定义探测", Stage: "worker"}
				}
				probes, err = custom.ProbeCustom(queryCtx, specs, commandCode == "store")
			} else {
				prober, ok := active.(interface {
					ProbeStorageEndpoints(context.Context, bool) ([]icloud.StorageEndpointProbe, error)
				})
				if !ok {
					return control.Result{Message: "后台版本不支持储存端点探测", Stage: "worker"}
				}
				probes, err = prober.ProbeStorageEndpoints(queryCtx, commandCode == "store")
			}
			if err != nil {
				result := control.Result{Message: "端点探测未完成", Stage: "storage-probe"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("端点探测未完成：%s，HTTP %d", failure.Stage, failure.HTTPStatus)
				}
				return result
			}
			return control.Result{OK: true, Probes: probes, Message: "只读探测完成（无变更）"}
		}
		if action == "storage-buy" {
			checker, ok := active.(interface {
				CheckStoragePurchaseSession(context.Context, string) (*icloud.StoreAuthenticationInfo, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持商店直购", Stage: "worker"}
			}
			buyer, ok := active.(interface {
				SubmitStorageBuyDirect(context.Context) (*appleservices.PurchaseResult, error)
			})
			if !ok {
				return control.Result{Message: "后台版本不支持商店直购", Stage: "worker"}
			}
			queryCtx, done := context.WithTimeout(actionCtx, 90*time.Second)
			defer done()
			if _, err := checker.CheckStoragePurchaseSession(queryCtx, commandCode); err != nil {
				result := control.Result{Message: "商店会话未建立，未提交购买", Stage: "purchase-store-authentication"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("商店会话失败：%s，HTTP %d", failure.Stage, failure.HTTPStatus)
					if failure.Stage == "purchase-store-2fa" {
						result.Message = "Apple Store 需要六位验证码"
					}
				}
				return result
			}
			purchase, err := buyer.SubmitStorageBuyDirect(queryCtx)
			if err != nil {
				result := control.Result{Message: "购买请求未完成", Stage: "purchase-buy"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("购买请求失败：%s，HTTP %d", failure.Stage, failure.HTTPStatus)
				}
				return result
			}
			if purchase == nil {
				return control.Result{Message: "Apple 未返回购买状态", Stage: "purchase-buy"}
			}
			if purchase.State == "completed" {
				return control.Result{OK: true, Purchase: purchase, Message: "Apple 已确认购买", Stage: "purchase-completed"}
			}
			return control.Result{OK: true, Purchase: purchase, Message: "Apple 返回后续步骤，订阅尚未完成", Stage: "purchase-handoff"}
		}
		if action == "storage-purchase" {
			queryCtx, done := context.WithTimeout(actionCtx, 75*time.Second)
			defer done()
			target := appleservices.PurchaseTarget{Bytes: 50 * 1024 * 1024 * 1024, PriceMinor: 600, Currency: "CNY", Period: "month"}
			// Default: the web-parity chain the official client runs (no
			// native Store session). The "store-session" code selects the
			// earlier SAP-login-first variant.
			var purchase *appleservices.PurchaseResult
			var err error
			if commandCode == "store-session" {
				buyer, ok := active.(interface {
					SubmitStoragePurchaseWithStoreSession(context.Context, appleservices.PurchaseTarget) (*appleservices.PurchaseResult, error)
				})
				if !ok {
					return control.Result{Message: "后台版本不支持 Store 会话购买变体", Stage: "worker"}
				}
				purchase, err = buyer.SubmitStoragePurchaseWithStoreSession(queryCtx, target)
			} else {
				buyer, ok := active.(interface {
					SubmitStoragePurchase(context.Context, appleservices.PurchaseTarget) (*appleservices.PurchaseResult, error)
				})
				if !ok {
					return control.Result{Message: "后台版本不支持 iCloud+ 购买路由，请更新后台", Stage: "worker"}
				}
				purchase, err = buyer.SubmitStoragePurchase(queryCtx, target)
			}
			if err != nil {
				result := control.Result{Message: "购买请求未完成，登录会话已保留", Stage: "purchase"}
				var failure *icloud.StorageError
				if errors.As(err, &failure) {
					result.Stage, result.HTTPStatus, result.AppleCode, result.AppleHint = failure.Stage, failure.HTTPStatus, failure.AppleCode, failure.Hint
					result.Message = fmt.Sprintf("购买请求失败：%s，HTTP %d。登录会话已保留", failure.Stage, failure.HTTPStatus)
				}
				return result
			}
			if purchase == nil {
				return control.Result{Message: "Apple 未返回购买状态", Stage: "purchase"}
			}
			if purchase.State == "completed" {
				return control.Result{OK: true, Purchase: purchase, Message: "iCloud+ 50GB 订阅已完成，付费容量已核对", Stage: "purchase-completed"}
			}
			if purchase.State == "existing-paid-plan" {
				return control.Result{OK: true, Purchase: purchase, Message: "账号已存在至少 50GB 的付费容量，未重复提交订阅", Stage: "purchase-existing"}
			}
			if purchase.State == "verification-pending" {
				return control.Result{OK: true, Purchase: purchase, Message: "Apple 返回完成页面，但付费容量尚未核对，请先读取容量", Stage: "purchase-verification-pending"}
			}
			return control.Result{OK: true, Purchase: purchase, Message: "Apple 返回后续认证或确认步骤，订阅尚未完成", Stage: "purchase-handoff"}
		}
		if action == "refresh" {
			return renew()
		}
		if action == "validate-account" {
			if err := active.BeginAccountValidation(); err != nil {
				status.State, status.VerificationRequired = "refresh_failed", true
				_ = status.save(*dir)
				return control.Result{Stage: "reconnect-required", Message: "当前是免密恢复会话；设备管理身份需要一次带密码重连，请重新连接此账号"}
			}
			if active.NeedsTwoFactor() {
				status.State, status.Authenticated, status.VerificationRequired = "awaiting_account_validation", false, true
				if err := status.save(*dir); err != nil {
					return control.Result{Message: "无法保存账户验证状态"}
				}
				return control.Result{OK: true, Message: "验证码已发送到受信任设备"}
			}
			client, err := active.Client()
			if err != nil || client == nil || client.ADSID != accountID {
				status.State, status.VerificationRequired = "refresh_failed", true
				_ = status.save(*dir)
				return control.Result{Message: "账户验证未完成，请稍后重试"}
			}
			if err := active.SaveSession(); err != nil {
				return control.Result{Message: "账户验证已完成，但登录状态保存失败"}
			}
			status.State, status.Authenticated, status.VerificationRequired = "online", true, false
			status.LastRefreshAt = time.Now().UTC()
			_ = status.save(*dir)
			return control.Result{OK: true, Message: "Apple 账户验证已完成"}
		}
		if action == "submit-account-code" {
			if status.State != "awaiting_account_validation" {
				return control.Result{Message: "当前没有待完成的账户验证"}
			}
			if err := active.SubmitCode(appleservices.TrustedDevice, commandCode); err != nil {
				return control.Result{Message: "验证码无效或已过期，请重新输入"}
			}
			client, err := active.Client()
			if err != nil || client == nil || client.ADSID != accountID {
				return control.Result{Message: "验证码已通过，但登录会话未建立"}
			}
			if err := active.SaveSession(); err != nil {
				return control.Result{Message: "验证已完成，但登录状态保存失败"}
			}
			status.State, status.Authenticated, status.VerificationRequired = "online", true, false
			status.LastRefreshAt = time.Now().UTC()
			if err := status.save(*dir); err != nil {
				return control.Result{Message: "验证已完成，但状态保存失败"}
			}
			return control.Result{OK: true, Message: "Apple 账户验证已完成"}
		}
		if action == "devices" {
			devices, err := active.AccountDevices()
			if err != nil {
				return deviceListFailure(err)
			}
			status.setDevices(devices, *name)
			if panel != nil {
				panel.SetDevices(panelDevices(devices, *name))
			}
			if active.SaveSession() != nil || status.save(*dir) != nil {
				return control.Result{Message: "无法保存登录状态"}
			}
			return control.Result{OK: true, Message: fmt.Sprintf("已更新 %d 台设备", len(devices))}
		}
		if action != "code" && action != "allow" && action != "deny" {
			return control.Result{Message: "当前无需短信验证码"}
		}
		if *listenOnly {
			return control.Result{Message: "观察模式不能生成验证码或批准登录"}
		}
		pendingMu.Lock()
		transaction := pending
		kind := pendingKind
		pendingMu.Unlock()
		if action != "code" && (transaction == "" || (visibleRequestID != "" && visibleRequestID != requestID(transaction))) {
			return control.Result{Message: "登录请求已取消或发生变化"}
		}
		code := ""
		if action == "allow" && kind == "circle" {
			code, err := circles.approve(transaction, sendCircle)
			if err != nil {
				return control.Result{Message: "配对响应未完成，请重试当前请求"}
			}
			pendingMu.Lock()
			if pending != transaction {
				pendingMu.Unlock()
				circles.cancel(transaction)
				return control.Result{Message: "配对请求已取消或更换"}
			}
			if pending == transaction {
				pending = ""
				pendingKind = ""
				if panel != nil {
					panel.SetPending(nil)
				}
			}
			pendingMu.Unlock()
			status.PairingState = "awaiting_client_proof"
			if err := status.save(*dir); err != nil {
				return control.Result{Message: "无法保存配对进度"}
			}
			return control.Result{OK: true, Code: code, CodeKind: "circle", HideAt: time.Now().Add(time.Minute).UTC(), Message: "请在发起端输入此配对验证码"}
		}
		if action != "deny" {
			generationCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			code, err = generateCode(generationCtx)
			stop()
			if err != nil {
				return control.Result{Message: "本机验证码生成失败"}
			}
		}
		if action != "code" {
			pendingMu.Lock()
			same := pending == transaction
			pendingMu.Unlock()
			if !same {
				return control.Result{Message: "登录请求已取消或发生变化"}
			}
			if active.RespondToSignIn(transaction, action == "allow") != nil {
				return control.Result{Message: "Apple 未接受本次登录响应"}
			}
			if action == "deny" {
				circles.cancel(transaction)
			}
			pendingMu.Lock()
			if pending == transaction {
				pending = ""
				pendingKind = ""
				if panel != nil {
					panel.SetPending(nil)
				}
			}
			pendingMu.Unlock()
		}
		return control.Result{OK: true, Code: code, HideAt: time.Now().Add(time.Minute).UTC(), Message: "操作已完成"}
	}
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) || (*listenFor > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
				return nil
			}
			return ctx.Err()
		case err := <-errorsCh:
			if ctx.Err() != nil {
				return nil
			}
			push.Close()
			status.PushConnected, status.PushIssue = false, "reconnecting"
			pendingMu.Lock()
			pending = ""
			pendingKind = ""
			circles = newCircleManager(accountID, client.DSID)
			if status.PairingState != "" {
				status.PairingState = "interrupted"
			}
			if panel != nil {
				panel.SetPending(nil)
			}
			pendingMu.Unlock()
			if saveErr := status.save(*dir); saveErr != nil {
				return saveErr
			}
			fmt.Fprintln(os.Stderr, "Push connection ended; retaining the authenticated session and retrying:", err)
			scheduleReconnect()
		case <-reconnectTick:
			reconnectTick = nil
			saved := push.State()
			go func() {
				candidate, err := reconnectPush(ctx, saved.Token, func() (pushTransport, error) {
					if *native {
						return aps.NewNativeClient()
					}
					return aps.NewClient(saved)
				})
				select {
				case reconnected <- pushConnection{client: candidate, err: err}:
				case <-ctx.Done():
					if candidate != nil {
						candidate.Close()
					}
				}
			}()
		case result := <-reconnected:
			if result.err != nil {
				if errors.Is(result.err, errPushTokenChanged) {
					status.PushIssue = "token_changed"
				} else {
					scheduleReconnect()
				}
			} else {
				push = result.client
				status.PushConnected, status.PushIssue = true, ""
				status.PushReconnectCount++
				reconnectDelay = 2 * time.Second
				listen(push)
				fmt.Fprintln(os.Stderr, "Push connection restored with the existing token; account session retained.")
			}
			if err := status.save(*dir); err != nil {
				return err
			}
		case command := <-panelCommands:
			if command.Context.Err() != nil {
				continue
			}
			if (command.Action.Action == "allow" || command.Action.Action == "deny") && command.RequestID == "" {
				command.Reply <- control.Result{Message: "请重新查看当前登录请求"}
				continue
			}
			command.Reply <- performAction(command.Context, command.Action.Action, command.RequestID, command.Code, command.Label, command.Note)
		case <-refresh.C:
			result := renew()
			if status.VerificationRequired {
				return errVerificationRequired
			}
			if result.OK {
				fmt.Printf("Session renewed. Device query succeeded: count=%d, installation_visible=%t.\n", status.DeviceCount, status.InstallationVisible)
			} else {
				fmt.Fprintln(os.Stderr, "Session renewal failed; retaining the existing connection.")
			}
		case event := <-events:
			if event.Unsupported {
				status.UnsupportedNoticeCount++
				status.LastUnsupportedNoticeAt = time.Now().UTC()
				status.PushIssue = "unsupported_idms_notification"
				_ = status.save(*dir)
				fmt.Fprintf(os.Stderr, "An unsupported IDMS notification arrived (size=%d); payload was discarded and the APS connection was kept alive.\n", event.NoticeSize)
				continue
			}
			if event.AccountID != "" && event.AccountID != accountID {
				continue
			}
			pendingMu.Lock()
			if event.Command == 100 {
				preserveCircle := pending == event.TransactionID && pendingKind == "circle"
				if pending != event.TransactionID {
					pendingKind = ""
				}
				pending = event.TransactionID
				if panel != nil && !preserveCircle {
					panel.SetPending(&control.Pending{ID: requestID(pending), Kind: pendingKind, ReceivedAt: time.Now().UTC()})
				}
			}
			if event.Command == 400 && pending == event.PreviousTransactionID {
				pending = ""
				pendingKind = ""
				if panel != nil {
					panel.SetPending(nil)
				}
			}
			pendingMu.Unlock()
			if event.Command == 700 {
				if event.CircleStep == 1 {
					if err := circles.remember(event); err != nil {
						status.PairingState = "request_rejected"
						fmt.Fprintln(os.Stderr, "Circle initial request rejected; no approval or code exchange performed.")
					} else if circles.pending(event.TransactionID) != nil {
						pendingMu.Lock()
						pending = event.TransactionID
						pendingKind = "circle"
						if panel != nil {
							panel.SetPending(&control.Pending{ID: requestID(pending), Kind: "circle", PairingID: requestID(event.CircleTransactionID), ReceivedAt: time.Now().UTC()})
						}
						pendingMu.Unlock()
						status.PairingState = "awaiting_approval"
					}
				} else {
					phase, err := circles.handle(event, sendCircle, freshCircleCode)
					if err != nil {
						status.PairingState = "exchange_failed"
						fmt.Fprintln(os.Stderr, "Circle exchange failed; account session retained.")
					} else {
						status.PairingState = phase
						fmt.Fprintln(os.Stderr, "Circle progress:", phase)
					}
				}
				status.PairingUnsupported = false
				if err := status.save(*dir); err != nil {
					return err
				}
			}
			if event.Command == 1200 {
				fmt.Fprintln(os.Stderr, "Apple delivered a device-status notification for this account. This does not verify code generation.")
			}
			if event.Command == 100 {
				if *background {
					fmt.Fprintln(os.Stderr, "An AuthKit sign-in request arrived for this account; no automatic approval in background mode.")
				} else if *listenOnly {
					fmt.Fprintln(os.Stderr, "An AuthKit sign-in request arrived for this account. Code generation is unavailable in observation mode.")
				} else {
					fmt.Fprintln(os.Stderr, "A sign-in request arrived for this account. Type allow to display a code, deny to reject it, or press Enter to ignore.")
				}
			}
			if event.Command == 400 {
				circles.cancel(event.PreviousTransactionID)
				fmt.Fprintln(os.Stderr, "Apple cancelled a sign-in request.")
			}
		case choice, ok := <-choices:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				choices = nil
				fmt.Fprintln(os.Stderr, "Sign-in input closed; session and reception remain active.")
				continue
			}
			if choice != "devices" && choice != "refresh" && choice != "code" && choice != "allow" && choice != "deny" {
				continue
			}
			result := performAction(ctx, choice, "", "", "", "")
			if result.Code != "" {
				fmt.Println("Apple verification code:", result.Code)
			} else {
				fmt.Fprintln(os.Stderr, result.Message)
			}
		}
	}
}

func lastNonEmptyLine(value string) string {
	lines := strings.Split(value, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	return "家庭工具未返回状态"
}

const familyStatusPrefix = "FAMILY_STATUS_JSON="

func runFamilyHelper(ctx context.Context, helper string, args, extraEnv []string) (string, error) {
	command := exec.CommandContext(ctx, helper, args...)
	command.Env = append(os.Environ(), extraEnv...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	for index := range extraEnv {
		extraEnv[index] = ""
	}
	return output.String(), err
}

func parseFamilyStatus(output string) (*control.FamilyInfo, error) {
	lines := strings.Split(output, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, familyStatusPrefix) {
			continue
		}
		var status control.FamilyInfo
		if json.Unmarshal([]byte(strings.TrimPrefix(line, familyStatusPrefix)), &status) != nil {
			return nil, errors.New("invalid redacted family status")
		}
		return &status, nil
	}
	return nil, errors.New("redacted family status missing")
}

func codeRequest(device *appleservices.Device) aps.CodeRequest {
	return aps.CodeRequest{DeviceID: (anisette.State{Identifier: device.Identifier}).DeviceID(), Identifier: device.Identifier, ProvisioningBlob: device.ProvisioningBlob}
}

func savePush(path string, state aps.State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return errors.New("could not save sensitive push state")
	}
	return nil
}

// loadProbeSpecs reads an optional diagnostic probe list from
// <dir>/probe-urls.json. It is a local experiment aid: when the file exists
// the storage-probe action probes those HTTPS endpoints instead of the
// built-in list, so probes can iterate without restarting the worker (each
// restart costs one Apple login). Anything malformed disables the override.
func loadProbeSpecs(dir string) []icloud.ProbeSpec {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "probe-urls.json"))
	if err != nil || len(data) == 0 || len(data) > 64*1024 {
		return nil
	}
	var specs []icloud.ProbeSpec
	if json.Unmarshal(data, &specs) != nil || len(specs) == 0 || len(specs) > 40 {
		return nil
	}
	for _, spec := range specs {
		if !strings.HasPrefix(spec.URL, "https://") {
			return nil
		}
	}
	return specs
}
