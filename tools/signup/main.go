// Command signup uses one persisted private session across registration stages.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
	"github.com/Laky-64/appleservices/internal/pacing"
)

type state struct {
	Version          int                  `json:"version"`
	Email            string               `json:"email"`
	Password         string               `json:"password,omitempty"`
	FirstName        string               `json:"firstName,omitempty"`
	LastName         string               `json:"lastName,omitempty"`
	Birthday         string               `json:"birthday,omitempty"`
	Country          string               `json:"country,omitempty"`
	Language         string               `json:"language,omitempty"`
	Phone            string               `json:"phone,omitempty"`
	PhoneCC          string               `json:"phoneCC,omitempty"`
	Stage            string               `json:"stage"`
	Status           string               `json:"status"`
	Failure          string               `json:"failure,omitempty"`
	HTTPStatus       int                  `json:"httpStatus,omitempty"`
	ErrorCodes       []string             `json:"errorCodes,omitempty"`
	Created          bool                 `json:"created"`
	EmailDone        bool                 `json:"emailVerified"`
	PhoneDone        bool                 `json:"phoneVerified"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	Session          *sessionState        `json:"session,omitempty"`
	Operations       map[string]operation `json:"operations,omitempty"`
	Body             map[string]any       `json:"body,omitempty"`
	Captcha          *captcha             `json:"captcha,omitempty"`
	CaptchaSource    string               `json:"captchaSource,omitempty"`
	VerificationID   json.RawMessage      `json:"verificationId,omitempty"`
	EmailRequestedAt time.Time            `json:"emailRequestedAt,omitempty"`
	PhoneRequestedAt time.Time            `json:"phoneRequestedAt,omitempty"`
	Profile          *fingerprint.Profile `json:"profile,omitempty"`
	ProfileMode      string               `json:"profileMode,omitempty"`
	Human            *humanConfig         `json:"human,omitempty"`
	Telemetry        map[string]any       `json:"telemetry,omitempty"`
	TelemetrySent    bool                 `json:"telemetrySent,omitempty"`
	Warmup           map[string]any       `json:"warmup,omitempty"`
}

func main() { os.Exit(runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

func runCLI(ctx context.Context, args []string, out, diagnostic io.Writer) int {
	fs := flag.NewFlagSet("signup", flag.ContinueOnError)
	fs.SetOutput(diagnostic)
	email := fs.String("email", "", "account email (required)")
	password := fs.String("password", os.Getenv("APPLESERVICES_SIGNUP_PASSWORD"), "password; preferably supplied through environment")
	first := fs.String("first", "James", "given name")
	last := fs.String("last", "Carter", "family name")
	birthday := fs.String("birthday", "1992-07-18", "birthday")
	country := fs.String("country", "USA", "account ISO3 country")
	language := fs.String("language", "en_US", "account language")
	phone := fs.String("phone", "", "national or international phone number")
	phoneCC := fs.String("phonecc", "US", "phone ISO2 country")
	stage := fs.String("stage", "preflight", "preflight|captcha|create|email|phone|finalize|verify|all")
	code := fs.String("code", "", "current verification code")
	statePath := fs.String("state", "", "private state path")
	base := fs.String("base", "", "loopback server override for offline testing")
	proxy := fs.String("proxy", "", "proxy; frozen with the session")
	timezone := fs.String("tz", "America/New_York", "timezone")
	locale := fs.String("locale", "en-US", "locale")
	ua := fs.String("user-agent", "", "User-Agent")
	key := fs.String("captcha-key", os.Getenv("YESCAPTCHA_KEY"), "captcha service key; prefer environment")
	capImage := fs.String("captcha-dump", "", "private captcha image path")
	answer := fs.String("captcha-answer", "", "answer for the saved challenge")
	answerFile := fs.String("captcha-answer-file", "", "manual challenge answer file")
	capWait := fs.Duration("captcha-wait", 180*time.Second, "captcha answer deadline")
	ch := fs.Bool("ch-hints", true, "send consistent Windows Chrome client hints")
	skip := fs.Bool("skip-landing", false, "skip landing page")
	curl := fs.Bool("curl", false, "use system curl; no automatic fallback")
	h2 := fs.Bool("http2", false, "request HTTP/2 negotiation")
	fd := fs.String("fd-client-info", "", "FD JSON matching configured UA and locale")
	profileMode := fs.String("profile-mode", "legacy", "legacy|new fingerprint profile mode")
	profileKind := fs.String("profile", "auto", "auto|chrome|firefox|mac_safari|ios_safari")
	egressCountry := fs.String("egress-country", "", "verified egress country (ISO2); required in new mode")
	egressSource := fs.String("egress-source", "", "explicit|verified-egress; required in new mode")
	engineInsecure := fs.Bool("engine-insecure", false, "skip certificate verification (offline tests only)")
	humanMode := fs.Bool("human", false, "warm up like a browser and pace the stages")
	pacingMin := fs.Duration("pacing-min", 1500*time.Millisecond, "minimum wait before a request in -human mode")
	pacingMax := fs.Duration("pacing-max", 9000*time.Millisecond, "maximum wait before a request in -human mode")
	warmupAssets := fs.Int("warmup-assets", 2, "widget bundles to fetch during warm-up (-human only)")
	journey := fs.Bool("journey", true, "visit apple.com/support before the account page, with a click-through referer")
	telemetryPath := fs.String("telemetry", "", "replay captured jslog telemetry from this capture file")
	deviceID := fs.String("device", "", "persistent device profile id: reuse cookies and fingerprint across tasks")
	migrateProfile := fs.Bool("migrate-profile", false, "rewrite a legacy preset alias (HelloChrome_Auto …) to its pinned preset when the version matches")
	deviceDir := fs.String("device-dir", ".cache/devices", "directory for device records")
	asJSON := fs.Bool("json", false, "structured redacted receipt")
	timeout := fs.Duration("timeout", 45*time.Second, "request timeout")
	emailFile := fs.String("email-code-file", "", "private six-digit email code file")
	phoneFile := fs.String("phone-code-file", "", "private six-digit SMS code file")
	codeWait := fs.Duration("code-wait", 0, "wait for input files; default returns waiting_input")
	capabilities := fs.Bool("capabilities", false, "print offline protocol capabilities")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *capabilities {
		_ = json.NewEncoder(out).Encode(map[string]any{"stateVersion": signupStateVersion, "sessionResume": true, "finalize": true})
		return 0
	}
	fail := func(reason string) int {
		if *asJSON {
			_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "reason": reason})
		} else {
			fmt.Fprintln(diagnostic, reason)
		}
		return 2
	}
	if strings.TrimSpace(*email) == "" {
		return fail("email_required")
	}
	allowed := map[string]bool{"preflight": true, "captcha": true, "create": true, "email": true, "phone": true, "finalize": true, "verify": true, "all": true}
	if !allowed[*stage] || *timeout <= 0 || *codeWait < 0 {
		return fail("invalid_stage_or_timeout")
	}
	if *base != "" {
		r, err := http.NewRequest("GET", *base, nil)
		if err != nil || (r.URL.Hostname() != "127.0.0.1" && r.URL.Hostname() != "localhost" && r.URL.Hostname() != "::1") {
			return fail("base_override_must_be_loopback")
		}
	}
	if *statePath == "" {
		*statePath = filepath.Join(".cache", "signup", safeName(strings.ToLower(strings.TrimSpace(*email)))+".json")
	}
	unlock, err := lockState(*statePath)
	if err != nil {
		return fail("task_locked_requires_review")
	}
	defer unlock()
	st, err := loadState(*statePath)
	if err != nil {
		return fail("state_unreadable")
	}
	if st.Email != "" && (!strings.EqualFold(st.Email, strings.TrimSpace(*email)) || st.Version != signupStateVersion) {
		return fail("state_identity_or_version_mismatch")
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if st.Email == "" {
		st.Version = signupStateVersion
		st.Email = strings.TrimSpace(*email)
		st.Password = *password
		if st.Password == "" {
			st.Password = GeneratedPassword(14)
		}
		st.FirstName = *first
		st.LastName = *last
		st.Birthday = *birthday
		st.Country = *country
		st.Language = *language
		st.Phone = digitsOnly(*phone)
		st.PhoneCC = strings.ToUpper(*phoneCC)
	} else {
		values := map[string][2]string{"password": {st.Password, *password}, "first": {st.FirstName, *first}, "last": {st.LastName, *last}, "birthday": {st.Birthday, *birthday}, "country": {st.Country, *country}, "language": {st.Language, *language}, "phonecc": {st.PhoneCC, strings.ToUpper(*phoneCC)}}
		for name, pair := range values {
			if visited[name] && pair[0] != pair[1] {
				return fail("task_identity_change_rejected")
			}
		}
		if visited["phone"] && digitsOnly(*phone) != st.Phone {
			if (st.Stage != "preflight" && st.Stage != "" && st.Stage != "captcha") || st.Body != nil || st.Operations["validate"].Status != "" {
				return fail("task_phone_change_rejected")
			}
			st.Phone = digitsOnly(*phone)
		}
	}
	opts := SessionOptions{Base: *base, UserAgent: *ua, Locale: *locale, Timezone: *timezone, Proxy: *proxy, FDClientInfo: *fd, HTTP2: *h2, SkipLanding: *skip, CHHints: *ch, UseCurl: *curl, Timeout: *timeout}
	if st.Session != nil {
		saved := st.Session.Options
		pairs := map[string][2]any{"base": {saved.Base, opts.Base}, "proxy": {saved.Proxy, opts.Proxy}, "user-agent": {saved.UserAgent, opts.UserAgent}, "locale": {saved.Locale, opts.Locale}, "tz": {saved.Timezone, opts.Timezone}, "fd-client-info": {saved.FDClientInfo, opts.FDClientInfo}, "http2": {saved.HTTP2, opts.HTTP2}, "skip-landing": {saved.SkipLanding, opts.SkipLanding}, "ch-hints": {saved.CHHints, opts.CHHints}, "curl": {saved.UseCurl, opts.UseCurl}}
		for name, pair := range pairs {
			if visited[name] && pair[0] != pair[1] {
				return fail("session_profile_change_rejected")
			}
		}
		opts = saved
	} else if opts.Proxy == "" {
		endpoint := opts.Base
		if endpoint == "" {
			endpoint = defaultBase
		}
		r, _ := http.NewRequest("GET", endpoint, nil)
		if p, e := http.ProxyFromEnvironment(r); e != nil {
			return fail("proxy_configuration_invalid")
		} else if p != nil {
			opts.Proxy = p.String()
		} else {
			opts.Direct = true
		}
	}
	var session *Session
	if st.Session != nil && st.Session.Scnt != "" {
		session, err = restoreSession(st.Session)
	} else if st.Stage == "" {
		session, err = newSession(opts)
	} else {
		return fail("saved_session_missing")
	}
	if err != nil {
		return fail("session_configuration_invalid")
	}
	defer session.Close()
	// Device state: a browser that has been used. Loaded before the profile so a
	// known device keeps its fingerprint, and before warm-up so the visit starts
	// with cookies behind it.
	var device *deviceState
	var cookiesLoaded int
	var profileReused bool
	if strings.TrimSpace(*deviceID) != "" {
		device, err = loadDevice(*deviceDir, *deviceID, time.Now())
		if err != nil {
			return fail("device_state_invalid")
		}
		// Only a task without intent may borrow the device's profile. A submitted
		// task whose profile went missing must hit the block below, not be patched
		// back to health behind the reviewer's back.
		if device.Profile != nil && st.Profile == nil && !hasIntent(st) {
			st.Profile = device.Profile
			profileReused = true
		}
		// Device cookies only seed a *fresh* task. On resume the task snapshot is
		// newer than the device file, and mixing them would pair a fresh scnt with
		// stale cookies.
		if deviceSeedsTask(st) {
			cookiesLoaded, err = device.warmSession(session)
			if err != nil {
				return fail("device_cookie_restore_failed")
			}
		}
	}
	// Migration: a brand-new task with egress inputs explicitly given gets the
	// profile engine without anyone having to remember a flag. Existing tasks
	// (and runs that give no egress inputs) stay on the legacy path.
	effectiveMode := *profileMode
	// A task that was created under the profile engine keeps it. Without this a
	// resume that omits -profile-mode silently fell back to the legacy path and
	// the receipt still claimed the engine was applied.
	if st.ProfileMode != "" && !visited["profile-mode"] {
		effectiveMode = st.ProfileMode
	}
	// A saved profile is authoritative: never run a profiled task as legacy.
	if st.Profile != nil {
		effectiveMode = profileModeNew
	}
	if !visited["profile-mode"] && st.Profile == nil && st.ProfileMode == "" && st.Session == nil && st.Stage == "" &&
		strings.TrimSpace(*egressCountry) != "" && strings.TrimSpace(*egressSource) != "" {
		effectiveMode = profileModeNew
	}
	profile, engineInfo, profileErr := installProfile(st, session, profileConfig{
		Mode:          effectiveMode,
		Kind:          *profileKind,
		EgressCountry: *egressCountry,
		EgressSource:  *egressSource,
		Insecure:      *engineInsecure,
		MigrateLegacy: *migrateProfile,
	}, opts.Proxy, opts.Timeout)
	if profileErr != nil {
		fmt.Fprintln(diagnostic, profileErr)
		if errors.Is(profileErr, ErrProfileBlocked) {
			st.Status = "needs_review"
			st.Failure = "profile_blocked"
			_ = writeState(*statePath, st)
			return fail("profile_blocked")
		}
		return fail("profile_configuration_invalid")
	}
	if profile != nil {
		// Persist before the first network request: a crash between here and the
		// first call must not leave a task that later mints a second profile.
		if err := writeState(*statePath, st); err != nil {
			return fail("state_write_failed")
		}
	}
	if st.Body != nil {
		account, ok := st.Body["account"].(map[string]any)
		if !ok || account["name"] != st.Email {
			return fail("saved_body_binding_invalid")
		}
		if _, ok := st.Body["phoneNumberVerification"].(map[string]any); !ok {
			return fail("saved_phone_binding_invalid")
		}
	}
	if (*stage == "create" || *stage == "all") && st.Phone == "" {
		return fail("phone_required")
	}
	if localDialCode(st.PhoneCC) == "" {
		return fail("unsupported_phone_country")
	}
	if *humanMode || st.Human != nil {
		human, err := freezeHuman(st, *humanMode, *pacingMin, *pacingMax, *warmupAssets, *journey, session)
		if err != nil {
			return fail("human_configuration_invalid")
		}
		pacer := pacing.New(human.Pacing, nil)
		if st.Stage == "" || st.Warmup == nil {
			steps := session.runWarmup(ctx, pacer, human.Assets, *journey)
			st.Warmup = describeWarmup(steps, session.warmupCookieCount())
			// Only skip the bootstrap walk when the warm-up actually established a
			// usable session. A warm-up that came back with cookies but no session
			// id must still let Bootstrap do its own (minimal-header) walk.
			if session.scnt != "" && session.sessionID != "" {
				session.SkipLanding = true
			} else {
				session.scnt, session.sessionID = "", ""
			}
			if err := writeState(*statePath, st); err != nil {
				return fail("state_write_failed")
			}
		}
		// The pacer is attached below, once the workflow exists.
		_ = pacer
	}
	if device != nil {
		var paths []string
		if st.Warmup != nil {
			if steps, ok := st.Warmup["steps"].([]warmupStep); ok {
				for _, step := range steps {
					if step.Path != "" {
						paths = append(paths, step.Path)
					}
				}
			}
		}
		// Read-modify-write under one lock: a warming run and a registration run
		// can overlap, and two separate load+save pairs would drop one visit.
		updated, err := updateDevice(*deviceDir, *deviceID, time.Now(), func(d *deviceState) error {
			d.noteVisit(paths, time.Now())
			d.noteTask(st.Email)
			d.captureCookies(session)
			if st.Profile != nil {
				d.Profile = st.Profile
			}
			return nil
		})
		if err != nil {
			return fail("device_state_write_failed")
		}
		device = updated
	}
	w := &workflow{state: st, session: session, path: *statePath, options: opts, emailFile: *emailFile, phoneFile: *phoneFile, wait: *codeWait}
	if st.Human != nil {
		w.pacer = pacing.New(st.Human.Pacing, nil)
	}
	// Telemetry replay: real captured batches, re-stamped, sent once per task.
	if strings.TrimSpace(*telemetryPath) != "" && !st.TelemetrySent {
		capture, err := loadTelemetryCapture(*telemetryPath)
		if err != nil {
			return fail("telemetry_capture_invalid")
		}
		plan, err := planRebase(capture, time.Now())
		if err != nil {
			return fail("telemetry_plan_invalid")
		}
		var telemetryPacer *pacing.Pacer
		if st.Human != nil {
			telemetryPacer = pacing.New(st.Human.Pacing, nil)
		}
		// At-most-once: the intent is persisted before the first envelope leaves,
		// so a crash mid-replay cannot resend the batch on the next run. A failed
		// send is recorded, not retried.
		st.TelemetrySent = true
		if err := writeState(*statePath, st); err != nil {
			return fail("state_write_failed")
		}
		st.Telemetry = telemetrySummary(capture, session.sendTelemetry(ctx, plan, telemetryPacer))
		if err := writeState(*statePath, st); err != nil {
			return fail("state_write_failed")
		}
	}
	w.captchaAnswer = func(ctx context.Context, c *captcha) (string, error) {
		st.CaptchaSource = captchaSource(*answer, *key, *answerFile)
		if err := writeCaptchaImage(*capImage, c.image); err != nil {
			return "", err
		}
		return answerQuestion(ctx, c, *answer, *key, *capWait, *capImage, *answerFile)
	}
	err = w.run(ctx, *stage, *code)
	if err != nil && st.Failure == "" {
		st.Failure = publicReason(err)
	}
	if *stage == "captcha" && err == nil && st.Captcha != nil {
		err = writeCaptchaImage(*capImage, st.Captcha.image)
	}
	exit := 0
	reason := ""
	if err != nil {
		exit = 1
		reason = "stage_unconfirmed"
		if errors.Is(err, errWaiting) {
			exit = 4
			reason = "waiting_input"
		} else if st.Failure != "" {
			reason = st.Failure
		}
	}
	engine := "go-net-http"
	if session.UseCurl {
		engine = "system-curl"
	}
	if session.engineApplied {
		engine = "utls"
	}
	receipt := map[string]any{"ok": err == nil, "stage": st.Stage, "state": st.Status, "reason": reason, "accountExists": st.Created && st.Status == "ready", "created": st.Created, "emailVerified": st.EmailDone, "phoneVerified": st.PhoneDone, "transport": engine, "httpStatus": st.HTTPStatus, "http2Requested": session.HTTP2}
	receipt["errorCodes"] = st.ErrorCodes
	receipt["captchaSource"] = st.CaptchaSource
	receipt["profile"] = profileReceipt(st.Profile, engineInfo)
	if st.Warmup != nil {
		receipt["warmup"] = st.Warmup
	}
	if st.Telemetry != nil {
		receipt["telemetry"] = st.Telemetry
	}
	if device != nil {
		receipt["device"] = deviceReceipt(device, time.Now(), cookiesLoaded, profileReused)
	}
	if st.Human != nil {
		receipt["pacing"] = map[string]any{"enabled": st.Human.Pacing.Enabled, "minMs": st.Human.Pacing.Min.Milliseconds(), "maxMs": st.Human.Pacing.Max.Milliseconds(), "applied": w.PacerApplied()}
	}
	if err != nil {
		receipt["error"] = reason
	}
	if *asJSON {
		_ = json.NewEncoder(out).Encode(receipt)
	} else {
		fmt.Fprintf(out, "stage=%s state=%s created=%v reason=%s\n", st.Stage, st.Status, st.Created, reason)
	}
	return exit
}

// publicReasons is the complete set of failure codes a receipt may carry.
// Anything else is reported as stage_unconfirmed: a provider body or an internal
// message must never reach a public receipt.
var publicReasons = map[string]bool{
	"bootstrap_unconfirmed":             true,
	"bootstrap_session_missing":         true,
	"preflight_unavailable":             true,
	"mailbox_in_use":                    true,
	"mailbox_rejected":                  true,
	"password_policy_rejected":          true,
	"captcha_answer_required":           true,
	"captcha_request_failed":            true,
	"captcha_out_of_order":              true,
	"validate_out_of_order":             true,
	"email_out_of_order":                true,
	"phone_out_of_order":                true,
	"finalize_out_of_order":             true,
	"verify_out_of_order":               true,
	"verification_code_required":        true,
	"verification_id_missing":           true,
	"verification_id_empty":             true,
	"verification_id_invalid":           true,
	"final_account_binding_unconfirmed": true,
	"final_creation_receipt_required":   true,
	"account_readback_unconfirmed":      true,
	"phone_required":                    true,
	"unsupported_phone_country":         true,
	"state_write_failed_after_request":  true,
	"unsupported_stage":                 true,
	"profile_blocked":                   true,
}

// publicReason maps an internal error to a stable receipt code.
func publicReason(err error) string {
	message := err.Error()
	if publicReasons[message] {
		return message
	}
	if strings.HasPrefix(message, "profile_blocked") || errors.Is(err, ErrProfileBlocked) {
		return "profile_blocked"
	}
	return "stage_unconfirmed"
}

func accountFrom(st *state) Account {
	return Account{
		Email:       st.Email,
		Password:    st.Password,
		FirstName:   st.FirstName,
		LastName:    st.LastName,
		Birthday:    st.Birthday,
		CountryISO3: st.Country,
		Language:    st.Language,
	}
}

func phoneFrom(st *state) Phone {
	return Phone{
		Number:       formatPhone(st.Phone, st.PhoneCC),
		CountryCode2: st.PhoneCC,
		DialCode:     localDialCode(st.PhoneCC),
	}
}

func loadState(path string) (*state, error) {
	st := &state{}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	return st, nil
}

// emit prints a protocol failure as the structured verdict it already is.
func emit(err error, asJSON bool, receipt map[string]any) {
	if asJSON {
		receipt["error"] = err.Error()
		line, _ := json.Marshal(receipt)
		fmt.Println(string(line))
		return
	}
	fmt.Fprintln(os.Stderr, "signup:", err)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatPhone mirrors the widget's display format for a US-style number; other
// countries are sent bare. SMS relays hand out numbers with or without the
// country code (hero returns +1 585 309 9954 as "15853099954") and Apple
// answers `ExceedsLength / Invalid Phone Number` when the dial code lands in
// the national slot, so it is stripped first.
func formatPhone(number, iso2 string) string {
	iso2 = strings.ToUpper(iso2)
	dial := localDialCode(iso2)
	if dial != "" && strings.HasPrefix(number, dial) && len(number) > len(dial) {
		if national := number[len(dial):]; len(national) == 10 || (iso2 != "US" && len(number) > 11) {
			number = national
		}
	}
	if iso2 == "US" && len(number) == 10 {
		return fmt.Sprintf("(%s) %s-%s", number[:3], number[3:6], number[6:])
	}
	return number
}

func safeName(s string) string {
	replacer := strings.NewReplacer("@", "_at_", ":", "_", "/", "_", "\\", "_", " ", "_")
	return replacer.Replace(s)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "signup: "+format+"\n", args...)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "signup: "+format+"\n", args...)
	os.Exit(1)
}
