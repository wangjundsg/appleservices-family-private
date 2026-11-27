package gsa

import (
	"io"
	"net/http"
	"testing"
)

func TestSecondFactorUsesActualRequestWithoutHEADGateOrReplay(t *testing.T) {
	for _, operation := range []string{"sms", "phone", "trusted"} {
		t.Run(operation, func(t *testing.T) {
			c := NewClient(headerProvider{})
			requests := 0
			c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				want := "/auth"
				if operation == "phone" {
					want = "/auth/verify/phone"
				}
				if operation == "trusted" {
					want = "/auth/verify/trusteddevice"
				}
				if r.Method == "HEAD" || r.URL.Path != want {
					t.Fatal("actual operation blocked by unrelated probe")
				}
				return nil, io.EOF
			})
			var err error
			switch operation {
			case "sms":
				err = c.RequestSMSCode("fixture-account", "fixture-token")
			case "phone":
				err = c.RequestSMSCodeForPhone("fixture-account", "fixture-token", 7)
			case "trusted":
				err = c.RequestTrustedDeviceCode("fixture-account", "fixture-token")
			}
			if err == nil || requests != 1 {
				t.Fatal("ambiguous request repeated or failure suppressed")
			}
		})
	}
}
