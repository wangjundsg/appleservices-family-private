package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Laky-64/appleservices/internal/pacing"
)

func writeCapture(t *testing.T, payload map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.json")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	return path
}

func sampleCapture() map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"capturedAt":    time.Now().UTC().Format(time.RFC3339),
		"target":        "https://appleid.apple.com/account?countryCode=USA",
		"urls":          []string{"https://appleid.apple.com/jslog", "https://appleid.apple.com/v2/jslogs"},
		"envelopes": []map[string]any{
			{
				"at": 1, "method": "POST", "path": "/jslog", "query": "",
				"contentType": "application/json",
				"headers":     map[string]string{"accept": "application/json", "accept-language": "en-US"},
				"body":        `{"message":"account","title":"ROUTE CHANGED","type":"INFO","messageMap":{"ACTION":"FE_INFO"},"details":"{}"}`,
			},
			{
				"at": 2, "method": "POST", "path": "/v2/jslogs", "query": "",
				"contentType": "application/json",
				"headers":     map[string]string{"accept": "application/json", "accept-language": "en-US"},
				"body":        `[{"eventId":"11111111-1111-4111-8111-111111111111","timestamp":1,"eventType":"view","componentName":"account","action":"view","metadata":{"path":"account"}}]`,
			},
		},
	}
}

func TestTelemetryRefusesCaptureWithoutBodies(t *testing.T) {
	capture := sampleCapture()
	capture["envelopes"] = []map[string]any{{"path": "/jslog", "body": ""}}
	path := writeCapture(t, capture)
	if _, err := loadTelemetryCapture(path); err == nil {
		t.Fatal("a capture without bodies must be refused instead of sending nothing")
	}
}

func TestTelemetryRejectsUnknownSchema(t *testing.T) {
	capture := sampleCapture()
	capture["schemaVersion"] = 99
	path := writeCapture(t, capture)
	if _, err := loadTelemetryCapture(path); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("unknown schema must be rejected, got %v", err)
	}
}

func TestTelemetryRebaseKeepsShapeAndRestamps(t *testing.T) {
	path := writeCapture(t, sampleCapture())
	capture, err := loadTelemetryCapture(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Now()
	plan, err := planRebase(capture, now)
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	if len(plan.Envelopes) != 2 || plan.Events != 1 {
		t.Fatalf("plan = %d envelopes / %d events", len(plan.Envelopes), plan.Events)
	}

	// The single-object envelope is replayed verbatim.
	if !strings.Contains(plan.Envelopes[0].Body, "ROUTE CHANGED") {
		t.Fatalf("single envelope was rewritten: %s", plan.Envelopes[0].Body)
	}

	// The array envelope keeps its event shape, gets a fresh id and a new stamp.
	var events []map[string]any
	if err := json.Unmarshal([]byte(plan.Envelopes[1].Body), &events); err != nil {
		t.Fatalf("rebase produced invalid JSON: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count changed: %d", len(events))
	}
	if events[0]["eventId"] == "11111111-1111-4111-8111-111111111111" {
		t.Fatal("event id was not regenerated")
	}
	if !strings.HasPrefix(events[0]["eventId"].(string), "") || len(events[0]["eventId"].(string)) != 36 {
		t.Fatalf("event id is not a uuid: %v", events[0]["eventId"])
	}
	stamp := int64(events[0]["timestamp"].(float64))
	if stamp < now.UnixMilli()-1000 || stamp > now.Add(2*time.Second).UnixMilli() {
		t.Fatalf("timestamp %d was not rebased onto now=%d", stamp, now.UnixMilli())
	}
	if events[0]["eventType"] != "view" || events[0]["componentName"] != "account" {
		t.Fatalf("event fields were rewritten: %v", events[0])
	}
}

func TestTelemetryReplayHitsTheCapturedPaths(t *testing.T) {
	var mu sync.Mutex
	type seen struct {
		path, contentType, body string
	}
	var requests []seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		requests = append(requests, seen{r.URL.Path, r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer server.Close()

	session, err := newSession(SessionOptions{Base: server.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	capture, err := loadTelemetryCapture(writeCapture(t, sampleCapture()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	plan, err := planRebase(capture, time.Now())
	if err != nil {
		t.Fatalf("rebase: %v", err)
	}
	pacer := pacing.New(pacing.Config{Enabled: true, Min: 0, Max: 1, Seed: 1}, func(context.Context, time.Duration) error { return nil })
	result := session.sendTelemetry(context.Background(), plan, pacer)
	if result["ok"] != true {
		t.Fatalf("replay failed: %v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("posted %d envelopes, want 2", len(requests))
	}
	if requests[0].path != "/jslog" || requests[1].path != "/v2/jslogs" {
		t.Fatalf("paths = %v, %v", requests[0].path, requests[1].path)
	}
	for _, request := range requests {
		if request.contentType != "application/json" {
			t.Fatalf("%s content type = %q", request.path, request.contentType)
		}
		if request.body == "" {
			t.Fatalf("%s sent an empty body", request.path)
		}
	}
}

func TestTelemetryFailureDoesNotAbort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()
	session, err := newSession(SessionOptions{Base: server.URL, Locale: "en-US", Timezone: "America/New_York", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	capture, _ := loadTelemetryCapture(writeCapture(t, sampleCapture()))
	plan, _ := planRebase(capture, time.Now())
	result := session.sendTelemetry(context.Background(), plan, nil)
	if result["ok"] != false {
		t.Fatalf("a failing endpoint should be reported as not ok: %v", result)
	}
	if _, ok := result["failures"]; !ok {
		t.Fatalf("failures were not recorded: %v", result)
	}
}

func TestTelemetrySummaryHasNoBodies(t *testing.T) {
	capture, _ := loadTelemetryCapture(writeCapture(t, sampleCapture()))
	summary := telemetrySummary(capture, map[string]any{"sent": 2, "ok": true})
	raw, _ := json.Marshal(summary)
	for _, forbidden := range []string{"ROUTE CHANGED", "eventId", "11111111"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("summary leaks %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), "capturedAt") {
		t.Fatalf("summary lost provenance: %s", raw)
	}
}
