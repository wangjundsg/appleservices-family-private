package main

import (
	"errors"
	"fmt"
	"strings"
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

func TestVerificationFailureControlsWhetherCodeEntryStaysOpen(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
		stop bool
		http int
	}{
		{appleservices.ErrVerificationCodeFormat, "verification_input_invalid", false, 0},
		{fmt.Errorf("private-marker: %w", appleservices.ErrVerifiedSessionIncomplete), "verified_session_incomplete", true, 0},
		{fmt.Errorf("%w: %w", appleservices.ErrVerifiedSessionIncomplete, gsa.ErrAccountRepairRequired), "gsa_account_repair_required", true, 0},
		{&gsa.SecondFactorError{HTTPStatus: 412}, "verification_blocked", true, 412},
		{&gsa.SecondFactorError{HTTPStatus: 429}, "verification_blocked", true, 429},
		{&gsa.SecondFactorError{HTTPStatus: 503}, "verification_unconfirmed", true, 503},
		{errors.New("private-marker"), "verification_unconfirmed", true, 0},
	} {
		result, stop := verificationFailure(tc.err)
		if result.OK || result.Stage != tc.want || stop != tc.stop || result.HTTPStatus != tc.http || strings.Contains(result.Message, "private-marker") {
			t.Fatalf("unexpected public result: %+v, stop=%v", result, stop)
		}
	}
}
