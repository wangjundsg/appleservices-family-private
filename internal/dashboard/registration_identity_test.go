package dashboard

import (
	"testing"
	"time"
)

func TestRegistrationIdentityIsAdultAndVaries(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	seen := map[registrationIdentity]bool{}
	for range 64 {
		identity, err := newRegistrationIdentity(now)
		if err != nil {
			t.Fatal(err)
		}
		if !validRegistrationIdentity(identity, now) {
			t.Fatalf("generated invalid identity: %+v", identity)
		}
		seen[identity] = true
	}
	if len(seen) < 8 {
		t.Fatalf("identity pool did not vary: %d unique values", len(seen))
	}
}

func TestRegistrationIdentityRequiresOlderThanEighteen(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	base := registrationIdentity{First: "Audrey", Last: "Bennett"}
	base.Birthday = "2008-11-26"
	if validRegistrationIdentity(base, now) {
		t.Fatal("exactly eighteen was accepted")
	}
	base.Birthday = "2008-11-25"
	if !validRegistrationIdentity(base, now) {
		t.Fatal("older than eighteen was rejected")
	}
}
