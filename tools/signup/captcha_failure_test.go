package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptchaFailurePreservesHTTPAndStopsResume(t *testing.T) {
	for _, status := range []int{401, 403, 423, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			base := deviceFixture(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/captcha" {
					calls++
					w.WriteHeader(status)
					fmt.Fprint(w, `{"service_errors":[{"code":"fixture_code","message":"private-provider-marker"}]}`)
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "task.json")
			code, receipt := invokeSignup(t, path, server.URL, "-stage", "captcha")
			wantState := "needs_review"
			if status == 423 || status == 429 {
				wantState = "rate_limited"
			}
			if code == 0 || receipt["reason"] != "captcha_request_failed" || receipt["httpStatus"] != float64(status) || receipt["state"] != wantState || receipt["failureClass"] != fmt.Sprintf("http_%d", status) {
				t.Fatalf("captcha failure lost evidence: %v", receipt)
			}
			st, err := loadState(path)
			if err != nil || len(st.ErrorCodes) != 1 || st.ErrorCodes[0] != "fixture_code" || st.HTTPStatus != status || st.Failure != "captcha_request_failed" {
				t.Fatal("failure evidence not persisted")
			}
			if wantState == "rate_limited" && st.CooldownUntil.IsZero() {
				t.Fatal("missing cooldown")
			}
			raw, _ := json.Marshal(receipt)
			if strings.Contains(string(raw), "private-provider-marker") {
				t.Fatal("provider text leaked")
			}
			code, _ = invokeSignup(t, path, server.URL, "-stage", "captcha")
			if code == 0 || calls != 1 {
				t.Fatal("resume repeated a rejected request")
			}
		})
	}
}
