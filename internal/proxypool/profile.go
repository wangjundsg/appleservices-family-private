package proxypool

import (
	"fmt"

	"github.com/Laky-64/appleservices/internal/profile"
)

// Bind records the intended exit in the account's profile: Expected is the
// route the configuration promised, and every later stage reads it back to ask
// whether it is still leaving from where it said it would.
//
// Only the egress reference is written. The profile is the fingerprint every
// stage agreed on, so a routing decision must not be able to move anything else
// in it.
func Bind(dir string, e Endpoint) error {
	ref := e.Ref()
	if ref == "" {
		return fmt.Errorf("%w: %s", ErrEmptyRef, e.Redacted())
	}
	p, err := profile.Load(dir)
	if err != nil {
		return fmt.Errorf("proxypool: bind %s: %w", ref, err)
	}
	p.Expected.EgressRef = ref
	if err := profile.Save(dir, p); err != nil {
		return fmt.Errorf("proxypool: bind %s: %w", ref, err)
	}
	return nil
}

// RecordActual stores the address the exit really used. Actual exists to stay
// visibly separate from Expected: when the two disagree that disagreement is
// the finding, so this writes one field and leaves the rest of the file — the
// intent, the timestamps, the fingerprint — exactly as it found them.
func RecordActual(dir, ip string) error {
	if normaliseIP(ip) == "" {
		return fmt.Errorf("%w: %q", ErrNoIP, ip)
	}
	p, err := profile.Load(dir)
	if err != nil {
		return fmt.Errorf("proxypool: record egress ip: %w", err)
	}
	p.Actual.EgressIP = normaliseIP(ip)
	if err := profile.Save(dir, p); err != nil {
		return fmt.Errorf("proxypool: record egress ip: %w", err)
	}
	return nil
}
