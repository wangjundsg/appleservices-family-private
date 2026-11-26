package gsa

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSecondFactorPreflightStopsBeforeCredentialsAndDelivery(t *testing.T) {
	for _, operation := range []string{"sms", "phone", "trusted"} {
		t.Run(operation, func(t *testing.T) {
			c := NewClient(headerProvider{})
			requests := 0
			c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != "HEAD" || r.URL.Path != "/" || len(r.Header) != 0 || r.Body != nil {
					t.Fatal("credentials or delivery preceded preflight")
				}
				return nil, errors.New("private-proxy-credential")
			})
			var err error
			switch operation {
			case "sms":
				err = c.RequestSMSCode("private-account", "private-token")
			case "phone":
				err = c.RequestSMSCodeForPhone("private-account", "private-token", 7)
			case "trusted":
				err = c.RequestTrustedDeviceCode("private-account", "private-token")
			}
			var preflight *RoutePreflightError
			if !errors.As(err, &preflight) || preflight.Stage != "connect" || requests != 1 || strings.Contains(err.Error(), "private") {
				t.Fatalf("missing safe preflight: %v", err)
			}
		})
	}
}
