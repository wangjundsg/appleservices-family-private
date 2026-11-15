package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Strict fixture derived from the stored widget's call sites. This verifies our
// contract implementation; it is not a claim that Apple accepted a real account.
type strictSignup struct {
	mu                                      sync.Mutex
	boots, sequence                         int
	phase                                   string
	scnt                                    string
	calls                                   map[string]int
	dropFinal, wrongFinal, omitVerification bool
}

func (f *strictSignup) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		if f.calls == nil {
			f.calls = map[string]int{}
		}
		f.calls[key]++
		if r.URL.Path == "/bootstrap/portal" {
			fmt.Fprint(w, `{"serviceKey":"fixture-widget"}`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/account" {
			f.boots++
			if f.boots != 1 {
				t.Error("original session was bootstrapped again")
			}
			http.SetCookie(w, &http.Cookie{Name: "bound", Value: "fixture-session", Path: "/", HttpOnly: true})
		} else {
			cookie, err := r.Cookie("bound")
			if err != nil || cookie.Value != "fixture-session" || r.Header.Get("scnt") != f.scnt || r.Header.Get("X-Apple-ID-Session-Id") != "fixture-session" {
				t.Error("session/cookie continuity lost")
				w.WriteHeader(403)
				return
			}
		}
		f.sequence++
		f.scnt = fmt.Sprint("fixture-", f.sequence)
		w.Header().Set("scnt", f.scnt)
		w.Header().Set("X-Apple-ID-Session-Id", "fixture-session")
		w.Header().Set("X-Apple-HC-Bits", "8")
		w.Header().Set("X-Apple-HC-Challenge", "fixture-challenge")
		var b map[string]any
		if r.Method != "GET" {
			if json.NewDecoder(r.Body).Decode(&b) != nil {
				t.Error("missing JSON body")
				w.WriteHeader(400)
				return
			}
		}
		verifyBody := func(phoneCode bool) {
			a, ok := b["account"].(map[string]any)
			if !ok || a["name"] != "fixture@example.test" {
				t.Error("full account body missing")
				return
			}
			v, ok := a["verificationInfo"].(map[string]any)
			if !ok || v["id"] != "mail-challenge-1" || v["answer"] != "012345" {
				t.Error("email challenge binding lost")
			}
			p, ok := b["phoneNumberVerification"].(map[string]any)
			if !ok {
				t.Error("phone body missing")
				return
			}
			if phoneCode {
				c, ok := p["securityCode"].(map[string]any)
				if !ok || c["code"] != "065432" {
					t.Error("SMS code must be beside phoneNumber")
				}
			}
		}
		switch key {
		case "GET /account":
			fmt.Fprint(w, `{}`)
		case "POST /account/validation/appleid":
			fmt.Fprintf(w, `{"valid":true,"used":%t}`, f.phase == "created")
		case "POST /account/validate/password":
			w.WriteHeader(204)
		case "POST /captcha":
			fmt.Fprintf(w, `{"id":1,"token":"fixture-token","payload":{"content":%q}}`, base64.StdEncoding.EncodeToString([]byte("fixture-image")))
		case "POST /account/validate":
			if f.phase != "" {
				t.Error("validate replayed")
			}
			f.phase = "validated"
			fmt.Fprint(w, `{}`)
		case "POST /account/verification":
			if f.phase != "validated" || b["countryCode"] != "USA" || b["account"] == nil || b["name"] != nil {
				t.Error("wrong email send body/order")
			}
			f.phase = "email-sent"
			if f.omitVerification {
				fmt.Fprint(w, `{}`)
			} else {
				fmt.Fprint(w, `{"verificationId":"mail-challenge-1"}`)
			}
		case "PUT /account/verification":
			v, _ := b["verificationInfo"].(map[string]any)
			if f.phase != "email-sent" || v["id"] != "mail-challenge-1" || v["answer"] != "012345" {
				t.Error("wrong email verification")
			}
			f.phase = "email-verified"
			fmt.Fprint(w, `{}`)
		case "POST /account/verification/phone":
			if f.phase != "email-verified" {
				t.Error("phone sent before email verified")
			}
			verifyBody(false)
			f.phase = "phone-sent"
			fmt.Fprint(w, `{}`)
		case "PUT /account/verification/phone":
			if f.phase != "phone-sent" {
				t.Error("phone submission out of order")
			}
			verifyBody(true)
			f.phase = "phone-verified"
			fmt.Fprint(w, `{}`)
		case "POST /account":
			if f.phase != "phone-verified" {
				t.Error("final creation out of order")
			}
			verifyBody(true)
			f.phase = "created"
			if f.dropFinal {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			name := "fixture@example.test"
			if f.wrongFinal {
				name = "other@example.test"
			}
			fmt.Fprintf(w, `{"name":%q,"person":{"name":{"firstName":"Fixture"}}}`, name)
		default:
			t.Error("unexpected endpoint", key)
			w.WriteHeader(404)
		}
	})
}
func invokeSignup(t *testing.T, path, base string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	argv := []string{"-email", "fixture@example.test", "-state", path, "-base", base, "-skip-landing", "-json"}
	argv = append(argv, args...)
	code := runCLI(context.Background(), argv, &out, &diagnostic)
	var receipt map[string]any
	if json.Unmarshal(out.Bytes(), &receipt) != nil {
		t.Fatalf("non JSON output: %s %s", out.String(), diagnostic.String())
	}
	for _, secret := range []string{"012345", "065432", "fixture-session", "fixture-token", "mail-challenge-1"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("secret in public receipt")
		}
	}
	return code, receipt
}
func toPhoneVerified(t *testing.T, path, base string) {
	t.Helper()
	steps := [][]string{{"-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE"}, {"-stage", "email"}, {"-stage", "email", "-code", "012345"}, {"-stage", "phone"}, {"-stage", "phone", "-code", "065432"}}
	for _, args := range steps {
		code, r := invokeSignup(t, path, base, args...)
		if code != 0 {
			t.Fatalf("%v failed: %v", args[0:2], r)
		}
	}
}
func TestCLIResumesSessionThroughFinalCreation(t *testing.T) {
	f := &strictSignup{}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	toPhoneVerified(t, path, server.URL)
	st, _ := loadState(path)
	if st.Created {
		t.Fatal("phone verification mislabeled as account creation")
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "verify"); code == 0 {
		t.Fatal("verify accepted without final creation")
	}
	for _, stage := range []string{"finalize", "finalize", "verify", "all"} {
		if code, r := invokeSignup(t, path, server.URL, "-stage", stage); code != 0 {
			t.Fatal(stage, r)
		}
	}
	st, _ = loadState(path)
	if !st.Created || st.Status != "ready" || f.calls["POST /account"] != 1 || f.boots != 1 {
		t.Fatal("final state or exactly-once boundary incorrect")
	}
}
func TestUnknownFinalCreationNeverReplays(t *testing.T) {
	f := &strictSignup{dropFinal: true}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	toPhoneVerified(t, path, server.URL)
	for i := 0; i < 2; i++ {
		if code, _ := invokeSignup(t, path, server.URL, "-stage", "finalize"); code == 0 {
			t.Fatal("unknown write treated as success")
		}
	}
	st, _ := loadState(path)
	if st.Status != "needs_review" || st.Created || f.calls["POST /account"] != 1 {
		t.Fatal("unknown write replayed or accepted")
	}
}
func TestFinalResponseMustBindAccount(t *testing.T) {
	f := &strictSignup{wrongFinal: true}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	toPhoneVerified(t, path, server.URL)
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "finalize"); code == 0 {
		t.Fatal("wrong account accepted")
	}
}
func TestAllWaitsAndResumesWithoutResending(t *testing.T) {
	f := &strictSignup{}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.json")
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "all", "-phone", "4155550134", "-captcha-answer", "ABCDE"); code != 4 {
		t.Fatal("must wait for email")
	}
	emailFile := filepath.Join(dir, "email.txt")
	phoneFile := filepath.Join(dir, "phone.txt")
	_ = os.WriteFile(emailFile, []byte("012345"), 0600)
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "all", "-email-code-file", emailFile); code != 4 {
		t.Fatal("must wait for phone")
	}
	_ = os.WriteFile(phoneFile, []byte("065432"), 0600)
	if code, r := invokeSignup(t, path, server.URL, "-stage", "all", "-phone-code-file", phoneFile); code != 0 || r["accountExists"] != true {
		t.Fatal(r)
	}
	if f.calls["POST /account/verification"] != 1 || f.calls["POST /account/verification/phone"] != 1 {
		t.Fatal("verification was resent")
	}
}
func TestMissingEmailChallengeStopsAndProfileCannotChange(t *testing.T) {
	f := &strictSignup{omitVerification: true}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	if code, r := invokeSignup(t, path, server.URL, "-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE"); code != 0 {
		t.Fatal(r)
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "email", "-tz", "Asia/Shanghai"); code != 2 {
		t.Fatal("profile change accepted")
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "email"); code == 0 {
		t.Fatal("missing challenge accepted")
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "email", "-code", "012345"); code == 0 {
		t.Fatal("continued with empty challenge")
	}
	if f.calls["PUT /account/verification"] != 0 {
		t.Fatal("unbound code was sent")
	}
}
func TestStateLockAndCookieExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json")
	unlock, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = lockState(path); err == nil {
		t.Fatal("concurrent task acquired lock")
	}
	s, err := newSession(SessionOptions{Base: "http://127.0.0.1", Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := http.NewRequest("GET", "http://127.0.0.1/private/check", nil)
	s.client.Jar.SetCookies(u.URL, []*http.Cookie{{Name: "expired", Value: "fixture", Path: "/private", Expires: time.Now().Add(-time.Second)}, {Name: "scoped", Value: "fixture", Path: "/private"}})
	s.scnt = "fixture"
	s.sessionID = "fixture"
	snap, err := s.snapshot(SessionOptions{Base: "http://127.0.0.1", Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restoreSession(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if cookies := restored.client.Jar.Cookies(u.URL); len(cookies) != 1 || cookies[0].Name != "scoped" {
		t.Fatal("cookie expiry/path lost")
	}
	u.URL.Path = "/elsewhere"
	if len(restored.client.Jar.Cookies(u.URL)) != 0 {
		t.Fatal("cookie scope broadened")
	}
}

func TestSignupCLIChild(t *testing.T) {
	if os.Getenv("SIGNUP_TEST_CHILD") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runCLI(context.Background(), os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	os.Exit(2)
}

func TestSeparateCLIProcessesRestoreGoAndCurlSessions(t *testing.T) {
	for _, curl := range []bool{false, true} {
		t.Run(fmt.Sprint("curl=", curl), func(t *testing.T) {
			if curl {
				if _, err := exec.LookPath("curl.exe"); err != nil {
					t.Skip("curl not installed")
				}
			}
			f := &strictSignup{}
			server := httptest.NewServer(f.handler(t))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "job.json")
			steps := [][]string{{"-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE"}, {"-stage", "email"}, {"-stage", "email", "-code", "012345"}, {"-stage", "phone"}, {"-stage", "phone", "-code", "065432"}, {"-stage", "finalize"}, {"-stage", "verify"}}
			if curl {
				steps[0] = append(steps[0], "-curl")
			}
			for _, step := range steps {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				args := []string{"-test.run=^TestSignupCLIChild$", "--", "-email", "fixture@example.test", "-state", path, "-base", server.URL, "-skip-landing", "-json"}
				args = append(args, step...)
				cmd := exec.CommandContext(ctx, os.Args[0], args...)
				cmd.Env = append(os.Environ(), "SIGNUP_TEST_CHILD=1")
				output, err := cmd.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("child failed: %v %s", err, output)
				}
			}
			if f.boots != 1 || f.calls["POST /account"] != 1 {
				t.Fatal("subprocess flow replayed bootstrap/finalize")
			}
		})
	}
}

func TestInterruptedIntentAndLegacyStateAreNotReplayed(t *testing.T) {
	f := &strictSignup{}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	if code, r := invokeSignup(t, path, server.URL, "-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE"); code != 0 {
		t.Fatal(r)
	}
	st, _ := loadState(path)
	st.Operations["email-send"] = operation{Digest: "fixture", Status: "intent"}
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "email"); code == 0 || f.calls["POST /account/verification"] != 0 {
		t.Fatal("interrupted intent was replayed")
	}
	st.Version = 0
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	if code, _ := invokeSignup(t, path, server.URL, "-stage", "all"); code != 2 {
		t.Fatal("legacy state was silently adopted")
	}
}

func TestCooldownAndErrorCodeSurviveRestart(t *testing.T) {
	f := &strictSignup{}
	inner := f.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/validate" {
			w.WriteHeader(423)
			fmt.Fprint(w, `{"service_errors":[{"code":"fixture.rateLimited"}],"message":"private-response"}`)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	for i := 0; i < 2; i++ {
		code, r := invokeSignup(t, path, server.URL, "-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE")
		if code == 0 || r["state"] != "rate_limited" {
			t.Fatal(r)
		}
	}
	st, _ := loadState(path)
	if len(st.ErrorCodes) != 1 || st.ErrorCodes[0] != "fixture.rateLimited" {
		t.Fatal("error code lost on restart")
	}
}

func TestCodeFileCannotPredateChallenge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old-code.txt")
	_ = os.WriteFile(path, []byte("012345"), 0600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	w := &workflow{state: &state{Stage: "email-sent", EmailRequestedAt: time.Now()}}
	if _, err := w.codeFromFile(context.Background(), path); err == nil {
		t.Fatal("stale code accepted")
	}
}

func TestSavedCaptchaCanBeAnsweredWithoutFetchingAnother(t *testing.T) {
	f := &strictSignup{}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "job.json")
	if code, r := invokeSignup(t, path, server.URL, "-stage", "captcha"); code != 0 {
		t.Fatal(r)
	}
	if code, r := invokeSignup(t, path, server.URL, "-stage", "create", "-phone", "4155550134", "-captcha-answer", "ABCDE"); code != 0 {
		t.Fatal(r)
	}
	if f.calls["POST /captcha"] != 1 {
		t.Fatal("answer submitted against a new challenge")
	}
}
