package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeApple is the smallest server that behaves like the account widget's API:
// it rotates scnt on every response, demands a valid hashcash stamp on the two
// protected paths, and tracks the created account so the preflight can answer
// used=true afterwards.
type fakeApple struct {
	mu          sync.Mutex
	counter     int
	scnt        string
	session     string
	hcBits      int
	hcChallenge string
	created     bool
	createdBody map[string]any
	emailCode   string
	phoneCode   string
	seen        []string
}

func newFakeApple() *fakeApple {
	return &fakeApple{scnt: "AAAA-seed|1", session: "SESSION-0", hcBits: 10, hcChallenge: "abc123"}
}

func (f *fakeApple) rotate(w http.ResponseWriter) {
	f.mu.Lock()
	f.counter++
	f.scnt = fmt.Sprintf("AAAA-seed|%d", f.counter)
	f.hcChallenge = fmt.Sprintf("challenge%d", f.counter)
	scnt := f.scnt
	session := f.session
	challenge := f.hcChallenge
	bits := f.hcBits
	f.mu.Unlock()
	w.Header().Set("scnt", scnt)
	w.Header().Set("X-Apple-ID-Session-Id", session)
	w.Header().Set("X-Apple-HC-Bits", fmt.Sprint(bits))
	w.Header().Set("X-Apple-HC-Challenge", challenge)
}

func (f *fakeApple) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/bootstrap/portal", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"serviceKey":"widget-key-from-portal","apiUrl":"https://appleid.apple.com"}`)
	})

	mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "countryCode=USA" {
			f.rotate(w)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"locale":"en_US","landingPageMode":"create"}`)
			return
		}
		f.rotate(w)
		fmt.Fprint(w, "<html>portal</html>")
	})

	mux.HandleFunc("/account/validation/appleid", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("scnt"); got == "" {
			t.Errorf("preflight sent no scnt")
		}
		f.rotate(w)
		fmt.Fprintf(w, `{"valid":true,"used":%v,"appleOwnedDomain":false,"isRecycledDomain":false}`, f.created)
	})

	mux.HandleFunc("/account/validate/password", func(w http.ResponseWriter, r *http.Request) {
		f.rotate(w)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/captcha", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["type"] != "IMAGE" {
			t.Errorf("captcha type = %q, want IMAGE", body["type"])
		}
		f.rotate(w)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":77,"token":"tok-77","type":"IMAGE","payload":{"contentType":"image/jpeg","content":%q}}`,
			base64.StdEncoding.EncodeToString([]byte("fakejpeg")))
	})

	mux.HandleFunc("/account/validate", func(w http.ResponseWriter, r *http.Request) {
		stamp := r.Header.Get("X-Apple-HC")
		f.mu.Lock()
		bits := f.hcBits
		f.mu.Unlock()
		if stamp == "" || !hashcashSatisfies(stamp, bits) {
			f.rotate(w)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"validationErrors":[{"code":"hashcash.Invalid"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.createdBody = body
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		captcha, _ := body["captcha"].(map[string]any)
		if captcha["answer"] != "ABCDE" || captcha["token"] != "tok-77" {
			f.rotate(w)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"validationErrors":[{"code":"captchaAnswer.Invalid"}]}`)
			return
		}
		f.rotate(w)
		f.mu.Lock()
		f.created = true
		f.emailCode, f.phoneCode = "111111", "222222"
		f.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})

	mux.HandleFunc("/account/verification", func(w http.ResponseWriter, r *http.Request) {
		f.rotate(w)
		if r.Method == http.MethodPost {
			f.mu.Lock()
			f.seen = append(f.seen, "email-send")
			f.mu.Unlock()
			fmt.Fprint(w, `{"sent":true}`)
			return
		}
		var body struct {
			Name             string            `json:"name"`
			VerificationInfo map[string]string `json:"verificationInfo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.VerificationInfo["answer"] != f.emailCode {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"code":"verificationCode.Invalid"}]}`)
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, "email-submit")
		f.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})

	mux.HandleFunc("/account/verification/phone", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		bits := f.hcBits
		f.mu.Unlock()
		stamp := r.Header.Get("X-Apple-HC")
		if stamp == "" || !hashcashSatisfies(stamp, bits) {
			f.rotate(w)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"code":"hashcash.Invalid"}]}`)
			return
		}
		var body struct {
			PhoneNumberVerification struct {
				PhoneNumber map[string]any `json:"phoneNumber"`
				Mode        string         `json:"mode"`
			} `json:"phoneNumberVerification"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.PhoneNumberVerification.Mode != "sms" {
			t.Errorf("phone mode = %q, want sms", body.PhoneNumberVerification.Mode)
		}
		f.rotate(w)
		if r.Method == http.MethodPost {
			f.mu.Lock()
			f.seen = append(f.seen, "phone-send")
			f.mu.Unlock()
			fmt.Fprint(w, `{"sent":true}`)
			return
		}
		code, _ := body.PhoneNumberVerification.PhoneNumber["securityCode"].(map[string]any)
		if fmt.Sprint(code["code"]) != f.phoneCode {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"code":"verificationCode.Invalid"}]}`)
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, "phone-submit")
		f.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})

	return mux
}

func newTestSession(t *testing.T, server *httptest.Server) *Session {
	t.Helper()
	s, err := newSession(SessionOptions{
		Base:      server.URL,
		UserAgent: "test-agent",
		Locale:    "en-US",
		Timezone:  "America/New_York",
		Timeout:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	return s
}

func TestHashcashStampSatisfiesDifficulty(t *testing.T) {
	for _, bits := range []int{8, 10, 11, 12} {
		stamp, err := hashcashStamp(bits, "deadbeefcafebabe", time.Unix(0, 0))
		if err != nil {
			t.Fatalf("bits=%d: %v", bits, err)
		}
		if !hashcashSatisfies(stamp, bits) {
			t.Fatalf("bits=%d: stamp does not satisfy its own difficulty: %s", bits, stamp)
		}
		if !strings.HasPrefix(stamp, fmt.Sprintf("1:%d:", bits)) {
			t.Fatalf("bits=%d: unexpected stamp shape %s", bits, stamp)
		}
		if !strings.Contains(stamp, "::") {
			t.Fatalf("bits=%d: stamp has no :: separator: %s", bits, stamp)
		}
	}
}

func TestHashcashRejectsMissingChallenge(t *testing.T) {
	if _, err := hashcashStamp(0, "abc", time.Now()); err == nil {
		t.Fatal("want an error when bits are missing")
	}
	if _, err := hashcashStamp(10, "  ", time.Now()); err == nil {
		t.Fatal("want an error when the challenge is empty")
	}
	if _, err := hashcashStamp(hashcashMaxBits+1, "abc", time.Now()); err == nil {
		t.Fatal("want an error when the difficulty is absurd")
	}
}

func TestBootstrapAbsorbsServerHeaders(t *testing.T) {
	fake := newFakeApple()
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	s := newTestSession(t, server)
	if err := s.Bootstrap(context.Background(), "USA"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if s.scnt == "" {
		t.Fatal("no scnt absorbed")
	}
	if s.hcBits != 10 || s.hcChallenge == "" {
		t.Fatalf("hashcash state not absorbed: bits=%d challenge=%q", s.hcBits, s.hcChallenge)
	}
	if s.widgetKey != "widget-key-from-portal" {
		t.Fatalf("widget key = %q, want the portal's serviceKey", s.widgetKey)
	}
	if !strings.HasSuffix(s.scnt, "|2") {
		t.Fatalf("scnt = %q, want the newest value the server issued", s.scnt)
	}
}

func TestPreflightIsReadOnlyAndUsesRotatingScnt(t *testing.T) {
	fake := newFakeApple()
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	s := newTestSession(t, server)
	if err := s.Bootstrap(context.Background(), "USA"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	free, used, err := s.EmailFree(context.Background(), "someone@example.test")
	if err != nil {
		t.Fatalf("EmailFree: %v", err)
	}
	if !free || used {
		t.Fatalf("free=%v used=%v, want free and unused", free, used)
	}
}

func TestFullCreateFlowOverPlainHTTP(t *testing.T) {
	fake := newFakeApple()
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	ctx := context.Background()
	s := newTestSession(t, server)
	if err := s.Bootstrap(ctx, "USA"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := s.CheckPassword(ctx, "a@b.test", "Str0ng#Pass", "James", "Carter"); err != nil {
		t.Fatalf("CheckPassword: %v", err)
	}
	c, err := s.RequestCaptcha(ctx)
	if err != nil {
		t.Fatalf("RequestCaptcha: %v", err)
	}
	if string(c.image) != "fakejpeg" {
		t.Fatalf("captcha image = %q", string(c.image))
	}
	account := Account{
		Email: "a@b.test", Password: "Str0ng#Pass", FirstName: "James", LastName: "Carter",
		Birthday: "1992-07-18", CountryISO3: "USA", Language: "en_US",
	}
	phone := Phone{Number: "(415) 555-0134", CountryCode2: "US", DialCode: "1"}
	if _, err := s.Create(ctx, account, phone, c, "ABCDE"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := fake.createdBody
	if body == nil {
		t.Fatal("create body never arrived")
	}
	acc, _ := body["account"].(map[string]any)
	if acc["name"] != "a@b.test" || acc["password"] != "Str0ng#Pass" {
		t.Fatalf("account block = %v", acc)
	}
	if _, ok := acc["verificationInfo"]; !ok {
		t.Fatal("account block lost verificationInfo")
	}
	pnv, _ := body["phoneNumberVerification"].(map[string]any)
	if pnv["mode"] != "sms" {
		t.Fatalf("phone mode = %v", pnv["mode"])
	}
	if _, ok := body["privacyPolicyChecked"]; !ok {
		t.Fatal("create body lost privacyPolicyChecked")
	}

	if _, err := s.SendEmailCode(ctx, "a@b.test"); err != nil {
		t.Fatalf("SendEmailCode: %v", err)
	}
	if _, err := s.SubmitEmailCode(ctx, "a@b.test", "111111"); err != nil {
		t.Fatalf("SubmitEmailCode: %v", err)
	}
	if _, err := s.SendPhoneCode(ctx, phone); err != nil {
		t.Fatalf("SendPhoneCode: %v", err)
	}
	if _, err := s.SubmitPhoneCode(ctx, phone, "222222"); err != nil {
		t.Fatalf("SubmitPhoneCode: %v", err)
	}

	free, used, err := s.EmailFree(ctx, "a@b.test")
	if err != nil {
		t.Fatalf("EmailFree after create: %v", err)
	}
	if free || !used {
		t.Fatalf("after create free=%v used=%v, want taken", free, used)
	}
	if len(fake.seen) != 5 {
		t.Fatalf("expected 5 state-changing calls, got %v", fake.seen)
	}
}

func TestCreateWithoutHashcashSurfacesServerVerdict(t *testing.T) {
	fake := newFakeApple()
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	s := newTestSession(t, server)
	if err := s.Bootstrap(context.Background(), "USA"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Simulate the bad state that produced the historical 403: no challenge.
	s.hcBits, s.hcChallenge = 0, ""
	_, err := s.Create(context.Background(), Account{Email: "x@y.test", Password: "p", FirstName: "a", LastName: "b"},
		Phone{Number: "4155550134", CountryCode2: "US", DialCode: "1"}, &captcha{Token: "t"}, "ABCDE")
	if err == nil {
		t.Fatal("want an error when the hashcash challenge is missing")
	}
	if !strings.Contains(err.Error(), "hashcash") {
		t.Fatalf("error should name hashcash, got: %v", err)
	}
}

func TestGeneratedPasswordSatifiesFourClasses(t *testing.T) {
	for i := 0; i < 50; i++ {
		pw := GeneratedPassword(14)
		if len(pw) != 14 {
			t.Fatalf("length %d", len(pw))
		}
		var upper, lower, digit, special bool
		for _, r := range pw {
			switch {
			case r >= 'A' && r <= 'Z':
				upper = true
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= '0' && r <= '9':
				digit = true
			default:
				special = true
			}
		}
		if !upper || !lower || !digit || !special {
			t.Fatalf("password %q misses a character class", pw)
		}
	}
}

func TestPhoneFormatting(t *testing.T) {
	if got := formatPhone("4155550134", "US"); got != "(415) 555-0134" {
		t.Fatalf("US format = %q", got)
	}
	if got := formatPhone("7911123456", "GB"); got != "7911123456" {
		t.Fatalf("non-US format = %q", got)
	}
	if got := localDialCode("GB"); got != "44" {
		t.Fatalf("GB dial code = %q", got)
	}
}
