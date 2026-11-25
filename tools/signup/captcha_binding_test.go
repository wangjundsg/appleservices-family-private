package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type noSolverTransport struct{ calls atomic.Int32 }

func (s *noSolverTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return nil, errors.New("unexpected solver call")
}

func TestExplicitCaptchaFileOverridesEnvironmentKey(t *testing.T) {
	// Intercept locally: the test must never spend at the configured solver.
	transport := &noSolverTransport{}
	prior := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = prior }()
	dir := t.TempDir()
	answerPath := filepath.Join(dir, "answer.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(answerPath, []byte("\xef\xbb\xbfAbCd\r\n"), 0600)
	}()
	answer, err := answerQuestion(ctx, &captcha{image: []byte("local-fixture")}, "", "fixture-env-key", 2*time.Second, "", answerPath)
	<-writerDone
	if err != nil || answer != "AbCd" || transport.calls.Load() != 0 {
		t.Fatalf("manual file not selected/normalized: err=%v solverCalls=%d", err, transport.calls.Load())
	}
}

func TestManualCaptchaAnswerPreservesCase(t *testing.T) {
	answer, err := answerQuestion(context.Background(), &captcha{}, "  aB3dE\r\n", "", 0, "", "")
	if err != nil || answer != "aB3dE" {
		t.Fatal("manual answer was changed")
	}
}

func TestDelayedCaptchaAnswerKeepsImageAndChallengeBound(t *testing.T) {
	f := &strictSignup{}
	server := httptest.NewServer(f.handler(t))
	defer server.Close()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "job.json")
	imagePath := filepath.Join(dir, "captcha.jpg")
	if code, r := invokeSignup(t, statePath, server.URL, "-stage", "captcha", "-captcha-dump", imagePath); code != 0 {
		t.Fatal(r)
	}
	st, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := restoreSession(st.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	unlock, err := lockState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	w := &workflow{state: st, session: s, path: statePath, options: st.Session.Options}
	st.Phone = "4155550134"
	id, token := st.Captcha.ID, st.Captcha.Token
	w.captchaAnswer = func(_ context.Context, c *captcha) (string, error) {
		if c != st.Captcha {
			t.Error("answer callback got another challenge")
		}
		image, err := os.ReadFile(imagePath)
		if err != nil || !bytes.Equal(image, c.image) {
			t.Error("displayed image differs from answer input")
		}
		// Waiting does not fetch another image; another process cannot mutate this job.
		time.Sleep(100 * time.Millisecond)
		if code, _ := invokeSignup(t, statePath, server.URL, "-stage", "captcha"); code != 2 {
			t.Error("concurrent process replaced challenge")
		}
		return "ABCDE", nil
	}
	if err = w.answerCaptcha(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = w.validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := saved.Body["captcha"].(map[string]any)
	encodedID, _ := json.Marshal(id)
	submittedID, _ := json.Marshal(body["id"])
	if string(encodedID) != string(submittedID) || body["token"] != token || body["answer"] != "ABCDE" {
		t.Fatal("submitted challenge differs from displayed challenge")
	}
	if f.calls["POST /captcha"] != 1 || f.calls["POST /account/validate"] != 1 {
		t.Fatal("extra captcha/validate request while waiting")
	}
}
