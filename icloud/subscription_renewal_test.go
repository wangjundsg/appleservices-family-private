package icloud

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSubscriptionMissingRenewalIsNotCancelled(t *testing.T) {
	for _, value := range []string{``, `,"isAutoRenewEnabled":"false"`, `,"isAutoRenewEnabled":null`, `,"isAutoRenewEnabled":false`} {
		client := &http.Client{Transport: testStoreTransport(func(req *http.Request) (*http.Response, error) {
			body := `{"active":[{"subscriptionId":"fixture","displayName":"iCloud+"}]}`
			if strings.HasSuffix(req.URL.Path, "/detail") {
				body = `{"specialSubInfo":{"storageAmount":"50 GB"},"latestPlan":{"paidPrice":"¥6.00"}` + value + `}`
			}
			return &http.Response{StatusCode: 200, Request: req, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		session := &StoreCommerceSession{client: client, identity: &storeSession{DSID: "123", PasswordToken: "fixture", GUID: "TEST", StoreFront: "143465"}}
		sub, err := session.ReadSkillSubscription(context.Background())
		if value == `,"isAutoRenewEnabled":false` {
			if err != nil || sub.AutoRenew {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("unknown renewal interpreted as cancelled")
		}
	}
}
