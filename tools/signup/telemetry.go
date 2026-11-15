package main

// Telemetry replay: send the widget's own logging batches, from a real capture.
//
// The rule this enforces: no invented events. The replay takes the envelopes a
// real Chrome session produced during one page load, re-stamps their timestamps
// to this session's clock and re-issues them. Event ids are regenerated (a new
// session would also mint new ids); nothing else is changed, added or removed.
//
// A capture without bodies is refused outright — silently sending nothing would
// make the receipt lie about telemetry being active.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

const telemetrySchemaVersion = 1

// allowedTelemetry is the complete set of endpoints this replay may touch.
// Anything else — /account, /account/validate, verification endpoints — must go
// through the workflow, which owns the intent journal; a capture that names them
// is refused rather than silently filtered.
func allowedTelemetry(path, method string) bool {
	if method != http.MethodPost && method != "" {
		return false
	}
	switch path {
	case "/jslog", "/v2/jslogs":
		return true
	default:
		return false
	}
}

type telemetryEnvelope struct {
	At          int64             `json:"at"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Query       string            `json:"query"`
	ContentType string            `json:"contentType"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
}

type telemetryCapture struct {
	SchemaVersion int                 `json:"schemaVersion"`
	CapturedAt    string              `json:"capturedAt"`
	Target        string              `json:"target"`
	URLs          []string            `json:"urls"`
	Envelopes     []telemetryEnvelope `json:"envelopes"`
}

// loadTelemetryCapture reads and vets a capture file.
func loadTelemetryCapture(path string) (*telemetryCapture, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("telemetry: no capture path")
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("telemetry: read capture: %w", err)
	}
	var capture telemetryCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		return nil, fmt.Errorf("telemetry: capture is not JSON: %w", err)
	}
	if capture.SchemaVersion != telemetrySchemaVersion {
		return nil, fmt.Errorf("telemetry: capture schema %d is not supported", capture.SchemaVersion)
	}
	usable := 0
	for _, env := range capture.Envelopes {
		if env.Body == "" || env.Path == "" {
			continue
		}
		if !allowedTelemetry(env.Path, env.Method) {
			return nil, fmt.Errorf("telemetry: %s %s is not a telemetry endpoint; refusing the capture", env.Method, env.Path)
		}
		usable++
	}
	if usable == 0 {
		return nil, errors.New("telemetry: capture carries no request bodies; refusing to invent events")
	}
	return &capture, nil
}

// replayPlan is what the session will actually send, already re-stamped.
type replayPlan struct {
	Envelopes []telemetryEnvelope
	Events    int
	Bytes     int
}

// planRebase re-stamps the captured batches to this session's clock. Array
// payloads (/v2/jslogs) get fresh event ids and timestamps in capture order;
// single-object payloads (/jslog) carry no timestamp and are sent verbatim.
func planRebase(capture *telemetryCapture, now time.Time) (*replayPlan, error) {
	plan := &replayPlan{}
	step := 250 * time.Millisecond
	for _, env := range capture.Envelopes {
		if env.Body == "" || env.Path == "" {
			continue
		}
		body := env.Body
		if strings.HasPrefix(strings.TrimSpace(body), "[") {
			var events []map[string]any
			if err := json.Unmarshal([]byte(body), &events); err != nil {
				return nil, fmt.Errorf("telemetry: %s payload is an array but not JSON events: %w", env.Path, err)
			}
			if len(events) == 0 {
				continue
			}
			for i, event := range events {
				// Only rewrite fields the capture already has: adding an eventId
				// or a timestamp to an event that never carried one would be
				// inventing shape, not replaying it.
				if _, ok := event["eventId"]; ok {
					event["eventId"] = newEventID()
				}
				if _, ok := event["timestamp"]; ok {
					event["timestamp"] = now.Add(time.Duration(i) * step).UnixMilli()
				}
			}
			encoded, err := json.Marshal(events)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
			plan.Events += len(events)
		}
		env.Body = body
		plan.Envelopes = append(plan.Envelopes, env)
		plan.Bytes += len(body)
	}
	if len(plan.Envelopes) == 0 {
		return nil, errors.New("telemetry: nothing usable in the capture")
	}
	return plan, nil
}

// sendTelemetry issues the plan. Failures are recorded, never fatal: telemetry
// is an extra signal, not part of the registration contract.
func (s *Session) sendTelemetry(ctx context.Context, plan *replayPlan, pacer *pacing.Pacer) map[string]any {
	result := map[string]any{"envelopes": len(plan.Envelopes), "events": plan.Events, "bytes": plan.Bytes}
	var failures []string
	sent := 0
	for _, env := range plan.Envelopes {
		if pacer != nil {
			if _, err := pacer.Before(ctx, "telemetry", "telemetry-send"); err != nil {
				failures = append(failures, "pacing")
				break
			}
		}
		header, err := s.headers(env.ContentType, false, time.Now())
		if err != nil {
			failures = append(failures, "headers")
			break
		}
		header.Set("Accept", "application/json, text/plain, */*")
		if env.Headers["accept-language"] != "" {
			header.Set("Accept-Language", env.Headers["accept-language"])
		}
		target := s.base + env.Path + env.Query
		status, respHeader, _, err := s.send(ctx, env.Method, target, header, []byte(env.Body))
		if err == nil {
			s.absorb(respHeader)
		}
		if err != nil || status < 200 || status > 299 {
			failures = append(failures, fmt.Sprintf("%s:%d", env.Path, status))
			continue
		}
		sent++
	}
	result["sent"] = sent
	result["ok"] = sent == len(plan.Envelopes)
	if len(failures) > 0 {
		result["failures"] = failures
	}
	return result
}

// newEventID is a UUIDv4, the shape the widget itself uses.
func newEventID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// telemetrySummary is the receipt block: provenance and counts, no bodies.
func telemetrySummary(capture *telemetryCapture, result map[string]any) map[string]any {
	out := map[string]any{
		"capturedAt": capture.CapturedAt,
		"target":     capture.Target,
		"urls":       capture.URLs,
	}
	for key, value := range result {
		out[key] = value
	}
	return out
}

var _ = http.MethodPost
