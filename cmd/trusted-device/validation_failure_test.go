package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Laky-64/appleservices"
	"github.com/Laky-64/appleservices/gsa"
)

func TestValidationOnlyOpensLoginWhenPasswordIsMissing(t *testing.T) {
	for _, tc := range []struct {
		err   error
		stage string
	}{
		{appleservices.ErrValidationPasswordRequired, "reconnect-required"},
		{fmt.Errorf("sign in: %w", gsa.ErrAccountRepairRequired), "gsa_account_repair_required"},
		{errors.New("private upstream message"), "account_validation_unconfirmed"},
	} {
		got := validationFailure(tc.err)
		if got.OK || got.Stage != tc.stage || got.Message == tc.err.Error() {
			t.Fatalf("unexpected classification %q", got.Stage)
		}
	}
}
