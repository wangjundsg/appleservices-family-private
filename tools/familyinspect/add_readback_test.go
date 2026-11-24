package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddedFamilyReadbackUsesFreshExactMember(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"is-member-of-family":true,"family-members":[{"is-me":true,"member-is-organizer":true,"appleId":"mother@example.test"}]}`))
			return
		}
		w.Write([]byte(`{"is-member-of-family":true,"family-members":[{"is-me":true,"member-is-organizer":true,"appleId":"mother@example.test"},{"member-apple-id":"child@example.test"}]}`))
	}))
	defer server.Close()
	request := func() (*http.Request, error) { return http.NewRequest("POST", server.URL, nil) }
	details, err := readAddedFamilyMember(context.Background(), server.Client(), request, "CHILD@example.test")
	if err != nil || calls != 2 || len(details["family-members"].([]any)) != 2 {
		t.Fatalf("fresh member readback failed: calls=%d err=%v", calls, err)
	}
}

func TestAddedFamilyReadbackRejectsUnprovenMembership(t *testing.T) {
	for _, body := range []string{
		`{"success":false,"is-member-of-family":true,"family-members":[{"appleId":"child@example.test"}]}`,
		`{"is-member-of-family":true,"family-invitations":[{"appleId":"child@example.test"}]}`,
		`{"is-member-of-family":true,"family-members":[{"appleId":"other@example.test"}]}`,
		`{"is-member-of-family":false,"family-members":[{"appleId":"child@example.test"}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		request := func() (*http.Request, error) { return http.NewRequest("POST", server.URL, nil) }
		if _, err := readAddedFamilyMember(context.Background(), server.Client(), request, "child@example.test"); err == nil {
			t.Fatal("unconfirmed member accepted")
		}
		server.Close()
	}
}
