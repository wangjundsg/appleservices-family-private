package main

// A stricter Apple stand-in than strictSignup in one respect: it validates the
// session bindings the cross-process test depends on — the bound cookie, the
// rotating scnt, the verificationId carried into the submit body, and the shape
// of the business payloads. A loose fixture would let a broken resume pass.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type flowFixture struct {
	mu                   sync.Mutex
	email                string
	scnt                 string
	issued               int
	phase                string
	hits                 map[string]int
	violations           int
	violationNotes       []string
	sawVerificationID    bool
	sawRotatedScnt       bool
	sawSessionCookie     bool
	emailCode, phoneCode string
}

func newFlowFixture(email string) *flowFixture {
	return &flowFixture{email: email, scnt: "flow-0", phase: "new", hits: map[string]int{}}
}

func (f *flowFixture) violate(format string, args ...any) {
	f.violations++
	if len(f.violationNotes) < 20 {
		f.violationNotes = append(f.violationNotes, fmt.Sprintf(format, args...))
	}
}

// rotate issues the next scnt and returns it through the response headers.
func (f *flowFixture) rotate(w http.ResponseWriter) {
	f.issued++
	f.scnt = fmt.Sprintf("flow-%d", f.issued)
	w.Header().Set("scnt", f.scnt)
	w.Header().Set("X-Apple-ID-Session-Id", "flow-session")
	w.Header().Set("X-Apple-HC-Bits", "8")
	w.Header().Set("X-Apple-HC-Challenge", "cafebabe")
}

func decodeBody(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func (f *flowFixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits[r.Method+" "+r.URL.Path]++

		// The portal bootstrap is the one call made before any session exists.
		if r.URL.Path == "/bootstrap/portal" {
			fmt.Fprint(w, `{"serviceKey":"flow-widget"}`)
			return
		}
		// Telemetry carries scnt but is not part of the registration contract.
		if r.URL.Path == "/jslog" || r.URL.Path == "/v2/jslogs" {
			f.rotate(w)
			w.WriteHeader(204)
			return
		}
		// The portal document and the create configuration share a path; a
		// browser (and this client) may hit either before any session exists, so
		// both are served without the session gate.
		if r.Method == http.MethodGet && r.URL.Path == "/account" {
			http.SetCookie(w, &http.Cookie{Name: "bound", Value: "flow-session", Path: "/", HttpOnly: true})
			f.rotate(w)
			if r.URL.RawQuery == "" {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "<html>portal</html>")
			} else {
				fmt.Fprint(w, `{"locale":"en_US"}`)
			}
			return
		}

		// Journey pages: apple.com / support, same shape as production.
		if strings.HasPrefix(r.URL.Path, "/www/") || strings.HasPrefix(r.URL.Path, "/support/") || r.URL.Path == "/support" {
			http.SetCookie(w, &http.Cookie{Name: "journey", Value: "1", Path: "/"})
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>apple</html>")
			return
		}

		// Documents and bundles a browser loads without any XHR headers.
		if strings.HasPrefix(r.URL.Path, "/widget/") || strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>widget</html>")
			return
		}

		// Everything below must carry the session cookie and the current scnt.
		cookie, err := r.Cookie("bound")
		if err != nil || cookie.Value != "flow-session" {
			f.violate("%s %s: missing bound cookie", r.Method, r.URL.Path)
		} else {
			f.sawSessionCookie = true
		}
		if got := r.Header.Get("scnt"); got == "" {
			f.violate("%s %s: no scnt header", r.Method, r.URL.Path)
		} else if got != f.scnt {
			f.violate("%s %s: stale scnt %q (server has %q)", r.Method, r.URL.Path, got, f.scnt)
		} else if f.issued > 1 {
			f.sawRotatedScnt = true
		}

		switch r.URL.Path {
		case "/account/validation/appleid":
			f.rotate(w)
			fmt.Fprintf(w, `{"valid":true,"used":%t}`, f.phase == "created")
		case "/account/validate/password":
			f.rotate(w)
			w.WriteHeader(204)
		case "/captcha":
			f.rotate(w)
			fmt.Fprintf(w, `{"id":7,"token":"flow-token","payload":{"content":%q}}`,
				base64.StdEncoding.EncodeToString([]byte("flow-image")))
		case "/account/validate":
			body := decodeBody(r)
			account, _ := body["account"].(map[string]any)
			if account == nil || account["name"] != f.email {
				f.violate("validate: account.name mismatch")
			}
			if _, ok := body["phoneNumberVerification"]; !ok {
				f.violate("validate: no phoneNumberVerification")
			}
			captcha, _ := body["captcha"].(map[string]any)
			if captcha == nil || captcha["answer"] != "ABCDE" || captcha["token"] != "flow-token" {
				f.violate("validate: captcha binding wrong")
			}
			f.phase = "validated"
			f.rotate(w)
			fmt.Fprint(w, `{}`)
		case "/account/verification":
			if r.Method == http.MethodPost {
				body := decodeBody(r)
				account, _ := body["account"].(map[string]any)
				if account == nil || account["name"] != f.email || body["countryCode"] == "" {
					f.violate("email send: account/countryCode wrong")
				}
				f.phase = "email-sent"
				f.rotate(w)
				fmt.Fprint(w, `{"verificationId":"flow-vid"}`)
				return
			}
			body := decodeBody(r)
			info, _ := body["verificationInfo"].(map[string]any)
			if info == nil {
				f.violate("email submit: no verificationInfo")
			} else {
				if info["id"] != "flow-vid" {
					f.violate("email submit: verificationId not carried (%v)", info["id"])
				} else {
					f.sawVerificationID = true
				}
				if code, _ := info["answer"].(string); len(code) != 6 {
					f.violate("email submit: answer is not a 6 digit code")
				}
			}
			f.phase = "email-verified"
			f.rotate(w)
			fmt.Fprint(w, `{}`)
		case "/account/verification/phone":
			body := decodeBody(r)
			verification, _ := body["phoneNumberVerification"].(map[string]any)
			if verification == nil {
				f.violate("phone: no phoneNumberVerification")
			}
			if r.Method == http.MethodPut {
				security, _ := verification["securityCode"].(map[string]any)
				if security == nil || security["code"] == "" {
					f.violate("phone submit: no securityCode")
				}
				f.phase = "phone-verified"
			} else {
				f.phase = "phone-sent"
			}
			f.rotate(w)
			fmt.Fprint(w, `{}`)
		case "/account":
			if r.Method != http.MethodPost {
				f.violate("unexpected %s /account", r.Method)
				w.WriteHeader(404)
				return
			}
			body := decodeBody(r)
			account, _ := body["account"].(map[string]any)
			if account == nil || account["name"] != f.email {
				f.violate("final: account.name mismatch")
			}
			info, _ := account["verificationInfo"].(map[string]any)
			if info == nil || info["id"] == "" {
				f.violate("final: verificationInfo lost")
			}
			f.phase = "created"
			f.rotate(w)
			fmt.Fprintf(w, `{"name":%q}`, f.email)
		default:
			f.rotate(w)
			fmt.Fprint(w, `{}`)
		}
	})
}

func newTLSFixture(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(handler)
}
