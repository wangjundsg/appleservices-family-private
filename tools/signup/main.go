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
	VerificationID   json.RawMessage      `json:"verificationId,omitempty"`
	EmailRequestedAt time.Time            `json:"emailRequestedAt,omitempty"`
	PhoneRequestedAt time.Time            `json:"phoneRequestedAt,omitempty"`
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
	w := &workflow{state: st, session: session, path: *statePath, options: opts, emailFile: *emailFile, phoneFile: *phoneFile, wait: *codeWait}
	w.captchaAnswer = func(ctx context.Context, c *captcha) (string, error) {
		if err := writeCaptchaImage(*capImage, c.image); err != nil {
			return "", err
		}
		return answerQuestion(ctx, c, *answer, *key, *capWait, *capImage, *answerFile)
	}
	err = w.run(ctx, *stage, *code)
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
	receipt := map[string]any{"ok": err == nil, "stage": st.Stage, "state": st.Status, "reason": reason, "accountExists": st.Created && st.Status == "ready", "created": st.Created, "emailVerified": st.EmailDone, "phoneVerified": st.PhoneDone, "transport": engine, "httpStatus": st.HTTPStatus, "http2Requested": session.HTTP2}
	receipt["errorCodes"] = st.ErrorCodes
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
