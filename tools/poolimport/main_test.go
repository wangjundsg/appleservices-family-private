package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testRID = "6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6f"

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

func writeBatch(t *testing.T, path, requestID string, items ...item) {
	t.Helper()
	writeJSON(t, path, inputDoc{AppleAccountID: "acct-0001", RequestID: requestID, Items: items})
}

func assertNoSecret(t *testing.T, output, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(output, secret) {
		t.Fatalf("output leaked the api key: %q", output)
	}
}

func readLedgerEntry(t *testing.T, statePath, endpoint, requestID string) *ledgerEntry {
	t.Helper()
	ld, err := loadLedger(statePath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	entry := ld.Entries[ledgerKey(endpoint, requestID)]
	if entry == nil {
		t.Fatalf("ledger has no entry for %s at %s: %+v", requestID, endpoint, ld.Entries)
	}
	return entry
}

// startPool runs a fake pool endpoint and records every request body.
func startPool(t *testing.T, reply func(w http.ResponseWriter, req requestBody)) (*httptest.Server, *atomic.Int32, chan requestBody) {
	t.Helper()
	var hits atomic.Int32
	bodies := make(chan requestBody, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server could not decode body: %v", err)
		}
		hits.Add(1)
		bodies <- body
		reply(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, bodies
}

// statusResults builds one successful item result per request item.
func statusResults(items []item, outcome string) []itemResult {
	out := make([]itemResult, len(items))
	for i, it := range items {
		out[i] = itemResult{BaseAddress: it.BaseAddress, Success: true, Code: "OK", HTTPStatus: 200, Outcome: outcome}
	}
	return out
}

// receiptFor renders a receipt the way the pool does, counts included.
func receiptFor(t *testing.T, requestID string, results []itemResult) string {
	t.Helper()
	var imported, unchanged, failed int
	for _, r := range results {
		switch {
		case !r.Success:
			failed++
		case r.Outcome == "unchanged":
			unchanged++
		default:
			imported++
		}
	}
	body, err := json.Marshal(struct {
		RequestID string       `json:"request_id"`
		Imported  int          `json:"imported"`
		Unchanged int          `json:"unchanged"`
		Failed    int          `json:"failed"`
		Results   []itemResult `json:"results"`
	}{requestID, imported, unchanged, failed, results})
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	return string(body)
}

func writeReceipt(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func TestValidUUIDv4(t *testing.T) {
	valid := []string{
		testRID,
		"00000000-0000-4000-8000-000000000000",
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
		"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", // upper case is a legal spelling
	}
	for _, id := range valid {
		if !validUUIDv4(id) {
			t.Errorf("validUUIDv4(%q) = false, want true", id)
		}
	}
	invalid := []string{
		"",
		"rid-first",                             // the old free-form key
		"6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6",   // too short
		"6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6ff", // too long
		"6f1f0b52-5c4e-1a1e-9a3f-1d2c3b4a5e6f",  // version 1
		"6f1f0b52-5c4e-4a1e-0a3f-1d2c3b4a5e6f",  // variant 0
		"6f1f0b52x5c4e-4a1e-9a3f-1d2c3b4a5e6f",  // separator
		"6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6g",  // non hex
		"{6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6f}",
	}
	for _, id := range invalid {
		if validUUIDv4(id) {
			t.Errorf("validUUIDv4(%q) = true, want false", id)
		}
	}
}

func boxes(n int) []item {
	out := make([]item, n)
	for i := range out {
		out[i] = item{BaseAddress: "box" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@privaterelay.appleid.com"}
	}
	return out
}

func TestBuildRequestItemLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   []item
		wantErr string
	}{
		{"zero", nil, "items 为空"},
		{"one", boxes(1), ""},
		{"hundred", boxes(100), ""},
		{"hundred-and-one", boxes(101), "超过服务端上限"},
	} {
		doc := &inputDoc{AppleAccountID: "acct-1", RequestID: testRID, Items: tc.items}
		body, err := buildRequest(doc)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Fatalf("%s: expected an error containing %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.wantErr)
		}
		if err == nil && len(body.Items) != len(tc.items) {
			t.Fatalf("%s: body has %d items, want %d", tc.name, len(body.Items), len(tc.items))
		}
	}
}

func TestBuildRequestRejectsDuplicateBaseAddress(t *testing.T) {
	doc := &inputDoc{
		AppleAccountID: "acct-1",
		RequestID:      testRID,
		Items: []item{
			{BaseAddress: "dup@privaterelay.appleid.com"},
			{BaseAddress: "  DUP@privaterelay.appleid.com "}, // same mailbox after the server's normalisation
		},
	}
	if _, err := buildRequest(doc); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("error = %v, want a duplicate base_address rejection", err)
	}
}

func TestBuildRequestRejectsNonUUIDRequestID(t *testing.T) {
	doc := &inputDoc{AppleAccountID: "acct-1", RequestID: "hme-acct-0001-20261113", Items: []item{{BaseAddress: "a@privaterelay.appleid.com"}}}
	if _, err := buildRequest(doc); err == nil || !strings.Contains(err.Error(), "UUID v4") {
		t.Fatalf("error = %v, want a UUID rejection", err)
	}
}

func TestTooManyItemsNeverReachesTheServer(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	items := boxes(101)
	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, items...)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitUsage, stderr.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0", hits.Load())
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("ledger written for an invalid batch (err=%v)", err)
	}
}

func TestHundredItemsImportInOneBatch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, bodies := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	items := boxes(100)
	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, items...)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
	body := <-bodies
	if len(body.Items) != 100 {
		t.Fatalf("server saw %d items, want 100", len(body.Items))
	}
	if !strings.Contains(stdout.String(), "state=done verdict=imported imported=100") {
		t.Errorf("stdout = %q, want a done/imported 100 line", stdout.String())
	}
}

func TestImportRecordsReceiptAndPerItemResults(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, _, bodies := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID,
		item{BaseAddress: "A@privaterelay.appleid.com", TagAddress: "A+netflix@privaterelay.appleid.com"},
		item{BaseAddress: "b@privaterelay.appleid.com"},
	)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-base", srv.URL, "-api-key", "secret-key-first", "-state", statePath, input}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}

	body := <-bodies
	if body.RequestID != testRID || body.AppleAccountID != "acct-0001" {
		t.Errorf("request header fields = %+v", body)
	}
	if len(body.Items) != 2 || body.Items[0].BaseAddress != "a@privaterelay.appleid.com" ||
		body.Items[0].TagAddress != "a+netflix@privaterelay.appleid.com" {
		t.Errorf("request items were not normalised: %+v", body.Items)
	}

	combined := stdout.String() + stderr.String()
	assertNoSecret(t, combined, "secret-key-first")
	if !strings.Contains(stdout.String(), "request_id="+testRID+" items=2 state=done verdict=imported imported=2 unchanged=0 failed=0 http=200") {
		t.Errorf("stdout = %q, want the verdict line", stdout.String())
	}
	if !strings.Contains(stdout.String(), "base=a@privaterelay.appleid.com success=true code=OK http=200 outcome=imported") {
		t.Errorf("stdout = %q, want a per-item result line", stdout.String())
	}

	entry := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if entry.State != stateDone || entry.Verdict != verdictImported || entry.Attempts != 1 || entry.HTTPStatus != 200 {
		t.Errorf("ledger entry = %+v, want done/imported/1/200", entry)
	}
	if len(entry.Results) != 2 || entry.Imported != 2 {
		t.Errorf("ledger entry results = %+v, want 2 imported results", entry.Results)
	}
	if entry.ContentSHA256 == "" {
		t.Error("ledger entry has no content hash")
	}
	if _, err := os.Stat(lockPath(statePath)); !os.IsNotExist(err) {
		t.Errorf("lock file left behind (err=%v)", err)
	}
}

func TestCachedDoneDoesNotResend(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "unchanged")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "replay-key-A9", "-state", statePath, input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code != exitOK {
		t.Fatalf("first run exit = %d, stderr=%s", code, firstErr.String())
	}
	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitOK {
		t.Fatalf("second run exit = %d, stderr=%s", code, secondErr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (a cached verdict must not be resent)", hits.Load())
	}
	if !strings.Contains(secondOut.String(), "state=done verdict=unchanged") || !strings.Contains(secondOut.String(), "cached=true") {
		t.Errorf("second stdout = %q, want a cached done line", secondOut.String())
	}
	assertNoSecret(t, firstOut.String()+firstErr.String()+secondOut.String()+secondErr.String(), "replay-key-A9")
}

func TestPartialSuccessKeepsPerItemResults(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, []itemResult{
			{BaseAddress: "a@privaterelay.appleid.com", Success: true, Code: "OK", HTTPStatus: 200, Outcome: "imported"},
			{BaseAddress: "b@privaterelay.appleid.com", Success: false, Code: "POOL_BASE_NOT_FOUND", HTTPStatus: 404, Outcome: "rejected"},
		}))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID,
		item{BaseAddress: "a@privaterelay.appleid.com"},
		item{BaseAddress: "b@privaterelay.appleid.com"},
	)
	args := []string{"-base", srv.URL, "-api-key", "partial-key", "-state", statePath, input}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=partial verdict=partial imported=1 unchanged=0 failed=1") {
		t.Errorf("stdout = %q, want a partial verdict line", stdout.String())
	}
	if !strings.Contains(stdout.String(), "base=b@privaterelay.appleid.com success=false code=POOL_BASE_NOT_FOUND http=404 outcome=rejected") {
		t.Errorf("stdout = %q, want the rejected item recorded", stdout.String())
	}
	if !strings.Contains(stderr.String(), "同一 request_id 会重放并返回原回执") {
		t.Errorf("stderr = %q, want the replay note", stderr.String())
	}

	entry := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if entry.State != statePartial || entry.Verdict != verdictPartial || len(entry.Results) != 2 {
		t.Errorf("ledger entry = %+v, want partial with both item results", entry)
	}

	var cachedOut, cachedErr bytes.Buffer
	if code := run(args, &cachedOut, &cachedErr); code != exitFailure {
		t.Fatalf("cached exit = %d, want %d", code, exitFailure)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (a partial receipt is a verdict)", hits.Load())
	}
	if !strings.Contains(cachedOut.String(), "cached=true") {
		t.Errorf("cached stdout = %q, want cached=true", cachedOut.String())
	}
}

func TestReceiptWithoutResultsIsUnknownAndReplays(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, `{"request_id":"`+req.RequestID+`","imported":1,"unchanged":0,"failed":0}`)
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "noresults-key", "-state", statePath, input}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=pending verdict=unknown") {
		t.Errorf("stdout = %q, want state=pending verdict=unknown", stdout.String())
	}
	if !strings.Contains(stderr.String(), "回执缺少 results 数组") {
		t.Errorf("stderr = %q, want the missing-results reason", stderr.String())
	}
	entry := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if entry.State != statePending || entry.Verdict != verdictUnknown {
		t.Fatalf("ledger entry = %+v, want pending/unknown", entry)
	}

	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitFailure {
		t.Fatalf("second exit = %d, want %d", code, exitFailure)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2 (an unknown outcome replays the same key)", hits.Load())
	}
	assertNoSecret(t, stdout.String()+stderr.String()+secondOut.String()+secondErr.String(), "noresults-key")
}

func TestAccepted202IsUnknown(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusAccepted, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "accepted-key", "-state", statePath, input}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=pending verdict=unknown") {
		t.Errorf("stdout = %q, want pending/unknown for 202", stdout.String())
	}
	entry := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if entry.State != statePending {
		t.Fatalf("ledger entry = %+v, want pending", entry)
	}
	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitFailure {
		t.Fatalf("second exit = %d, want %d", code, exitFailure)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2", hits.Load())
	}
}

func TestEmptyBodyIsUnknown(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		w.WriteHeader(http.StatusOK)
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "empty-key", "-state", statePath, input}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=pending verdict=unknown") {
		t.Errorf("stdout = %q, want pending/unknown", stdout.String())
	}
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("second exit = %d, want %d", code, exitFailure)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2", hits.Load())
	}
}

func TestReceiptRequestIDMismatchIsUnknown(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, _, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, "00000000-0000-4000-8000-000000000000", statusResults(req.Items, "imported")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "request_id 与请求不一致") {
		t.Errorf("stderr = %q, want the request_id mismatch reason", stderr.String())
	}
}

func TestServerRejectionMarksFailedWithoutResend(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusBadRequest, `{"error":"POOL_INVALID_REQUEST"}`)
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "failed-key-D1", "-state", statePath, input}

	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stdout.String(), "state=failed verdict=failed") {
		t.Errorf("stdout = %q, want failed verdict", stdout.String())
	}
	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitFailure {
		t.Fatalf("second exit = %d, want %d", code, exitFailure)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (a business rejection is never resent)", hits.Load())
	}
	if !strings.Contains(secondOut.String(), "cached=true") {
		t.Errorf("second stdout = %q, want cached=true", secondOut.String())
	}
	entry := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if entry.State != stateFailed || entry.HTTPStatus != http.StatusBadRequest {
		t.Errorf("ledger entry = %+v, want failed/400", entry)
	}
	assertNoSecret(t, stdout.String()+stderr.String()+secondOut.String()+secondErr.String(), "failed-key-D1")
}

func TestEndpointSwitchDoesNotHitCache(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	reply := func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	}
	srvA, hitsA, _ := startPool(t, reply)
	srvB, hitsB, _ := startPool(t, reply)

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})

	for _, base := range []string{srvA.URL, srvB.URL, srvA.URL} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-base", base, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr); code != exitOK {
			t.Fatalf("run against %s exit = %d; stderr=%s", base, code, stderr.String())
		}
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("hits = %d/%d, want 1/1 (a new service must not reuse the old verdict)", hitsA.Load(), hitsB.Load())
	}

	ld, err := loadLedger(statePath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	if len(ld.Entries) != 2 {
		t.Fatalf("ledger entries = %d, want one per endpoint: %+v", len(ld.Entries), ld.Entries)
	}
	seenDigests := map[string]bool{}
	for key, entry := range ld.Entries {
		if !strings.HasPrefix(key, entry.Endpoint+" ") {
			t.Errorf("ledger key %q is not bound to endpoint %q", key, entry.Endpoint)
		}
		if seenDigests[entry.ContentSHA256] {
			t.Errorf("two endpoints share content hash %s", entry.ContentSHA256)
		}
		seenDigests[entry.ContentSHA256] = true
	}
}

func TestConflictSameKeyDifferentContent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code != exitOK {
		t.Fatalf("first run exit = %d, stderr=%s", code, firstErr.String())
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	writeBatch(t, input, testRID,
		item{BaseAddress: "a@privaterelay.appleid.com"},
		item{BaseAddress: "b@privaterelay.appleid.com"},
	)
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != exitConflict {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitConflict, stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (a conflict must not send)", hits.Load())
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read ledger after conflict: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger changed on conflict:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestTimeoutKeepsPendingAndReplaysSameRequestID(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var answered atomic.Int32
	bodies := make(chan requestBody, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server could not decode body: %v", err)
		}
		bodies <- body
		if answered.Add(1) == 1 {
			// Outlive the client timeout so the first attempt ends unknown.
			time.Sleep(500 * time.Millisecond)
		}
		writeReceipt(w, http.StatusOK, receiptFor(t, body.RequestID, statusResults(body.Items, "imported")))
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})
	args := []string{"-base", srv.URL, "-api-key", "timeout-key-C3", "-state", statePath, "-timeout", "60ms", input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code == exitOK {
		t.Fatalf("first run exit = %d, want failure on timeout", code)
	}
	if !strings.Contains(firstOut.String(), "state=pending verdict=unknown") {
		t.Errorf("first stdout = %q, want pending/unknown", firstOut.String())
	}
	firstID := (<-bodies).RequestID
	if firstID != testRID {
		t.Fatalf("first request id = %q, want %q", firstID, testRID)
	}
	pending := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if pending.State != statePending || pending.Attempts != 1 {
		t.Fatalf("ledger entry after timeout = %+v, want pending/1", pending)
	}

	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitOK {
		t.Fatalf("replay exit = %d, stderr=%s", code, secondErr.String())
	}
	if secondID := (<-bodies).RequestID; secondID != firstID {
		t.Fatalf("request ids = %q then %q, want the same key on replay", firstID, secondID)
	}
	done := readLedgerEntry(t, statePath, srv.URL+importPath, testRID)
	if done.State != stateDone || done.Attempts != 2 || done.HTTPStatus != 200 {
		t.Errorf("ledger entry after replay = %+v, want done/2/200", done)
	}
	assertNoSecret(t, firstOut.String()+firstErr.String()+secondOut.String()+secondErr.String(), "timeout-key-C3")
}

func TestSecondInstanceIsLockedOut(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	unlock, err := acquireLock(lockPath(statePath), time.Now(), io.Discard)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer unlock()

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})

	var stdout, stderr bytes.Buffer
	code := run([]string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr)
	if code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "另一个 poolimport 实例正在运行") {
		t.Errorf("stderr = %q, want the lock-holder message", stderr.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0 while another instance holds the lock", hits.Load())
	}
}

func TestStaleLockIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	lock := lockPath(statePath)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload, err := json.Marshal(lockOwner{PID: 4242, StartedAt: time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("marshal lock: %v", err)
	}
	if err := os.WriteFile(lock, payload, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})
	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "a@privaterelay.appleid.com"})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-base", srv.URL, "-api-key", "k", "-state", statePath, input}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if !strings.Contains(stderr.String(), "接管陈旧锁") {
		t.Errorf("stderr = %q, want a stale-lock takeover note", stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
}

func TestDryRunSendsNothingAndWritesNoLedger(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	srv, hits, _ := startPool(t, func(w http.ResponseWriter, req requestBody) {
		writeReceipt(w, http.StatusOK, receiptFor(t, req.RequestID, statusResults(req.Items, "imported")))
	})

	input := filepath.Join(dir, "batch.json")
	writeBatch(t, input, testRID, item{BaseAddress: "dry@privaterelay.appleid.com", TagAddress: "dry+tag@privaterelay.appleid.com"})

	var stdout, stderr bytes.Buffer
	code := run([]string{"-base", srv.URL, "-api-key", "secret-key-dry", "-state", statePath, "-dry-run", input}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0 in dry-run", hits.Load())
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file exists after dry-run (err=%v)", err)
	}
	if _, err := os.Stat(lockPath(statePath)); !os.IsNotExist(err) {
		t.Fatalf("lock file exists after dry-run (err=%v)", err)
	}
	out := stdout.String()
	assertNoSecret(t, out+stderr.String(), "secret-key-dry")
	for _, want := range []string{
		"request_id=" + testRID,
		"items=1",
		"X-API-Key: ***",
		"dry-run body:",
		`"base_address":"dry@privaterelay.appleid.com"`,
		`"tag_address":"dry+tag@privaterelay.appleid.com"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run stdout missing %q: %s", want, out)
		}
	}
}
