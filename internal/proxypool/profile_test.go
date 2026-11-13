package proxypool

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Laky-64/appleservices/internal/profile"
)

// freezeProfile writes a valid frozen profile into a fresh directory, the way
// the creation stage leaves it.
func freezeProfile(t *testing.T, dir string) profile.Profile {
	t.Helper()
	frozen, err := profile.Generate("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Save(dir, frozen); err != nil {
		t.Fatal(err)
	}
	loaded, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// TestBindWritesOnlyTheEgressRef: the profile is the fingerprint every stage
// agreed on, so binding a route must not be able to move anything else.
func TestBindWritesOnlyTheEgressRef(t *testing.T) {
	dir := t.TempDir()
	before := freezeProfile(t, dir)

	endpoint := mustParse(t, diHTTP)
	if err := Bind(dir, endpoint); err != nil {
		t.Fatalf("bind: %v", err)
	}

	after, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.Expected.EgressRef = "di:us:10012"
	if !reflect.DeepEqual(want, after) {
		t.Fatalf("bind touched more than expected.egress_ref:\nwant %+v\ngot  %+v", want, after)
	}
	if after.Actual.EgressIP != "" || after.Actual.ObservedAt != "" {
		t.Fatalf("bind wrote into actual: %+v", after.Actual)
	}
	if err := profile.Validate(after); err != nil {
		t.Fatalf("the bound profile no longer validates: %v", err)
	}

	// The reference is what later stages compare against, and it is a plain
	// file: it must not carry the provider's credentials.
	data, err := os.ReadFile(profile.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("s3cr3t-pass")) || bytes.Contains(data, []byte("abc123__cr.us")) {
		t.Fatalf("profile.json carries credentials:\n%s", data)
	}
	if !strings.Contains(string(data), `"egress_ref": "di:us:10012"`) {
		t.Fatalf("profile.json has no readable egress_ref:\n%s", data)
	}
}

// TestRecordActualWritesOnlyTheEgressIP: Expected and Actual stay separate on
// purpose; overwriting one with the other would erase the disagreement a later
// stage is supposed to report.
func TestRecordActualWritesOnlyTheEgressIP(t *testing.T) {
	dir := t.TempDir()
	freezeProfile(t, dir)

	if err := Bind(dir, mustParse(t, trust711)); err != nil {
		t.Fatal(err)
	}
	bound, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordActual(dir, " 203.0.113.7 "); err != nil {
		t.Fatalf("record: %v", err)
	}

	after, err := profile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := bound
	want.Actual.EgressIP = "203.0.113.7"
	if !reflect.DeepEqual(want, after) {
		t.Fatalf("record touched more than actual.egress_ip:\nwant %+v\ngot  %+v", want, after)
	}
	if after.Expected.EgressRef != "711:us:12345678" {
		t.Fatalf("record clobbered the expected route: %+v", after.Expected)
	}
	if after.Actual.ObservedAt != "" {
		t.Fatalf("record invented an observation time: %q", after.Actual.ObservedAt)
	}
}

// TestRecordActualRejectsGarbage keeps a probe body that merely looked like an
// address out of the profile.
func TestRecordActualRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	freezeProfile(t, dir)
	if err := RecordActual(dir, "999.1.1.1"); !errors.Is(err, ErrNoIP) {
		t.Fatalf("err = %v, want ErrNoIP", err)
	}
	if err := RecordActual(dir, ""); !errors.Is(err, ErrNoIP) {
		t.Fatalf("empty ip: err = %v, want ErrNoIP", err)
	}
}

// TestBindAndRecordNeedAProfile: a directory without a frozen fingerprint is
// not an account, and neither write may invent one.
func TestBindAndRecordNeedAProfile(t *testing.T) {
	dir := t.TempDir()
	endpoint := mustParse(t, clash)
	if err := Bind(dir, endpoint); err == nil {
		t.Fatal("bind accepted a directory without a profile")
	}
	if err := RecordActual(dir, "203.0.113.7"); err == nil {
		t.Fatal("record accepted a directory without a profile")
	}
}
