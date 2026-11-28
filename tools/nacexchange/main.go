// Public NAC certificate/session exchange, using the same verified TLS and
// SOCKS policy as GSA. No Apple account credentials are accepted by this tool.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Laky-64/appleservices/gsa"
	"howett.net/plist"
)

func useIPv4(transport *http.Transport) {
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network == "tcp" {
			network = "tcp4"
		}
		return dial(ctx, network, address)
	}
}

func run() error {
	op := flag.String("op", "", "cert or initialize")
	ipv4 := flag.Bool("ipv4", false, "use IPv4 for this NAC exchange without changing proxy or TLS verification")
	input := flag.String("in", "", "private NAC request plist for initialize")
	output := flag.String("out", "", "private response plist")
	flag.Parse()
	if *output == "" {
		return errors.New("private output required")
	}
	endpoint, method := "https://static.ess.apple.com/identity/validation/cert-1.0.plist", http.MethodGet
	var body []byte
	switch *op {
	case "cert":
	case "initialize":
		endpoint, method = "https://identity.ess.apple.com/WebObjects/TDIdentityService.woa/wa/initializeValidation", http.MethodPost
		var err error
		body, err = os.ReadFile(*input)
		if err != nil {
			return errors.New("private request unavailable")
		}
		var request map[string]any
		if _, err := plist.Unmarshal(body, &request); err != nil {
			return errors.New("NAC request is not a plist")
		}
		data, _ := request["session-info-request"].([]byte)
		if len(data) == 0 || len(data) > 65536 || len(request) != 1 {
			return errors.New("invalid NAC session request")
		}
	default:
		return errors.New("unsupported NAC operation")
	}
	transport := gsa.AppleRootTransport().(*http.Transport)
	if *ipv4 {
		useIPv4(transport)
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 30 * time.Second}
	req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot build NAC request")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-apple-plist")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("NAC verified TLS request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("NAC HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return errors.New("cannot read NAC response")
	}
	var decoded map[string]any
	if _, err := plist.Unmarshal(data, &decoded); err != nil {
		return errors.New("NAC response is not a plist")
	}
	if err := os.WriteFile(*output, data, 0600); err != nil {
		return errors.New("cannot save private NAC response")
	}
	fmt.Printf("NAC %s HTTP 200; response-bytes=%d\n", *op, len(data))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
