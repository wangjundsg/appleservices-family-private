// Command poolimport imports mailbox addresses into the mailbox pool service.
//
// The pool exposes POST {base}/api/v1/mailbox-pool/import. request_id is the
// server's idempotency key: the same key replays the stored receipt instead of
// importing twice, so this tool keeps a local ledger bound to request_id AND to
// the normalised endpoint (scheme + host + path):
//
//   - same endpoint, same request_id, same body: print the recorded verdict, no
//     HTTP request is made;
//   - same endpoint, same request_id, different body: conflict, non-zero exit,
//     the ledger is left untouched;
//   - another endpoint never sees this endpoint's entry, so switching services
//     always sends.
//
// An outcome that is not a server verdict (transport error, timeout, 202, 408,
// 429, 5xx, missing/invalid receipt) keeps the entry pending and the next run
// resends the very same request_id. A 4xx rejection is a verdict and is never
// resent. The state machine is:
//
//	done     all items succeeded                    exit 0
//	partial  some items succeeded                   exit 1, per-item results kept
//	failed   the server rejected every item / 4xx   exit 1, not resent
//	pending  the outcome is unknown (verdict unknown) exit 1, replayed
//
// Usage:
//
//	poolimport -base http://127.0.0.1:8788 -api-key ... batch.json
//	poolimport -base http://127.0.0.1:8788 -api-key ... -dry-run batch.json
//
// Input file:
//
//	{
//	  "apple_account_id": "acct-0001",
//	  "request_id": "6f1f0b52-5c4e-4a1e-9a3f-1d2c3b4a5e6f",
//	  "items": [
//	    {"base_address": "abc@privaterelay.appleid.com", "tag_address": "abc+netflix@privaterelay.appleid.com"}
//	  ]
//	}
//
// The api key is never printed. Only request_id, item count, state, per-item
// results and HTTP status go to stdout.
//
// Exit codes: 0 done / cached done / dry-run, 1 failed, partial or outcome
// unknown, 2 usage or input error, 3 request_id conflict.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	importPath       = "/api/v1/mailbox-pool/import"
	defaultStatePath = ".cache/poolimport-state.json"
	defaultTimeout   = 30 * time.Second
	ledgerVersion    = 2

	maxItems         = 100
	maxResponseBytes = 1 << 20
	maxStoredResult  = 4 << 10

	// lockStaleAfter is how long a lock file may sit untouched before another
	// run may take it over. A running import holds it for one request only.
	lockStaleAfter = 10 * time.Minute

	statePending = "pending" // outcome unknown: the same request_id replays
	stateDone    = "done"
	statePartial = "partial"
	stateFailed  = "failed"

	verdictImported  = "imported"
	verdictUnchanged = "unchanged"
	verdictPartial   = "partial"
	verdictFailed    = "failed"
	verdictUnknown   = "unknown"

	replayNote = "同一 request_id 会重放并返回原回执；只有服务端确认失败后才可新建操作"

	exitOK       = 0
	exitFailure  = 1
	exitUsage    = 2
	exitConflict = 3
)

// item is one mailbox entry as accepted by the pool import endpoint.
type item struct {
	BaseAddress string `json:"base_address"`
	TagAddress  string `json:"tag_address,omitempty"`
}

// inputDoc is the on-disk input file. request_id doubles as the ledger key.
type inputDoc struct {
	AppleAccountID string `json:"apple_account_id"`
	RequestID      string `json:"request_id"`
	Items          []item `json:"items"`
}

// requestBody is the exact JSON document posted to the pool.
type requestBody struct {
	RequestID      string `json:"request_id"`
	AppleAccountID string `json:"apple_account_id"`
	Items          []item `json:"items"`
}

// itemResult is one entry of the server receipt's results array.
type itemResult struct {
	BaseAddress string `json:"base_address"`
	Success     bool   `json:"success"`
	Code        string `json:"code"`
	HTTPStatus  int    `json:"http_status"`
	Outcome     string `json:"outcome"`
}

type ledgerEntry struct {
	Endpoint      string       `json:"endpoint"`
	RequestID     string       `json:"request_id"`
	ContentSHA256 string       `json:"content_sha256"`
	State         string       `json:"state"`
	Verdict       string       `json:"verdict,omitempty"`
	Attempts      int          `json:"attempts"`
	HTTPStatus    int          `json:"http_status,omitempty"`
	Imported      int          `json:"imported"`
	Unchanged     int          `json:"unchanged"`
	Failed        int          `json:"failed"`
	Results       []itemResult `json:"results,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
	Result        string       `json:"result,omitempty"`
	UpdatedAt     string       `json:"updated_at"`
}

type ledger struct {
	Version int                     `json:"version"`
	Entries map[string]*ledgerEntry `json:"entries"`
}

type config struct {
	base      string
	apiKey    string
	statePath string
	input     string
	timeout   time.Duration
	dryRun    bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `poolimport — 邮箱池导入（request_id 幂等，失败按原键重放）

用法:
  poolimport -base URL [-api-key KEY] [-state PATH] [-timeout D] [-dry-run] FILE

参数:
  -base     池服务根地址，如 http://127.0.0.1:8788
  -api-key  X-API-Key；缺省读环境变量 POOL_API_KEY
  -state    本地幂等台账，默认 .cache/poolimport-state.json（同目录 .lock 保护）
  -input    输入 JSON；也可作为唯一位置参数传入
  -timeout  单次请求超时，默认 30s
  -dry-run  只打印将发送的请求（api key 隐藏），不写台账、不发请求

输入文件:
  {"apple_account_id": "...", "request_id": "<UUID v4>", "items": [{"base_address": "...", "tag_address": "..."}]}
  items 必须 1~100 条，同一批次 base_address 不得重复（服务端会整批 400）。

退出码: 0 全部成功/命中已完成台账/演练, 1 部分成功、失败或结果未知, 2 用法或输入错误, 3 request_id 冲突
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return usageError(stderr, err)
	}
	if cfg.apiKey == "" {
		cfg.apiKey = strings.TrimSpace(os.Getenv("POOL_API_KEY"))
	}
	if cfg.statePath == "" {
		cfg.statePath = defaultStatePath
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultTimeout
	}
	return execute(cfg, stdout, stderr)
}

func parseArgs(args []string, stderr io.Writer) (*config, error) {
	cfg := &config{}
	flags := flag.NewFlagSet("poolimport", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }
	flags.StringVar(&cfg.base, "base", "", "池服务根地址")
	flags.StringVar(&cfg.apiKey, "api-key", "", "X-API-Key（缺省读 POOL_API_KEY）")
	flags.StringVar(&cfg.statePath, "state", "", "本地幂等台账路径")
	flags.StringVar(&cfg.input, "input", "", "输入 JSON 文件")
	flags.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "单次请求超时")
	flags.BoolVar(&cfg.dryRun, "dry-run", false, "只打印将发送的请求")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	switch {
	case flags.NArg() == 1 && cfg.input == "":
		cfg.input = flags.Arg(0)
	case flags.NArg() == 1:
		return nil, errors.New("输入文件重复指定：既给了 -input 又给了位置参数")
	case flags.NArg() > 1:
		return nil, fmt.Errorf("-input 之外的多余参数: %s", strings.Join(flags.Args()[1:], " "))
	}
	return cfg, nil
}

func execute(cfg *config, stdout, stderr io.Writer) int {
	endpoint, err := endpointURL(cfg.base)
	if err != nil {
		return usageError(stderr, err)
	}
	doc, err := readInput(cfg.input)
	if err != nil {
		return usageError(stderr, err)
	}
	body, err := buildRequest(doc)
	if err != nil {
		return usageError(stderr, err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return usageError(stderr, fmt.Errorf("编码请求体失败: %w", err))
	}
	// The digest covers the endpoint too: the same request_id against another
	// pool is a different operation, never a cache hit.
	sum := sha256.Sum256(append([]byte(endpoint+"\n"), encoded...))
	contentSHA := hex.EncodeToString(sum[:])
	key := ledgerKey(endpoint, body.RequestID)

	if !cfg.dryRun && cfg.apiKey == "" {
		return usageError(stderr, errors.New("缺少 api key：-api-key 或环境变量 POOL_API_KEY"))
	}
	if cfg.dryRun {
		ld, loadErr := loadLedger(cfg.statePath)
		if loadErr != nil {
			return usageError(stderr, loadErr)
		}
		return reportDryRun(stdout, stderr, cfg, endpoint, body, encoded, classify(ld.Entries[key], contentSHA))
	}

	// The lock covers the whole read-decide-send-save cycle: two runs sharing a
	// ledger must not interleave an in-flight request with a cached verdict.
	unlock, lockErr := acquireLock(lockPath(cfg.statePath), time.Now(), stderr)
	if lockErr != nil {
		fmt.Fprintf(stderr, "poolimport: %v\n", lockErr)
		return exitFailure
	}
	defer unlock()

	ld, err := loadLedger(cfg.statePath)
	if err != nil {
		return usageError(stderr, err)
	}
	entry := ld.Entries[key]
	plan := classify(entry, contentSHA)

	if plan == planConflict {
		fmt.Fprintf(stdout, "request_id=%s items=%d state=conflict\n", body.RequestID, len(body.Items))
		fmt.Fprintf(stderr, "conflict: request_id %s 已在 %s 按不同内容登记，台账未改动\n", body.RequestID, endpoint)
		return exitConflict
	}
	if plan == planCached {
		printStatus(stdout, body.RequestID, len(body.Items), entry, true)
		printResults(stdout, entry.Results)
		if entry.State == stateDone {
			return exitOK
		}
		fmt.Fprintf(stderr, "poolimport: state=%s，未重发；%s\n", entry.State, replayNote)
		return exitFailure
	}

	// Persist the pending entry before the request: if this process dies
	// mid-flight, the next run knows the outcome is unknown and replays the
	// same request_id instead of starting a fresh import.
	if entry == nil {
		entry = &ledgerEntry{Endpoint: endpoint, RequestID: body.RequestID}
		ld.Entries[key] = entry
	}
	entry.ContentSHA256 = contentSHA
	entry.State = statePending
	entry.Verdict = verdictUnknown
	entry.Attempts++
	entry.HTTPStatus = 0
	entry.Imported, entry.Unchanged, entry.Failed = 0, 0, 0
	entry.Results = nil
	entry.LastError = ""
	if err := record(cfg.statePath, ld, entry); err != nil {
		fmt.Fprintf(stderr, "poolimport: 台账写入失败，未发送请求: %v\n", err)
		return exitFailure
	}

	result := post(cfg, endpoint, encoded)
	entry.HTTPStatus = result.status
	entry.Result = truncate(result.body, maxStoredResult)
	switch {
	case result.err != nil:
		entry.State = statePending
		entry.Verdict = verdictUnknown
		entry.LastError = redact(result.err.Error(), cfg.apiKey)
	case outcomeFor(result.status) == outcomeUnknown:
		entry.State = statePending
		entry.Verdict = verdictUnknown
		entry.LastError = fmt.Sprintf("服务端返回 HTTP %d，结果未知", result.status)
	case outcomeFor(result.status) == outcomeRejected:
		entry.State = stateFailed
		entry.Verdict = verdictFailed
		entry.LastError = fmt.Sprintf("服务端返回 HTTP %d，判定为业务失败", result.status)
	default:
		results, receiptErr := parseReceipt(result.body, body.RequestID, len(body.Items))
		if receiptErr != nil {
			entry.State = statePending
			entry.Verdict = verdictUnknown
			entry.LastError = fmt.Sprintf("回执不可用（HTTP %d）：%v", result.status, receiptErr)
			break
		}
		entry.Results = results
		entry.State, entry.Verdict, entry.Imported, entry.Unchanged, entry.Failed = summarize(results)
		entry.LastError = ""
	}
	if err := record(cfg.statePath, ld, entry); err != nil {
		fmt.Fprintf(stderr, "poolimport: 台账写入失败: %v\n", err)
		return exitFailure
	}

	printStatus(stdout, body.RequestID, len(body.Items), entry, false)
	printResults(stdout, entry.Results)
	if entry.State == stateDone {
		return exitOK
	}
	if entry.LastError != "" {
		fmt.Fprintf(stderr, "poolimport: %s\n", entry.LastError)
	}
	fmt.Fprintf(stderr, "poolimport: %s\n", replayNote)
	return exitFailure
}

type plan int

const (
	planSend plan = iota
	planReplay
	planCached
	planConflict
)

func (p plan) String() string {
	switch p {
	case planSend:
		return "send"
	case planReplay:
		return "replay-pending"
	case planCached:
		return "cached"
	case planConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

func classify(entry *ledgerEntry, contentSHA string) plan {
	switch {
	case entry == nil:
		return planSend
	case entry.ContentSHA256 != contentSHA:
		return planConflict
	case entry.State == statePending:
		return planReplay
	default:
		return planCached
	}
}

type transportOutcome int

const (
	outcomeReceipt transportOutcome = iota
	outcomeUnknown
	outcomeRejected
)

// outcomeFor maps a status code to what the client may conclude from it. 202 is
// an acknowledgement, not a verdict; 408 and 429 ask for the same key again.
func outcomeFor(status int) transportOutcome {
	switch {
	case status == http.StatusAccepted:
		return outcomeUnknown
	case status >= 200 && status < 300:
		return outcomeReceipt
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return outcomeUnknown
	case status >= 500:
		return outcomeUnknown
	case status >= 400:
		return outcomeRejected
	default:
		return outcomeUnknown
	}
}

// parseReceipt checks the pool receipt before any of it may become a local
// verdict: the request_id echo must match (the server lower-cases it), results
// must be a JSON array, and it must carry one entry per item.
func parseReceipt(body, requestID string, want int) ([]itemResult, error) {
	var receipt struct {
		RequestID string          `json:"request_id"`
		Results   json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &receipt); err != nil {
		return nil, fmt.Errorf("回执不是合法 JSON: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(receipt.RequestID), requestID) {
		return nil, errors.New("回执 request_id 与请求不一致")
	}
	raw := bytes.TrimSpace(receipt.Results)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, errors.New("回执缺少 results 数组")
	}
	var results []itemResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("回执 results 解析失败: %v", err)
	}
	if len(results) != want {
		return nil, fmt.Errorf("回执 results 条目数 %d 与请求的 %d 条不一致", len(results), want)
	}
	return results, nil
}

// summarize turns per-item results into the run verdict. A missing success is a
// rejected item; an outcome other than "unchanged" on a successful item counts
// as an import.
func summarize(results []itemResult) (state, verdict string, imported, unchanged, failed int) {
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
	switch {
	case failed == 0:
		state = stateDone
		if imported > 0 {
			verdict = verdictImported
		} else {
			verdict = verdictUnchanged
		}
	case imported+unchanged == 0:
		state = stateFailed
		verdict = verdictFailed
	default:
		state = statePartial
		verdict = verdictPartial
	}
	return state, verdict, imported, unchanged, failed
}

func reportDryRun(stdout, stderr io.Writer, cfg *config, endpoint string, body requestBody, encoded []byte, p plan) int {
	fmt.Fprintf(stdout, "dry-run request_id=%s items=%d plan=%s url=%s\n", body.RequestID, len(body.Items), p, endpoint)
	keyHeader := "(unset)"
	if cfg.apiKey != "" {
		keyHeader = "***"
	}
	fmt.Fprintf(stdout, "dry-run headers: Content-Type: application/json; X-API-Key: %s\n", keyHeader)
	fmt.Fprintf(stdout, "dry-run body: %s\n", encoded)
	if p == planConflict {
		fmt.Fprintf(stderr, "conflict: request_id %s 已在 %s 按不同内容登记，台账未改动\n", body.RequestID, endpoint)
		return exitConflict
	}
	return exitOK
}

type postResult struct {
	status int
	body   string
	err    error
}

func post(cfg *config, endpoint string, payload []byte) postResult {
	client := &http.Client{
		Timeout: cfg.timeout,
		// An import endpoint must not be followed elsewhere; a redirect would
		// also move the API key to a host we did not choose.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return postResult{err: fmt.Errorf("构造请求失败: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", cfg.apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return postResult{err: transportError(err, cfg.timeout)}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return postResult{status: resp.StatusCode, err: fmt.Errorf("读取响应失败，结果未知: %w", readErr)}
	}
	return postResult{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
}

func transportError(err error, timeout time.Duration) error {
	var deadline interface{ Timeout() bool }
	if errors.As(err, &deadline) && deadline.Timeout() {
		return fmt.Errorf("请求超时（%s 内无响应），结果未知", timeout)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Unwrap so the message never carries the full endpoint URL.
		return fmt.Errorf("网络错误，结果未知: %v", urlErr.Err)
	}
	return fmt.Errorf("请求失败，结果未知: %v", err)
}

// endpointURL returns the normalised import endpoint: scheme + host + path with
// no credentials, query or fragment. The ledger is keyed on it, so two spellings
// of the same service share one entry and two services never do.
func endpointURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("缺少 -base")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("-base 不是合法 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("-base 需要 http:// 或 https:// 前缀")
	}
	if u.Host == "" {
		return "", errors.New("-base 缺少主机名")
	}
	u.User = nil // never carry credentials from the base URL into requests or logs
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("-base 不应带查询串或 fragment")
	}
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = ""
	u.Fragment = ""
	endpoint := strings.TrimRight(u.String(), "/")
	if strings.HasSuffix(endpoint, importPath) {
		return endpoint, nil
	}
	return endpoint + importPath, nil
}

func ledgerKey(endpoint, requestID string) string { return endpoint + " " + requestID }

func lockPath(statePath string) string { return statePath + ".lock" }

func readInput(path string) (*inputDoc, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("缺少输入文件：用 -input FILE 或位置参数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取输入文件失败: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("输入文件为空")
	}
	doc := &inputDoc{}
	if err := decodeStrict(data, doc); err != nil {
		return nil, fmt.Errorf("解析输入文件失败: %w", err)
	}
	return doc, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON 之后还有多余内容")
	}
	return nil
}

// buildRequest validates and normalises the batch the way the server does:
// lower-cased addresses, 1~100 items, no duplicate base_address in one batch.
func buildRequest(doc *inputDoc) (requestBody, error) {
	body := requestBody{
		RequestID:      strings.TrimSpace(doc.RequestID),
		AppleAccountID: strings.TrimSpace(doc.AppleAccountID),
	}
	if body.RequestID == "" {
		return body, errors.New("缺少 request_id")
	}
	if !validUUIDv4(body.RequestID) {
		return body, fmt.Errorf("request_id %q 不是 UUID v4：服务端把它当幂等键，必须形如 xxxxxxxx-xxxx-4xxx-[89ab]xxx-xxxxxxxxxxxx", body.RequestID)
	}
	if body.AppleAccountID == "" {
		return body, errors.New("缺少 apple_account_id")
	}
	if len(doc.Items) < 1 {
		return body, errors.New("items 为空：至少 1 条")
	}
	if len(doc.Items) > maxItems {
		return body, fmt.Errorf("items %d 条超过服务端上限 %d 条", len(doc.Items), maxItems)
	}
	seen := make(map[string]bool, len(doc.Items))
	body.Items = make([]item, 0, len(doc.Items))
	for i, raw := range doc.Items {
		base := strings.ToLower(strings.TrimSpace(raw.BaseAddress))
		tag := strings.ToLower(strings.TrimSpace(raw.TagAddress))
		if base == "" {
			return body, fmt.Errorf("items[%d] 缺少 base_address", i)
		}
		if seen[base] {
			return body, fmt.Errorf("items[%d] 的 base_address %s 与本批次重复（服务端会整批 400）", i, base)
		}
		seen[base] = true
		body.Items = append(body.Items, item{BaseAddress: base, TagAddress: tag})
	}
	return body, nil
}

// validUUIDv4 reports whether id is a canonical UUID v4 (8-4-4-4-12 hex digits,
// version nibble 4, variant 10xx). Implemented here so the tool keeps its
// dependency set; the server accepts any canonical UUID but the pool issues v4.
func validUUIDv4(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch i {
		case 8, 13, 18, 23:
			if id[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(id[i]) {
				return false
			}
		}
	}
	return id[14] == '4' && strings.ContainsRune("89abAB", rune(id[19]))
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

type lockOwner struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// acquireLock takes the single-instance lock next to the ledger. O_EXCL makes
// the create the atomic test, so two runs cannot both win. A lock left behind by
// a dead run (older than lockStaleAfter) is taken over with a log line instead
// of blocking the import forever.
func acquireLock(path string, now time.Time, log io.Writer) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建台账目录失败: %w", err)
	}
	for attempt := 0; ; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			payload, _ := json.Marshal(lockOwner{PID: os.Getpid(), StartedAt: now.UTC().Format(time.RFC3339)})
			// A failed write only shortens the stale-detection window; the lock
			// itself already exists.
			_, _ = file.Write(payload)
			file.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("创建锁文件 %s 失败: %w", path, err)
		}
		owner, age := lockAge(path, now)
		if attempt > 0 || age <= lockStaleAfter {
			return nil, fmt.Errorf("另一个 poolimport 实例正在运行（锁 %s，pid=%d，起始于 %s）；本次未发送任何请求", path, owner.PID, owner.StartedAt)
		}
		fmt.Fprintf(log, "poolimport: 接管陈旧锁 %s（pid=%d 起始于 %s，已 %.0f 分钟，超过 %s）\n",
			path, owner.PID, owner.StartedAt, age.Minutes(), lockStaleAfter)
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("移除陈旧锁 %s 失败: %w", path, err)
		}
	}
}

func lockAge(path string, now time.Time) (lockOwner, time.Duration) {
	var owner lockOwner
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &owner)
	}
	if started, err := time.Parse(time.RFC3339, owner.StartedAt); err == nil {
		return owner, now.Sub(started)
	}
	if info, err := os.Stat(path); err == nil {
		return owner, now.Sub(info.ModTime())
	}
	return owner, 0 // unreadable lock: treat as fresh and refuse
}

func loadLedger(path string) (*ledger, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &ledger{Version: ledgerVersion, Entries: map[string]*ledgerEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取台账失败: %w", err)
	}
	var ld ledger
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ld); err != nil {
		return nil, fmt.Errorf("台账解析失败（%s），未做任何改动: %w", path, err)
	}
	if ld.Version != ledgerVersion {
		return nil, fmt.Errorf("台账版本不支持（%d，当前 %d）：换一个 -state 或先归档旧台账", ld.Version, ledgerVersion)
	}
	if ld.Entries == nil {
		ld.Entries = map[string]*ledgerEntry{}
	}
	for key, e := range ld.Entries {
		if e.RequestID == "" {
			e.RequestID = key
		}
	}
	return &ld, nil
}

func record(path string, ld *ledger, entry *ledgerEntry) error {
	entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return saveLedger(path, ld)
}

func saveLedger(path string, ld *ledger) error {
	data, err := json.MarshalIndent(ld, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".poolimport-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func printStatus(w io.Writer, requestID string, items int, entry *ledgerEntry, cached bool) {
	line := fmt.Sprintf("request_id=%s items=%d state=%s verdict=%s imported=%d unchanged=%d failed=%d http=%d",
		requestID, items, entry.State, entry.Verdict, entry.Imported, entry.Unchanged, entry.Failed, entry.HTTPStatus)
	if cached {
		line += " cached=true"
	}
	fmt.Fprintln(w, line)
}

func printResults(w io.Writer, results []itemResult) {
	for _, r := range results {
		fmt.Fprintf(w, "  base=%s success=%t code=%s http=%d outcome=%s\n", r.BaseAddress, r.Success, r.Code, r.HTTPStatus, r.Outcome)
	}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

func usageError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "poolimport: %v\n", err)
	return exitUsage
}
