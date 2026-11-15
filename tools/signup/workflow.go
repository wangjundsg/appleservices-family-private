package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

const signupStateVersion = 1

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)
var safeErrorCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
var errWaiting = errors.New("waiting_input")

type operation struct {
	Digest string
	Status string
}
type workflow struct {
	state                *state
	session              *Session
	path                 string
	options              SessionOptions
	captchaAnswer        func(context.Context, *captcha) (string, error)
	emailFile, phoneFile string
	wait                 time.Duration
	pacer                *pacing.Pacer
}

// PacerApplied reports the waits this run actually performed.
func (w *workflow) PacerApplied() []string {
	if w.pacer == nil {
		return nil
	}
	return w.pacer.Applied()
}

func (w *workflow) save() error {
	v, err := w.session.snapshot(w.options)
	if err != nil {
		return err
	}
	w.state.Session = v
	return writeState(w.path, w.state)
}
func (w *workflow) once(ctx context.Context, name, next string, body any, apply func() error) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	if w.state.Operations == nil {
		w.state.Operations = map[string]operation{}
	}
	if prior, ok := w.state.Operations[name]; ok {
		if prior.Status == "succeeded" && prior.Digest == key {
			return nil
		}
		return errors.New("operation_requires_review")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	w.state.Operations[name] = operation{key, "intent"}
	w.state.Status = "running"
	if err = w.save(); err != nil {
		return errors.New("state_write_failed_before_request")
	}
	if err = apply(); err != nil {
		w.state.Status = "needs_review"
		w.state.Failure = "operation_unconfirmed"
		switch err.Error() {
		case "verification_id_missing", "verification_id_empty", "verification_id_invalid", "final_account_binding_unconfirmed":
			w.state.Failure = err.Error()
		}
		var pe *ProtocolError
		if errors.As(err, &pe) {
			w.state.HTTPStatus = pe.Status
			var raw map[string]json.RawMessage
			if json.Unmarshal([]byte(pe.Body), &raw) == nil {
				for _, field := range []string{"errors", "service_errors", "serviceErrors", "validationErrors"} {
					var items []struct {
						Code string `json:"code"`
					}
					if json.Unmarshal(raw[field], &items) == nil {
						for _, item := range items {
							if safeErrorCode.MatchString(item.Code) {
								w.state.ErrorCodes = append(w.state.ErrorCodes, item.Code)
							}
						}
					}
				}
			}
			if pe.Status == 423 || pe.Status == 429 {
				w.state.Status = "rate_limited"
				w.state.Failure = "server_requires_cooldown"
			}
		}
		if saveErr := w.save(); saveErr != nil {
			return errors.New("state_write_failed_after_request")
		}
		return errors.New(w.state.Failure)
	}
	w.state.Operations[name] = operation{key, "succeeded"}
	w.state.Stage = next
	w.state.Status = "running"
	w.state.Failure = ""
	w.state.HTTPStatus = 0
	w.state.ErrorCodes = nil
	if err = w.save(); err != nil {
		return errors.New("state_write_failed_after_request")
	}
	return nil
}
func (w *workflow) request(ctx context.Context, action, method, path, next string, body any, hc bool, decode func([]byte) error) error {
	// The wait happens before the request is built, never after an effect, so a
	// crash cannot leave Apple with a half-applied action plus silence.
	if w.pacer != nil {
		if _, err := w.pacer.Before(ctx, w.state.Stage, action); err != nil {
			return err
		}
	}
	return w.once(ctx, action, next, body, func() error {
		raw, err := w.session.do(ctx, action, method, path, body, hc)
		if err != nil {
			return err
		}
		if decode != nil {
			return decode(raw)
		}
		return nil
	})
}
func (w *workflow) prepare(ctx context.Context) error {
	st := w.state
	if st.Stage != "" {
		return nil
	}
	if err := w.session.Bootstrap(ctx, st.Country); err != nil {
		return errors.New("bootstrap_unconfirmed")
	}
	if w.session.sessionID == "" {
		return errors.New("bootstrap_session_missing")
	}
	free, used, err := w.session.EmailFree(ctx, st.Email)
	if err != nil || !free || used {
		return errors.New("mailbox_not_confirmed_available")
	}
	if err = w.session.CheckPassword(ctx, st.Email, st.Password, st.FirstName, st.LastName); err != nil {
		return errors.New("password_policy_rejected")
	}
	st.Stage = "preflight"
	st.Status = "running"
	return w.save()
}
func (w *workflow) validate(ctx context.Context) error {
	if w.state.Stage != "preflight" && w.state.Stage != "captcha" {
		return nil
	}
	if w.state.Operations["validate"].Status != "" {
		return errors.New("operation_requires_review")
	}
	if w.state.Body == nil {
		return errors.New("captcha_answer_required")
	}
	return w.request(ctx, "validate", "POST", "/account/validate", "validated", w.state.Body, true, nil)
}
func (w *workflow) getCaptcha(ctx context.Context) (*captcha, error) {
	if w.state.Stage != "preflight" && w.state.Stage != "captcha" {
		return nil, errors.New("captcha_out_of_order")
	}
	if w.state.Operations["validate"].Status != "" {
		return nil, errors.New("operation_requires_review")
	}
	c, err := w.session.RequestCaptcha(ctx)
	if err != nil {
		return nil, errors.New("captcha_request_failed")
	}
	w.state.Captcha = c
	w.state.Body = nil
	w.state.Stage = "captcha"
	if err = w.save(); err != nil {
		return nil, err
	}
	return c, nil
}
func (w *workflow) answerCaptcha(ctx context.Context) error {
	if w.state.Body != nil {
		return nil
	}
	c := w.state.Captcha
	if c == nil {
		var err error
		c, err = w.getCaptcha(ctx)
		if err != nil {
			return err
		}
	}
	if err := c.decodeImage(); err != nil {
		return err
	}
	answer, err := w.captchaAnswer(ctx, c)
	if err != nil {
		return err
	}
	w.state.Body = createBody(accountFrom(w.state), phoneFrom(w.state), c, answer)
	return w.save()
}
func (w *workflow) sendEmail(ctx context.Context) error {
	if w.state.Stage != "validated" {
		return errors.New("email_send_out_of_order")
	}
	st := w.state
	body := map[string]any{"account": map[string]any{"name": st.Email, "person": map[string]any{"name": map[string]string{"firstName": st.FirstName, "lastName": st.LastName}}}, "countryCode": st.Country}
	st.EmailRequestedAt = time.Now().UTC()
	return w.request(ctx, "email-send", "POST", "/account/verification", "email-sent", body, false, func(raw []byte) error {
		var reply struct {
			VerificationID json.RawMessage `json:"verificationId"`
		}
		if json.Unmarshal(raw, &reply) != nil || len(reply.VerificationID) == 0 || string(reply.VerificationID) == "null" {
			return errors.New("verification_id_missing")
		}
		var id string
		if json.Unmarshal(reply.VerificationID, &id) != nil {
			var n json.Number
			if json.Unmarshal(reply.VerificationID, &n) != nil {
				return errors.New("verification_id_invalid")
			}
			id = n.String()
		}
		if strings.TrimSpace(id) == "" {
			return errors.New("verification_id_empty")
		}
		st.VerificationID = reply.VerificationID
		return nil
	})
}
func (w *workflow) submitEmail(ctx context.Context, code string) error {
	if w.state.Stage != "email-sent" || !sixDigits.MatchString(code) {
		return errors.New("email_code_or_stage_invalid")
	}
	verification := map[string]any{"id": w.state.VerificationID, "answer": code}
	body := map[string]any{"name": w.state.Email, "verificationInfo": verification}
	return w.request(ctx, "email-submit", "PUT", "/account/verification", "email-verified", body, false, func([]byte) error {
		w.state.Body["account"].(map[string]any)["verificationInfo"] = verification
		w.state.EmailDone = true
		return nil
	})
}
func (w *workflow) sendPhone(ctx context.Context) error {
	if w.state.Stage != "email-verified" {
		return errors.New("phone_send_out_of_order")
	}
	w.state.PhoneRequestedAt = time.Now().UTC()
	return w.request(ctx, "phone-send", "POST", "/account/verification/phone", "phone-sent", w.state.Body, true, nil)
}
func (w *workflow) submitPhone(ctx context.Context, code string) error {
	if w.state.Stage != "phone-sent" || !sixDigits.MatchString(code) {
		return errors.New("phone_code_or_stage_invalid")
	}
	// The widget puts securityCode beside phoneNumber, not inside phoneNumber.
	w.state.Body["phoneNumberVerification"].(map[string]any)["securityCode"] = map[string]string{"code": code}
	return w.request(ctx, "phone-submit", "PUT", "/account/verification/phone", "phone-verified", w.state.Body, true, func([]byte) error { w.state.PhoneDone = true; return nil })
}
func (w *workflow) finish(ctx context.Context) error {
	if w.state.Created {
		return nil
	}
	if w.state.Stage != "phone-verified" || !w.state.EmailDone || !w.state.PhoneDone {
		return errors.New("finalize_out_of_order")
	}
	return w.request(ctx, "finalize", "POST", "/account", "created", w.state.Body, true, func(raw []byte) error {
		var reply struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &reply) != nil || !strings.EqualFold(reply.Name, w.state.Email) {
			return errors.New("final_account_binding_unconfirmed")
		}
		w.state.Created = true
		return nil
	})
}
func (w *workflow) verify(ctx context.Context) error {
	if !w.state.Created || w.state.Operations["finalize"].Status != "succeeded" {
		return errors.New("final_creation_receipt_required")
	}
	_, used, err := w.session.EmailFree(ctx, w.state.Email)
	if err != nil || !used {
		return errors.New("account_readback_unconfirmed")
	}
	w.state.Stage = "done"
	w.state.Status = "ready"
	return w.save()
}
func (w *workflow) codeFromFile(ctx context.Context, path string) (string, error) {
	deadline := time.Now().Add(w.wait)
	for {
		if path != "" {
			if info, err := os.Stat(path); err == nil {
				since := w.state.EmailRequestedAt
				if w.state.Stage == "phone-sent" {
					since = w.state.PhoneRequestedAt
				}
				if !since.IsZero() && info.ModTime().Before(since) {
					return "", errors.New("code_file_predates_challenge")
				}
			}
			raw, err := os.ReadFile(path)
			if err == nil {
				code := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
				if sixDigits.MatchString(code) {
					return code, nil
				}
				return "", errors.New("invalid_code_file")
			}
			if !os.IsNotExist(err) {
				return "", errors.New("code_file_unreadable")
			}
		}
		if path == "" || !time.Now().Before(deadline) {
			w.state.Status = "waiting_input"
			if err := w.save(); err != nil {
				return "", err
			}
			return "", errWaiting
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (w *workflow) run(ctx context.Context, stage, code string) error {
	if w.state.Status == "needs_review" || w.state.Status == "rate_limited" {
		return errors.New("task_requires_review")
	}
	for _, op := range w.state.Operations {
		if op.Status != "succeeded" {
			w.state.Status = "needs_review"
			w.state.Failure = "operation_requires_review"
			if err := w.save(); err != nil {
				return err
			}
			return errors.New(w.state.Failure)
		}
	}
	if err := w.prepare(ctx); err != nil {
		return err
	}
	switch stage {
	case "preflight":
		return nil
	case "captcha":
		_, err := w.getCaptcha(ctx)
		return err
	case "create":
		if w.state.Stage != "preflight" && w.state.Stage != "captcha" {
			return nil
		}
		if err := w.answerCaptcha(ctx); err != nil {
			return err
		}
		return w.validate(ctx)
	case "email":
		if code == "" {
			if w.state.Stage == "email-sent" || w.state.EmailDone {
				return nil
			}
			return w.sendEmail(ctx)
		}
		if w.state.EmailDone {
			return nil
		}
		return w.submitEmail(ctx, code)
	case "phone":
		if code == "" {
			if w.state.Stage == "phone-sent" || w.state.PhoneDone {
				return nil
			}
			return w.sendPhone(ctx)
		}
		if w.state.PhoneDone {
			return nil
		}
		return w.submitPhone(ctx, code)
	case "finalize":
		return w.finish(ctx)
	case "verify":
		if w.state.Status == "ready" {
			return nil
		}
		return w.verify(ctx)
	case "all":
		for {
			switch w.state.Stage {
			case "preflight", "captcha":
				if err := w.answerCaptcha(ctx); err != nil {
					return err
				}
				if err := w.validate(ctx); err != nil {
					return err
				}
			case "validated":
				if err := w.sendEmail(ctx); err != nil {
					return err
				}
			case "email-sent":
				code, err := w.codeFromFile(ctx, w.emailFile)
				if err != nil {
					return err
				}
				if err = w.submitEmail(ctx, code); err != nil {
					return err
				}
			case "email-verified":
				if err := w.sendPhone(ctx); err != nil {
					return err
				}
			case "phone-sent":
				code, err := w.codeFromFile(ctx, w.phoneFile)
				if err != nil {
					return err
				}
				if err = w.submitPhone(ctx, code); err != nil {
					return err
				}
			case "phone-verified":
				if err := w.finish(ctx); err != nil {
					return err
				}
			case "created":
				return w.verify(ctx)
			case "done":
				return nil
			default:
				return fmt.Errorf("unsupported_saved_stage")
			}
		}
	default:
		return errors.New("unsupported_stage")
	}
}
