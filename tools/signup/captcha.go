// Image captcha plumbing for the Apple Account create widget.
//
// The widget asks POST /captcha {"type":"IMAGE"} and gets back
// {"id":<int>,"token":"<hex>","type":"IMAGE","payload":{"contentType":...,
// "content":"<base64 jpeg>"}}. The answer is 5 characters, and id+token are
// single use: every rejected answer needs a fresh image.
//
// Two answer sources, in order of preference:
//   - a manual answer (a human read the image; docs/REGISTRAR-CONTRACT.md A-2
//     measured OCR alone at well under 100% on this 126x56 noisy image)
//   - YesCaptcha (ImageToTextTask), same provider tools/captcha/solver.py uses
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// captcha is one challenge: the identity to echo back plus the image to read.
type captcha struct {
	ID      json.Number `json:"id"`
	Token   string      `json:"token"`
	Type    string      `json:"type"`
	Payload struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"payload"`

	image []byte
}

// decodeImage materializes the base64 payload once.
func (c *captcha) decodeImage() error {
	if c.Payload.Content == "" {
		return errors.New("captcha: response carries no image payload")
	}
	raw, err := base64.StdEncoding.DecodeString(c.Payload.Content)
	if err != nil {
		return fmt.Errorf("captcha: decode image: %w", err)
	}
	c.image = raw
	return nil
}

// answerQuestion returns the characters a human or a paid solver read.
// `manual` wins when set; otherwise YesCaptcha is asked.
func answerQuestion(ctx context.Context, c *captcha, manual, key string, timeout time.Duration) (string, error) {
	if manual != "" {
		return strings.ToUpper(strings.TrimSpace(manual)), nil
	}
	if key == "" {
		return "", errors.New("captcha: no manual answer and no YESCAPTCHA_KEY")
	}
	return solveYesCaptcha(ctx, key, c.image, timeout)
}

// YesCaptcha's two-step protocol, as implemented by tools/captcha/solver.py.
const (
	yesCaptchaCreate = "https://api.yescaptcha.com/createTask"
	yesCaptchaResult = "https://api.yescaptcha.com/getTaskResult"
)

func solveYesCaptcha(ctx context.Context, key string, image []byte, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	client := &http.Client{Timeout: 30 * time.Second}

	createBody := map[string]any{
		"clientKey": key,
		"task": map[string]any{
			"type": "ImageToTextTask",
			"body": base64.StdEncoding.EncodeToString(image),
		},
	}
	var created struct {
		ErrorID int    `json:"errorId"`
		Error   string `json:"errorDescription"`
		TaskID  any    `json:"taskId"`
	}
	if err := postJSON(ctx, client, yesCaptchaCreate, createBody, &created); err != nil {
		return "", fmt.Errorf("yescaptcha createTask: %w", err)
	}
	if created.ErrorID != 0 || created.TaskID == nil {
		return "", fmt.Errorf("yescaptcha createTask rejected: %s", orDefault(created.Error, "no taskId"))
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return "", errors.New("yescaptcha: timed out waiting for the answer")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		var result struct {
			ErrorID  int    `json:"errorId"`
			Error    string `json:"errorDescription"`
			Status   string `json:"status"`
			Solution struct {
				Text string `json:"text"`
			} `json:"solution"`
		}
		query := map[string]any{"clientKey": key, "taskId": created.TaskID}
		if err := postJSON(ctx, client, yesCaptchaResult, query, &result); err != nil {
			return "", fmt.Errorf("yescaptcha getTaskResult: %w", err)
		}
		if result.ErrorID != 0 {
			return "", fmt.Errorf("yescaptcha getTaskResult rejected: %s", orDefault(result.Error, "unknown error"))
		}
		if text := strings.ToUpper(strings.TrimSpace(result.Solution.Text)); text != "" {
			return text, nil
		}
	}
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// writeCaptchaImage saves the challenge so a human can read it.
func writeCaptchaImage(path string, image []byte) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, image, 0o600)
}
