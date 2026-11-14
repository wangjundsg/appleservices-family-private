// appleweb owns one web login through 2FA and terms, or validates a saved session.
// Credentials arrive through the environment; stdout is a value-free JSONL protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/appleweb"
)

type authFlow interface {
	Submit2FA(context.Context, string, string, json.RawMessage) (appleweb.ICloudSession, error)
	Terms(context.Context, string) (appleweb.ICloudTerms, error)
	AcceptTerms(context.Context, string, string, bool) (appleweb.ICloudSession, error)
}

func finish(ctx context.Context, f authFlow, start appleweb.AuthStartResult, consent bool, code func() (string, error), emit func(string)) (appleweb.ICloudSession, error) {
	session := start.Session
	needsTerms := start.NeedsTerms
	if start.Needs2FA {
		emit("waiting_input")
		value, err := code()
		if err != nil {
			return session, err
		}
		session, err = f.Submit2FA(ctx, start.PendingID, value, nil)
		if err != nil {
			name, _, _ := appleweb.ErrorDetails(err)
			if name != "icloud_terms_update_needed" {
				return session, err
			}
			needsTerms = true
		}
	}
	if needsTerms {
		emit("terms")
		if !consent {
			return session, errors.New("terms_consent_required")
		}
		terms, err := f.Terms(ctx, start.PendingID)
		if err != nil {
			return session, err
		}
		return f.AcceptTerms(ctx, start.PendingID, terms.Version, true)
	}
	return session, nil
}

func saveSession(file string, value appleweb.ICloudSession, account string) error {
	if !strings.EqualFold(value.AppleID, account) || value.DSID == "" || len(value.Cookies) == 0 {
		return errors.New("session_binding_failed")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".web-session-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), file)
}

func waitCode(ctx context.Context, file string) (string, error) {
	if file == "" {
		return "", errors.New("code_file_required")
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(file)
		if err == nil && regexp.MustCompile(`^\d{6}$`).MatchString(strings.TrimSpace(string(data))) {
			if err := os.Remove(file); err != nil {
				return "", errors.New("code_file_consume_failed")
			}
			return strings.TrimSpace(string(data)), nil
		}
		select {
		case <-ctx.Done():
			return "", errors.New("code_timeout")
		case <-tick.C:
		}
	}
}

func emit(state string) { _ = json.NewEncoder(os.Stdout).Encode(map[string]string{"state": state}) }

func run() error {
	mode := flag.String("mode", "login", "login, probe (no code request), or keepalive")
	file := flag.String("session", "", "private session file")
	codeFile := flag.String("code-file", "", "one-use six-digit code file")
	host := flag.String("host", "www.icloud.com", "www.icloud.com or www.icloud.com.cn")
	consent := flag.Bool("accept-terms", false, "accept the current iCloud terms during this login")
	timeout := flag.Duration("timeout", 10*time.Minute, "total operation deadline")
	flag.Parse()
	account, password := os.Getenv("APPLESERVICES_APPLEID"), os.Getenv("APPLESERVICES_PASSWORD")
	if *file == "" || account == "" || (*mode != "login" && *mode != "keepalive" && *mode != "probe") || *timeout <= 0 || (*host != "www.icloud.com" && *host != "www.icloud.com.cn") {
		return errors.New("invalid_arguments")
	}
	ua, language := appleweb.IdentityFromEnv()
	appleweb.SetIdentityOverride(ua, language)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var session appleweb.ICloudSession
	var err error
	if *mode == "keepalive" {
		data, e := os.ReadFile(*file)
		if e != nil || json.Unmarshal(data, &session) != nil || !strings.EqualFold(session.AppleID, account) {
			return errors.New("session_binding_failed")
		}
		session, err = appleweb.NewICloudSessionValidator().ValidateSession(ctx, session, session.Host)
	} else {
		if password == "" {
			return errors.New("password_required")
		}
		f := appleweb.NewAuthFacade()
		method := "sms"
		if *mode == "probe" {
			method = "probe"
		}
		start, e := f.Start(ctx, appleweb.AuthFlowICloudWeb, account, password, *host, "", method)
		if e != nil {
			return e
		}
		if *mode == "probe" {
			emit("account_exists")
			return nil
		}
		session, err = finish(ctx, f, start, *consent, func() (string, error) { return waitCode(ctx, *codeFile) }, emit)
	}
	if err != nil {
		return err
	}
	if err = saveSession(*file, session, account); err != nil {
		return err
	}
	emit("success")
	return nil
}

func main() {
	if err := run(); err != nil {
		state := "unknown"
		if strings.Contains(err.Error(), "423") || strings.Contains(err.Error(), "tooManyCodesSent") {
			state = "rate_limited"
		}
		if err.Error() == "code_timeout" {
			state = "waiting_input"
		}
		emit(state)
		// Never echo upstream responses, URLs containing credentials, or tokens.
		fmt.Fprintln(os.Stderr, "appleweb: operation did not complete; private session preserved")
		os.Exit(3)
	}
}
