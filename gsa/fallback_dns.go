package gsa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Only the public GSA hostname goes to the recovery resolver. Account data stays
// on the original Apple request, whose Host, SNI and certificate checks survive.
func resolveGSAFallback(ctx context.Context, transport http.RoundTripper) ([]netip.Addr, error) {
	return resolveGSAFallbackWithResolver(ctx, transport, net.DefaultResolver.LookupNetIP)
}

func resolveGSAFallbackWithResolver(ctx context.Context, transport http.RoundTripper, lookup func(context.Context, string, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	// Refresh the configured system DNS first; external DoH may be unreachable
	// on the same direct route on which Apple itself is available.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, 3*time.Second)
	resolved, lookupErr := lookup(lookupCtx, "ip4", "gsa.apple.com")
	cancelLookup()
	if lookupErr == nil {
		var addresses []netip.Addr
		for _, address := range resolved {
			if validGSAAddress(address) {
				addresses = append(addresses, address)
			}
			if len(addresses) == 2 {
				break
			}
		}
		if len(addresses) > 0 {
			return addresses, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://dns.google/resolve?name=gsa.apple.com&type=A", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("GSA recovery DNS unavailable")
	}
	return parseGSAFallbackDNS(io.LimitReader(resp.Body, 64<<10))
}

func parseGSAFallbackDNS(body io.Reader) ([]netip.Addr, error) {
	var reply struct {
		Status *int
		Answer []struct {
			Type int
			Data string
		}
	}
	if err := json.NewDecoder(body).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Status == nil || *reply.Status != 0 {
		return nil, errors.New("GSA recovery DNS unsuccessful")
	}
	var addresses []netip.Addr
	for _, answer := range reply.Answer {
		if answer.Type != 1 {
			continue
		}
		address, err := netip.ParseAddr(answer.Data)
		if err != nil || !validGSAAddress(address) {
			continue
		}
		duplicate := false
		for _, existing := range addresses {
			if existing == address {
				duplicate = true
				break
			}
		}
		if !duplicate {
			addresses = append(addresses, address)
		}
		if len(addresses) == 2 {
			break
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("GSA recovery DNS has no public address")
	}
	return addresses, nil
}

func validGSAAddress(address netip.Addr) bool {
	return address.Is4() && address.IsGlobalUnicast() && !address.IsPrivate() && !netip.MustParsePrefix("198.18.0.0/15").Contains(address)
}
