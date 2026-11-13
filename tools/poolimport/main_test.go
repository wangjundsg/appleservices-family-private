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

// capturedRequest is what the fake pool saw, handed back over a channel so the
// test never reads handler state concurrently.
type capturedRequest struct {
	path string
	key  string
	body requestBody
}

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

func assertNoSecret(t *testing.T, output, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(output, secret) {
		t.Fatalf("output leaked the api key: %q", output)
	}
}

func readLedgerEntry(t *testing.T, statePath, requestID string) *ledgerEntry {
	t.Helper()
	ld, err := loadLedger(statePath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	entry := ld.Entries[requestID]
	if entry == nil {
		t.Fatalf("ledger has no entry for %s: %+v", requestID, ld.Entries)
	}
	return entry
}

func TestEndpointURL(t *testing.T) {
	ok := []struct{ base, want string }{
		{"http://127.0.0.1:8788", "http://127.0.0.1:8788" + importPath},
		{"http://127.0.0.1:8788/", "http://127.0.0.1:8788" + importPath},
		{"https://pool.example/prefix/", "https://pool.example/prefix" + importPath},
		{"https://pool.example" + importPath, "https://pool.example" + importPath},
	}
	for _, tc := range ok {
		got, err := endpointURL(tc.base)
		if err != nil {
			t.Errorf("endpointURL(%q) error = %v", tc.base, err)
			continue
		}
		if got != tc.want {
			t.Errorf("endpointURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}

	withCreds, err := endpointURL("https://user:secret@pool.example")
	if err != nil {
		t.Fatalf("endpointURL with userinfo: %v", err)
	}
	if strings.Contains(withCreds, "secret") || strings.Contains(withCreds, "user@") {
		t.Errorf("endpoint kept URL credentials: %q", withCreds)
	}

	for _, bad := range []string{"", "ftp://pool.example", "http://", "https://pool.example/x?a=1"} {
		if _, err := endpointURL(bad); err == nil {
			t.Errorf("endpointURL(%q) accepted an invalid base", bad)
		}
	}
}

func TestFirstImportSendsOnceAndMarksDone(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	requests := make(chan capturedRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := capturedRequest{path: r.URL.Path, key: r.Header.Get("X-API-Key")}
		if err := json.NewDecoder(r.Body).Decode(&req.body); err != nil {
			t.Errorf("server could not decode body: %v", err)
		}
		requests <- req
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"imported":2}`)
	}))
	defer srv.Close()

	t.Setenv("POOL_API_KEY", "secret-key-first")
	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0001",
		RequestID:       "rid-first",
		Items: []item{
			{Address: "a@privaterelay.appleid.com", Alias: "netflix", DeliveryRef: "hme-1", Tag: "streaming"},
			{Address: "b@privaterelay.appleid.com"},
		},
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"-base", srv.URL, "-state", statePath, input}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, exitOK, stderr.String())
	}

	got := <-requests
	if got.path != importPath {
		t.Errorf("path = %q, want %q", got.path, importPath)
	}
	if got.key != "secret-key-first" {
		t.Errorf("X-API-Key = %q, want value from POOL_API_KEY", got.key)
	}
	if got.body.RequestID != "rid-first" || got.body.Source != "icloud-hme" || got.body.SourceAccountID != "acct-0001" {
		t.Errorf("unexpected body header fields: %+v", got.body)
	}
	if len(got.body.Items) != 2 || got.body.Items[0].Address != "a@privaterelay.appleid.com" || got.body.Items[1].Alias != "" {
		t.Errorf("unexpected items: %+v", got.body.Items)
	}

	combined := stdout.String() + stderr.String()
	assertNoSecret(t, combined, "secret-key-first")
	if !strings.Contains(stdout.String(), "request_id=rid-first items=2 state=done http=200") {
		t.Errorf("stdout = %q, want request_id/items/state/http line", stdout.String())
	}

	entry := readLedgerEntry(t, statePath, "rid-first")
	if entry.State != stateDone || entry.Attempts != 1 || entry.HTTPStatus != 200 {
		t.Errorf("ledger entry = %+v, want done/1/200", entry)
	}
	if entry.ContentSHA256 == "" {
		t.Error("ledger entry has no content hash")
	}
}

func TestSameRequestIDSameContentDoesNotResend(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0002",
		RequestID:       "rid-replay",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
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
		t.Fatalf("server hits = %d, want 1 (idempotent replay must not resend)", hits.Load())
	}
	if !strings.Contains(secondOut.String(), "state=done") || !strings.Contains(secondOut.String(), "cached=true") {
		t.Errorf("second stdout = %q, want cached done line", secondOut.String())
	}
	assertNoSecret(t, firstOut.String()+firstErr.String()+secondOut.String()+secondErr.String(), "replay-key-A9")
}

func TestSameRequestIDDifferentContentConflicts(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0003",
		RequestID:       "rid-conflict",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
	args := []string{"-base", srv.URL, "-api-key", "conflict-key-B7", "-state", statePath, input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code != exitOK {
		t.Fatalf("first run exit = %d, stderr=%s", code, firstErr.String())
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	// Same request_id, different payload: the tool must refuse instead of
	// overwriting the recorded outcome.
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0003",
		RequestID:       "rid-conflict",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}, {Address: "b@privaterelay.appleid.com"}},
	})

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	if code != exitConflict {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", code, exitConflict, stdout.String(), stderr.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (conflict must not send)", hits.Load())
	}
	if !strings.Contains(stdout.String(), "state=conflict") {
		t.Errorf("stdout = %q, want state=conflict", stdout.String())
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read ledger after conflict: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("ledger changed on conflict:\nbefore=%s\nafter=%s", before, after)
	}
	entry := readLedgerEntry(t, statePath, "rid-conflict")
	if entry.Attempts != 1 || entry.State != stateDone {
		t.Errorf("ledger entry = %+v, want untouched done/1", entry)
	}
	assertNoSecret(t, stdout.String()+stderr.String(), "conflict-key-B7")
}

func TestTimeoutKeepsPendingAndReplaysSameRequestID(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	requestIDs := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server could not decode body: %v", err)
		}
		n := hits.Add(1)
		requestIDs <- body.RequestID
		if n == 1 {
			// Outlive the client timeout so the first attempt ends unknown.
			time.Sleep(500 * time.Millisecond)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0004",
		RequestID:       "rid-timeout",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
	args := []string{"-base", srv.URL, "-api-key", "timeout-key-C3", "-state", statePath, "-timeout", "60ms", input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code == exitOK {
		t.Fatalf("first run exit = %d, want failure on timeout (stdout=%s)", code, firstOut.String())
	}
	if !strings.Contains(firstOut.String(), "state=pending") {
		t.Errorf("first stdout = %q, want state=pending", firstOut.String())
	}
	// The handler reports before sleeping, so waiting here pins attempt #1 to
	// rid-timeout without depending on machine speed.
	firstID := <-requestIDs
	if firstID != "rid-timeout" {
		t.Fatalf("first request id = %q, want rid-timeout", firstID)
	}
	pending := readLedgerEntry(t, statePath, "rid-timeout")
	if pending.State != statePending || pending.Attempts != 1 {
		t.Fatalf("ledger entry after timeout = %+v, want pending/1", pending)
	}

	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitOK {
		t.Fatalf("replay exit = %d, stderr=%s", code, secondErr.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2 (timeout then replay)", hits.Load())
	}
	secondID := <-requestIDs
	if secondID != firstID {
		t.Fatalf("request ids = %q then %q, want the same rid-timeout on replay", firstID, secondID)
	}
	assertNoSecret(t, firstOut.String()+firstErr.String()+secondOut.String()+secondErr.String(), "timeout-key-C3")

	done := readLedgerEntry(t, statePath, "rid-timeout")
	if done.State != stateDone || done.Attempts != 2 || done.HTTPStatus != 200 {
		t.Errorf("ledger entry after replay = %+v, want done/2/200", done)
	}
}

func TestClientErrorMarksFailedAndIsNotResent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"bad items"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0005",
		RequestID:       "rid-failed",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
	args := []string{"-base", srv.URL, "-api-key", "failed-key-D1", "-state", statePath, input}

	var firstOut, firstErr bytes.Buffer
	if code := run(args, &firstOut, &firstErr); code != exitFailure {
		t.Fatalf("first run exit = %d, want %d", code, exitFailure)
	}
	var secondOut, secondErr bytes.Buffer
	if code := run(args, &secondOut, &secondErr); code != exitFailure {
		t.Fatalf("second run exit = %d, want %d (cached failure)", code, exitFailure)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (known failure is cached, not retried)", hits.Load())
	}
	if !strings.Contains(secondOut.String(), "cached=true") {
		t.Errorf("second stdout = %q, want cached line", secondOut.String())
	}
	assertNoSecret(t, firstOut.String()+firstErr.String()+secondOut.String()+secondErr.String(), "failed-key-D1")
	entry := readLedgerEntry(t, statePath, "rid-failed")
	if entry.State != stateFailed || entry.HTTPStatus != http.StatusBadRequest {
		t.Errorf("ledger entry = %+v, want failed/400", entry)
	}
}

func TestDifferentRequestIDSendsSeparately(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0007",
		RequestID:       "rid-batch-1",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
	args := []string{"-base", srv.URL, "-api-key", "distinct-key-E5", "-state", statePath, input}

	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != exitOK {
		t.Fatalf("first batch exit = %d, stderr=%s", code, errOut.String())
	}
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0007",
		RequestID:       "rid-batch-2",
		Items:           []item{{Address: "a@privaterelay.appleid.com"}},
	})
	out.Reset()
	errOut.Reset()
	if code := run(args, &out, &errOut); code != exitOK {
		t.Fatalf("second batch exit = %d, stderr=%s", code, errOut.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("server hits = %d, want 2 (idempotency is per request_id)", hits.Load())
	}
	ld, err := loadLedger(statePath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	for _, id := range []string{"rid-batch-1", "rid-batch-2"} {
		if entry := ld.Entries[id]; entry == nil || entry.State != stateDone {
			t.Errorf("ledger entry %s = %+v, want done", id, entry)
		}
	}
}

func TestDryRunSendsNothingAndWritesNoLedger(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	input := filepath.Join(dir, "batch.json")
	writeJSON(t, input, inputDoc{
		Source:          "icloud-hme",
		SourceAccountID: "acct-0006",
		RequestID:       "rid-dry",
		Items:           []item{{Address: "dry@privaterelay.appleid.com", Tag: "demo"}},
	})

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
	out := stdout.String()
	assertNoSecret(t, out+stderr.String(), "secret-key-dry")
	for _, want := range []string{"request_id=rid-dry", "items=1", "X-API-Key: ***", "dry-run body:", `"address":"dry@privaterelay.appleid.com"`} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run stdout missing %q: %s", want, out)
		}
	}
}
