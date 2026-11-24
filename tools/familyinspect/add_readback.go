package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func familyContainsAccount(details map[string]any, appleID string) bool {
	if appleID == "" || familyResponseError(details) || !boolValue(details["is-member-of-family"]) {
		return false
	}
	members, _ := details["family-members"].([]any)
	for _, raw := range members {
		member, _ := raw.(map[string]any)
		if strings.EqualFold(familyMemberAppleID(member), appleID) {
			return true
		}
	}
	return false
}

// Only repeat reads: a timed-out add may already have committed at Apple.
func readAddedFamilyMember(ctx context.Context, client *http.Client, request func() (*http.Request, error), appleID string) (map[string]any, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := request()
		var details map[string]any
		if err == nil {
			if response, err := client.Do(req.Clone(readCtx)); err == nil {
				if data, err := readLimited(response); err == nil {
					details, _ = decodeObject(data)
				}
			}
		}
		cancel()
		if familyContainsAccount(details, appleID) {
			return details, nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(350 * time.Millisecond):
			}
		}
	}
	return nil, errors.New("current organizer state did not confirm the exact target; do not replay addition")
}
