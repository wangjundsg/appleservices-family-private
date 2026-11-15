// curl transport: the escape hatch for Apple's edge.
//
// Measured 2026-11-15: `GET /account?countryCode=USA` answers 200 to curl and
// 404 (www.apple.com/errors/us_error.html) to Go's crypto/tls client, from the
// same exit IP, with the same headers and both HTTP versions. That is a TLS
// fingerprint block, not an IP or header rule. Instead of dragging uTLS into the
// module, the client can hand the wire to curl, which speaks OpenSSL and passes.
//
// Enabled with -curl. Cookies still live in the session's cookie jar: they are
// serialized into a Netscape cookie file per call and read back afterwards, so
// scnt/session chaining behaves exactly like the pure-Go path.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cookieFile returns the on-disk jar curl reads and writes.
func (s *Session) cookieFile() string {
	if s.curlJar == "" {
		dir := filepath.Join(os.TempDir(), "signup-curl")
		_ = os.MkdirAll(dir, 0o700)
		s.curlJar = filepath.Join(dir, fmt.Sprintf("jar-%d.txt", os.Getpid()))
	}
	return s.curlJar
}

// curlRoundTrip performs one request through curl.exe and normalizes the reply
// into the same shape net/http would hand back.
func (s *Session) curlRoundTrip(ctx context.Context, method, url string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	dir, err := os.MkdirTemp("", "signup-curl-call")
	if err != nil {
		return 0, nil, nil, err
	}
	defer os.RemoveAll(dir)
	headerPath := filepath.Join(dir, "headers.txt")
	bodyPath := filepath.Join(dir, "body.bin")

	args := []string{
		"-sS", "--max-time", "45",
		"-X", method,
		"-D", headerPath,
		"-o", bodyPath,
		"-A", s.userAgent,
		"-b", s.cookieFile(),
		"-c", s.cookieFile(),
	}
	if s.proxyURL != "" {
		args = append(args, "--proxy", s.proxyURL)
	}
	for key, values := range header {
		for _, value := range values {
			args = append(args, "-H", key+": "+value)
		}
	}
	if body != nil {
		payloadPath := filepath.Join(dir, "payload.json")
		if err := os.WriteFile(payloadPath, body, 0o600); err != nil {
			return 0, nil, nil, err
		}
		args = append(args, "--data-binary", "@"+payloadPath)
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, "curl.exe", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, nil, fmt.Errorf("curl: %v: %s", err, strings.TrimSpace(stderr.String()))
	}

	rawHeaders, err := os.ReadFile(headerPath)
	if err != nil {
		return 0, nil, nil, err
	}
	status, respHeader, err := parseCurlHeaders(string(rawHeaders))
	if err != nil {
		return 0, nil, nil, err
	}
	respBody, err := os.ReadFile(bodyPath)
	if err != nil && !os.IsNotExist(err) {
		return 0, nil, nil, err
	}
	return status, respHeader, respBody, nil
}

// parseCurlHeaders keeps the last response block (redirects produce several)
// and turns it into a status code plus http.Header.
func parseCurlHeaders(raw string) (int, http.Header, error) {
	blocks := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n")
	var last string
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.TrimSpace(blocks[i]) != "" {
			last = blocks[i]
			break
		}
	}
	lines := strings.Split(strings.TrimSpace(last), "\n")
	if len(lines) == 0 {
		return 0, nil, fmt.Errorf("curl: empty header block")
	}
	var status int
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "HTTP/%*s %d", &status); err != nil {
		return 0, nil, fmt.Errorf("curl: unparsable status line %q", lines[0])
	}
	header := http.Header{}
	for _, line := range lines[1:] {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		header.Add(key, value)
	}
	return status, header, nil
}

// drain keeps io imported for the shared signature without changing the caller.
var _ = io.Discard
