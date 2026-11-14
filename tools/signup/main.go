// Command signup creates an Apple Account over plain HTTP.
//
// It is the protocol-only replacement for the browser driver in
// tools/capture/create-account.cjs: same endpoints, same bodies, no page load.
// Stages are resumable through a state file so a human (or the SMS relay) can
// step in with a code, then re-run.
//
//	signup -email a@b.c -password ... -stage all          # up to the email code
//	signup -email a@b.c -stage email -code 123456         # submit it
//	signup -email a@b.c -stage phone -phone 4155550134 -phonecc US -code 654321
//
// Nothing is written on Apple's side before -stage create; -stage preflight and
// -stage captcha are read-only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// state is the resumable job record. It holds no tokens and no session
// material: cookies live in memory for one run, by design.
type state struct {
	Email     string    `json:"email"`
	Password  string    `json:"password,omitempty"`
	FirstName string    `json:"firstName,omitempty"`
	LastName  string    `json:"lastName,omitempty"`
	Birthday  string    `json:"birthday,omitempty"`
	Country   string    `json:"country,omitempty"`
	Language  string    `json:"language,omitempty"`
	Phone     string    `json:"phone,omitempty"`
	PhoneCC   string    `json:"phoneCC,omitempty"`
	Stage     string    `json:"stage"`
	Created   bool      `json:"created"`
	EmailDone bool      `json:"emailVerified"`
	PhoneDone bool      `json:"phoneVerified"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func main() {
	var (
		email      = flag.String("email", "", "Apple ID address to create (required)")
		password   = flag.String("password", "", "password; generated when empty")
		first      = flag.String("first", "James", "given name")
		last       = flag.String("last", "Carter", "family name")
		birthday   = flag.String("birthday", "1992-07-18", "birthday, yyyy-mm-dd")
		country    = flag.String("country", "USA", "ISO3 country of the account")
		language   = flag.String("language", "en_US", "preferredLanguage")
		phone      = flag.String("phone", "", "phone number, digits only (required for -stage create)")
		phoneCC    = flag.String("phonecc", "US", "ISO2 dialing country of the phone number")
		stage      = flag.String("stage", "preflight", "preflight|captcha|create|email|phone|verify|all")
		code       = flag.String("code", "", "code to submit for the current email/phone stage")
		statePath  = flag.String("state", "", "state file (default .cache/signup/<email>.json)")
		base       = flag.String("base", "", "override API host, for the offline tests")
		proxy      = flag.String("proxy", "", "explicit HTTP proxy; default honours HTTP(S)_PROXY")
		timezone   = flag.String("tz", "America/New_York", "browser timezone reported in X-Apple-I-Timezone")
		locale     = flag.String("locale", "en-US", "browser locale")
		userAgent  = flag.String("user-agent", "", "override User-Agent")
		captchaKey = flag.String("captcha-key", os.Getenv("YESCAPTCHA_KEY"), "YesCaptcha key; empty means manual")
		captchaPNG = flag.String("captcha-dump", "", "write the captcha image here so a human can read it")
		captchaAns = flag.String("captcha-answer", "", "answer to use instead of asking YesCaptcha")
		asJSON     = flag.Bool("json", false, "print the stage receipt as one JSON line")
		timeout    = flag.Duration("timeout", 45*time.Second, "per request timeout")
	)
	flag.Parse()

	if strings.TrimSpace(*email) == "" {
		fatalf("-email is required")
	}
	if *statePath == "" {
		*statePath = filepath.Join(".cache", "signup", safeName(*email)+".json")
	}
	st, err := loadState(*statePath)
	if err != nil {
		fatalf("state: %v", err)
	}
	if st.Email == "" {
		st.Email = strings.TrimSpace(*email)
	}
	if st.Password == "" {
		st.Password = *password
		if st.Password == "" {
			st.Password = GeneratedPassword(14)
		}
	}
	st.FirstName = orDefault(st.FirstName, *first)
	st.LastName = orDefault(st.LastName, *last)
	st.Birthday = orDefault(st.Birthday, *birthday)
	st.Country = orDefault(st.Country, *country)
	st.Language = orDefault(st.Language, *language)
	if *phone != "" {
		st.Phone = digitsOnly(*phone)
	}
	if st.PhoneCC == "" {
		st.PhoneCC = strings.ToUpper(*phoneCC)
	}

	ctx := context.Background()
	session, err := newSession(SessionOptions{
		Base:      *base,
		UserAgent: *userAgent,
		Locale:    *locale,
		Timezone:  *timezone,
		Proxy:     *proxy,
		Timeout:   *timeout,
	})
	if err != nil {
		fatalf("%v", err)
	}

	receipt := map[string]any{"stage": *stage, "email": st.Email}

	switch *stage {
	case "preflight":
		fail := runBootstrap(ctx, session, st)
		if fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		free, used, err := session.EmailFree(ctx, st.Email)
		if err != nil {
			fatalf("%v", err)
		}
		receipt["free"], receipt["used"] = free, used
		if !free {
			fatalf("preflight: address is not usable (valid=false or used=true)")
		}

	case "captcha":
		fail := runBootstrap(ctx, session, st)
		if fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		c, err := session.RequestCaptcha(ctx)
		if err != nil {
			fatalf("%v", err)
		}
		if err := writeCaptchaImage(*captchaPNG, c.image); err != nil {
			fatalf("captcha: %v", err)
		}
		receipt["captchaId"], receipt["imageBytes"] = c.ID, len(c.image)
		if *captchaPNG != "" {
			receipt["image"] = *captchaPNG
		}

	case "create":
		if st.Phone == "" {
			fatalf("-phone is required for -stage create")
		}
		if fail := runBootstrap(ctx, session, st); fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		if err := session.CheckPassword(ctx, st.Email, st.Password, st.FirstName, st.LastName); err != nil {
			warnf("password precheck: %v", err)
		}
		c, err := session.RequestCaptcha(ctx)
		if err != nil {
			fatalf("%v", err)
		}
		if err := writeCaptchaImage(*captchaPNG, c.image); err != nil {
			fatalf("captcha: %v", err)
		}
		answer, err := answerQuestion(ctx, c, *captchaAns, *captchaKey, 90*time.Second)
		if err != nil {
			fatalf("%v", err)
		}
		body, err := session.Create(ctx, accountFrom(st), phoneFrom(st), c, answer)
		if err != nil {
			emit(err, *asJSON, receipt)
			os.Exit(1)
		}
		st.Created, st.Stage = true, "created"
		saveState(*statePath, st)
		receipt["answered"] = true
		_ = body

	case "email":
		if *code == "" {
			if fail := runBootstrap(ctx, session, st); fail != nil {
				emit(fail, *asJSON, receipt)
				os.Exit(1)
			}
			if _, err := session.SendEmailCode(ctx, st.Email); err != nil {
				emit(err, *asJSON, receipt)
				os.Exit(1)
			}
			receipt["sent"] = true
			st.Stage = "email-sent"
			saveState(*statePath, st)
			break
		}
		if fail := runBootstrap(ctx, session, st); fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		if _, err := session.SubmitEmailCode(ctx, st.Email, digitsOnly(*code)); err != nil {
			emit(err, *asJSON, receipt)
			os.Exit(1)
		}
		st.EmailDone, st.Stage = true, "email-verified"
		saveState(*statePath, st)
		receipt["verified"] = true

	case "phone":
		if st.Phone == "" {
			fatalf("-phone is required for -stage phone")
		}
		if fail := runBootstrap(ctx, session, st); fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		if *code == "" {
			if _, err := session.SendPhoneCode(ctx, phoneFrom(st)); err != nil {
				emit(err, *asJSON, receipt)
				os.Exit(1)
			}
			st.Stage = "phone-sent"
			saveState(*statePath, st)
			receipt["sent"] = true
			break
		}
		if _, err := session.SubmitPhoneCode(ctx, phoneFrom(st), digitsOnly(*code)); err != nil {
			emit(err, *asJSON, receipt)
			os.Exit(1)
		}
		st.PhoneDone, st.Stage = true, "done"
		saveState(*statePath, st)
		receipt["verified"] = true

	case "verify":
		if fail := runBootstrap(ctx, session, st); fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		free, used, err := session.EmailFree(ctx, st.Email)
		if err != nil {
			fatalf("%v", err)
		}
		receipt["free"], receipt["used"], receipt["accountExists"] = free, used, used
		if !used {
			fatalf("verify: address is not taken yet, the create stage did not land")
		}

	case "all":
		if fail := runBootstrap(ctx, session, st); fail != nil {
			emit(fail, *asJSON, receipt)
			os.Exit(1)
		}
		free, used, err := session.EmailFree(ctx, st.Email)
		if err != nil || !free {
			fatalf("preflight: free=%v used=%v err=%v", free, used, err)
		}
		if st.Phone == "" {
			fatalf("-phone is required for -stage all")
		}
		if err := session.CheckPassword(ctx, st.Email, st.Password, st.FirstName, st.LastName); err != nil {
			warnf("password precheck: %v", err)
		}
		c, err := session.RequestCaptcha(ctx)
		if err != nil {
			fatalf("%v", err)
		}
		if err := writeCaptchaImage(*captchaPNG, c.image); err != nil {
			fatalf("captcha: %v", err)
		}
		answer, err := answerQuestion(ctx, c, *captchaAns, *captchaKey, 90*time.Second)
		if err != nil {
			fatalf("%v", err)
		}
		if _, err := session.Create(ctx, accountFrom(st), phoneFrom(st), c, answer); err != nil {
			emit(err, *asJSON, receipt)
			os.Exit(1)
		}
		st.Created, st.Stage = true, "created"
		saveState(*statePath, st)
		receipt["created"] = true

	default:
		fatalf("unknown -stage %q", *stage)
	}

	saveState(*statePath, st)
	if *asJSON {
		line, _ := json.Marshal(receipt)
		fmt.Println(string(line))
		return
	}
	fmt.Printf("stage=%s email=%s state=%s\n", *stage, st.Email, *statePath)
	if st.Created {
		fmt.Printf("created=%v emailVerified=%v phoneVerified=%v\n", st.Created, st.EmailDone, st.PhoneDone)
	}
}

// runBootstrap keeps the five stages from repeating the same three lines.
func runBootstrap(ctx context.Context, s *Session, st *state) error {
	if err := s.Bootstrap(ctx, st.Country); err != nil {
		return err
	}
	return nil
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

func saveState(path string, st *state) {
	st.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		warnf("state: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		warnf("state: %v", err)
		return
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		warnf("state: %v", err)
	}
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
// countries are sent bare.
func formatPhone(number, iso2 string) string {
	if strings.ToUpper(iso2) == "US" && len(number) == 10 {
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
