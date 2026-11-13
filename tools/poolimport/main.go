// Command poolimport imports mailbox addresses into the mailbox pool service.
//
// The pool exposes POST {base}/api/v1/mailbox-pool/import with an X-API-Key
// header. It treats request_id as an idempotency key, so this tool keeps a
// local ledger keyed by request_id:
//
//   - same request_id and same content hash: replay the recorded outcome, no
//     HTTP request is made;
//   - same request_id and different content: conflict, non-zero exit, the
//     ledger is left untouched;
//   - outcome unknown (timeout / transport error / 5xx / 408 / 429): the entry
//     stays pending and the next run resends the very same request_id instead
//     of inventing a new one.
//
// Because a failed or pending import is bound to its request_id, retrying with
// different content requires a new request_id; that is what keeps the server's
// idempotency key meaningful.
//
// Usage:
//
//	poolimport -base http://127.0.0.1:8788 -api-key ... batch.json
//	poolimport -base http://127.0.0.1:8788 -api-key ... -dry-run batch.json
//
// Input file:
//
//	{
//	  "source": "icloud-hme",
//	  "source_account_id": "acct-0001",
//	  "request_id": "hme-acct-0001-20261113",
//	  "items": [
//	    {"address": "abc@privaterelay.appleid.com", "alias": "netflix", "delivery_ref": "hme-1", "tag": "streaming"}
//	  ]
//	}
//
// A bare JSON array of items is also accepted; then -source,
// -source-account-id and -request-id must be given on the command line.
//
// The api key, cookies and any token value are never printed. Only request_id,
// item count, state and HTTP status go to stdout (plus the fully masked request
// in -dry-run).
//
// Exit codes: 0 done / idempotent replay / dry-run, 1 failed or outcome
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
	ledgerVersion    = 1

	maxResponseBytes = 1 << 20
	maxStoredResult  = 4 << 10

	statePending = "pending"
	stateDone    = "done"
	stateFailed  = "failed"

	exitOK       = 0
	exitFailure  = 1
	exitUsage    = 2
	exitConflict = 3
)

// item is one mailbox entry as accepted by the pool import endpoint.
type item struct {
	Address     string `json:"address"`
	Alias       string `json:"alias,omitempty"`
	DeliveryRef string `json:"delivery_ref,omitempty"`
	Tag         string `json:"tag,omitempty"`
}

// inputDoc is the on-disk input file. request_id doubles as the ledger key.
type inputDoc struct {
	Source          string `json:"source"`
	SourceAccountID string `json:"source_account_id"`
	RequestID       string `json:"request_id"`
	Items           []item `json:"items"`
}

// requestBody is the exact JSON document posted to the pool.
type requestBody struct {
	Source          string `json:"source"`
	SourceAccountID string `json:"source_account_id"`
	Items           []item `json:"items"`
	RequestID       string `json:"request_id"`
}

type ledgerEntry struct {
	RequestID     string `json:"request_id"`
	ContentSHA256 string `json:"content_sha256"`
	State         string `json:"state"`
	Attempts      int    `json:"attempts"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Result        string `json:"result,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

type ledger struct {
	Version int                     `json:"version"`
	Entries map[string]*ledgerEntry `json:"entries"`
}

type config struct {
	base            string
	apiKey          string
	statePath       string
	input           string
	source          string
	sourceAccountID string
	requestID       string
	timeout         time.Duration
	dryRun          bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `poolimport — 邮箱池导入（request_id 幂等）

用法:
  poolimport -base URL [-api-key KEY] [-state PATH] [-dry-run] FILE
  poolimport -base URL ... [-source S] [-source-account-id ID] [-request-id RID] -input FILE

参数:
  -base               池服务根地址，如 http://127.0.0.1:8788
  -api-key            X-API-Key；缺省读环境变量 POOL_API_KEY
  -state              本地幂等台账，默认 .cache/poolimport-state.json
  -input              输入 JSON；也可作为唯一位置参数传入
  -source             覆盖输入文件里的 source（裸数组输入时必填）
  -source-account-id  覆盖输入文件里的 source_account_id（裸数组输入时必填）
  -request-id         覆盖输入文件里的 request_id（裸数组输入时必填）
  -timeout            单次请求超时，默认 30s
  -dry-run            只打印将发送的请求（api key 隐藏），不写台账、不发请求

退出码: 0 成功/幂等返回/演练, 1 失败或结果未知, 2 用法或输入错误, 3 request_id 冲突
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
	flags.StringVar(&cfg.source, "source", "", "覆盖 source")
	flags.StringVar(&cfg.sourceAccountID, "source-account-id", "", "覆盖 source_account_id")
	flags.StringVar(&cfg.requestID, "request-id", "", "覆盖 request_id")
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
	applyOverrides(doc, cfg)
	items, err := normalizeItems(doc.Items)
	if err != nil {
		return usageError(stderr, err)
	}
	body := requestBody{
		Source:          strings.TrimSpace(doc.Source),
		SourceAccountID: strings.TrimSpace(doc.SourceAccountID),
		Items:           items,
		RequestID:       strings.TrimSpace(doc.RequestID),
	}
	if err := validateBody(body); err != nil {
		return usageError(stderr, err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return usageError(stderr, fmt.Errorf("编码请求体失败: %w", err))
	}
	sum := sha256.Sum256(encoded)
	contentSHA := hex.EncodeToString(sum[:])

	if !cfg.dryRun && cfg.apiKey == "" {
		return usageError(stderr, errors.New("缺少 api key：-api-key 或环境变量 POOL_API_KEY"))
	}

	ld, err := loadLedger(cfg.statePath)
	if err != nil {
		return usageError(stderr, err)
	}
	entry := ld.Entries[body.RequestID]
	plan := classify(entry, contentSHA)

	if cfg.dryRun {
		return reportDryRun(stdout, stderr, cfg, endpoint, body, encoded, plan)
	}

	switch plan {
	case planConflict:
		printStatus(stdout, body.RequestID, len(body.Items), "conflict", 0, false)
		fmt.Fprintf(stderr, "conflict: request_id %s 已按不同内容登记，台账未改动\n", body.RequestID)
		return exitConflict
	case planCached:
		printStatus(stdout, body.RequestID, len(body.Items), entry.State, entry.HTTPStatus, true)
		if entry.State == stateDone {
			return exitOK
		}
		return exitFailure
	}

	// Persist the pending entry before the request: if this process dies
	// mid-flight, the next run knows the outcome is unknown and replays the
	// same request_id instead of starting a fresh import.
	if entry == nil {
		entry = &ledgerEntry{RequestID: body.RequestID, ContentSHA256: contentSHA}
		ld.Entries[body.RequestID] = entry
	}
	entry.ContentSHA256 = contentSHA
	entry.State = statePending
	entry.Attempts++
	entry.HTTPStatus = 0
	entry.LastError = ""
	if err := record(cfg.statePath, ld, entry); err != nil {
		fmt.Fprintf(stderr, "poolimport: 台账写入失败，未发送请求: %v\n", err)
		return exitFailure
	}

	result := post(cfg, endpoint, encoded)
	entry.HTTPStatus = result.status
	if result.err != nil {
		entry.State = statePending
		entry.LastError = redact(result.err.Error(), cfg.apiKey)
		if err := record(cfg.statePath, ld, entry); err != nil {
			fmt.Fprintf(stderr, "poolimport: 台账写入失败: %v\n", err)
			return exitFailure
		}
		printStatus(stdout, body.RequestID, len(body.Items), statePending, 0, false)
		fmt.Fprintf(stderr, "poolimport: %s；已记 pending，下次运行用同一 request_id 重放（attempt=%d）\n", entry.LastError, entry.Attempts)
		return exitFailure
	}

	entry.Result = truncate(result.body, maxStoredResult)
	entry.LastError = ""
	switch {
	case result.status >= 200 && result.status < 300:
		entry.State = stateDone
	case result.status == http.StatusRequestTimeout, result.status == http.StatusTooManyRequests, result.status >= 500:
		// The server answered, but the import outcome is not a verdict.
		entry.State = statePending
		entry.LastError = fmt.Sprintf("server returned HTTP %d", result.status)
	default:
		entry.State = stateFailed
	}
	if err := record(cfg.statePath, ld, entry); err != nil {
		fmt.Fprintf(stderr, "poolimport: 台账写入失败: %v\n", err)
		return exitFailure
	}
	printStatus(stdout, body.RequestID, len(body.Items), entry.State, entry.HTTPStatus, false)
	if entry.State != stateDone {
		fmt.Fprintf(stderr, "poolimport: request_id %s 未成功（state=%s），未重发；确需重试请换新的 request_id\n", body.RequestID, entry.State)
		return exitFailure
	}
	return exitOK
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
		return "resend-pending"
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

func reportDryRun(stdout, stderr io.Writer, cfg *config, endpoint string, body requestBody, encoded []byte, p plan) int {
	fmt.Fprintf(stdout, "dry-run request_id=%s items=%d state=%s url=%s\n", body.RequestID, len(body.Items), p, endpoint)
	keyHeader := "(unset)"
	if cfg.apiKey != "" {
		keyHeader = "***"
	}
	fmt.Fprintf(stdout, "dry-run headers: Content-Type: application/json; X-API-Key: %s\n", keyHeader)
	fmt.Fprintf(stdout, "dry-run body: %s\n", encoded)
	if p == planConflict {
		fmt.Fprintf(stderr, "conflict: request_id %s 已按不同内容登记，台账未改动\n", body.RequestID)
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
	u.RawQuery = ""
	u.Fragment = ""
	endpoint := strings.TrimRight(u.String(), "/")
	if strings.HasSuffix(endpoint, importPath) {
		return endpoint, nil
	}
	return endpoint + importPath, nil
}

func readInput(path string) (*inputDoc, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("缺少输入文件：用 -input FILE 或位置参数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取输入文件失败: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("输入文件为空")
	}
	doc := &inputDoc{}
	if trimmed[0] == '[' {
		if err := decodeStrict(data, &doc.Items); err != nil {
			return nil, fmt.Errorf("解析输入文件失败: %w", err)
		}
		return doc, nil
	}
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

func applyOverrides(doc *inputDoc, cfg *config) {
	if v := strings.TrimSpace(cfg.source); v != "" {
		doc.Source = v
	}
	if v := strings.TrimSpace(cfg.sourceAccountID); v != "" {
		doc.SourceAccountID = v
	}
	if v := strings.TrimSpace(cfg.requestID); v != "" {
		doc.RequestID = v
	}
}

func normalizeItems(items []item) ([]item, error) {
	out := make([]item, len(items))
	for i, it := range items {
		out[i] = item{
			Address:     strings.TrimSpace(it.Address),
			Alias:       strings.TrimSpace(it.Alias),
			DeliveryRef: strings.TrimSpace(it.DeliveryRef),
			Tag:         strings.TrimSpace(it.Tag),
		}
		if out[i].Address == "" {
			return nil, fmt.Errorf("items[%d] 缺少 address", i)
		}
	}
	return out, nil
}

func validateBody(body requestBody) error {
	switch {
	case body.Source == "":
		return errors.New("缺少 source：输入文件字段或 -source")
	case body.SourceAccountID == "":
		return errors.New("缺少 source_account_id：输入文件字段或 -source-account-id")
	case body.RequestID == "":
		return errors.New("缺少 request_id：输入文件字段或 -request-id")
	case len(body.Items) == 0:
		return errors.New("items 为空")
	}
	return nil
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
		return nil, fmt.Errorf("台账版本不支持（%d），未做任何改动", ld.Version)
	}
	if ld.Entries == nil {
		ld.Entries = map[string]*ledgerEntry{}
	}
	for id, e := range ld.Entries {
		if e.RequestID == "" {
			e.RequestID = id
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

func printStatus(w io.Writer, requestID string, items int, state string, status int, cached bool) {
	line := fmt.Sprintf("request_id=%s items=%d state=%s http=%d", requestID, items, state, status)
	if cached {
		line += " cached=true"
	}
	fmt.Fprintln(w, line)
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
