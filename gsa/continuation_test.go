package gsa

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthenticationContinuation(t *testing.T) {
	for _, au := range []string{"", "secondaryAuth", "trustedDeviceSecondaryAuth", "repair", "private-marker"} {
		needs2FA, err := continuationRequires2FA(au)
		want2FA := au == "secondaryAuth" || au == "trustedDeviceSecondaryAuth"
		if needs2FA != want2FA || (err != nil) != (au == "repair" || au == "private-marker") || errors.Is(err, ErrAccountRepairRequired) != (au == "repair") {
			t.Fatalf("unexpected result for %q", au)
		}
		if err != nil && strings.Contains(err.Error(), "private-marker") {
			t.Fatal("unrecognised service value in public error")
		}
	}
}
