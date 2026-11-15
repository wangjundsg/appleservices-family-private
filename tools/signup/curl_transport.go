// Optional system curl transport. Its TLS backend/capabilities depend on the
// installed executable (inspect curl --version); this is not Chrome emulation.
// Cookies use a separate curl jar, not the Go jar or a resumable session.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// cookieFile returns the on-disk jar curl reads and writes.
func (s *Session) cookieFile() string {
	if s.curlJar == "" {
		file, err := os.CreateTemp("", "signup-curl-jar-*.txt")
		if err != nil {
			return ""
		}
		s.curlJar = file.Name()
		_ = file.Close()
	}
	return s.curlJar
}

func (s *Session) Close() {
	s.client.CloseIdleConnections()
	if s.curlJar != "" {
		_ = os.Remove(s.curlJar)
		s.curlJar = ""
	}
}

// curlRoundTrip performs one request through curl.exe and normalizes the reply
// into the same shape net/http would hand back.
func (s *Session) curlRoundTrip(ctx context.Context, method, url string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	jar := s.cookieFile()
	if jar == "" {
		return 0, nil, nil, fmt.Errorf("curl: cannot create private cookie jar")
	}
	dir, err := os.MkdirTemp("", "signup-curl-call")
	if err != nil {
		return 0, nil, nil, err
	}
	defer os.RemoveAll(dir)
	headerPath := filepath.Join(dir, "headers.txt")
	bodyPath := filepath.Join(dir, "body.bin")

	args := []string{
		"-sS", "--max-time", strconv.FormatFloat(s.client.Timeout.Seconds(), 'f', 3, 64),
		"-X", method,
		"-D", headerPath,
		"-o", bodyPath,
		"-A", s.userAgent,
		"-b", jar,
		"-c", jar,
	}
	if s.HTTP2 {
		args = append(args, "--http2")
	} else {
		args = append(args, "--http1.1")
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
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, nil, fmt.Errorf("curl: unparsable status line %q", lines[0])
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil || status < 100 || status > 599 {
		return 0, nil, fmt.Errorf("curl: invalid HTTP status")
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
