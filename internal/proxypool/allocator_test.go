package proxypool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// leasePorts is the candidate set most tests use: ports inside the DataImpulse
// range, so nothing here depends on an arithmetic that would not happen in
// production.
func leasePorts(n int) []int {
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ports = append(ports, DataImpulseLeaseStart+i)
	}
	return ports
}

// TestAllocateReusesPortForTheSameAccount is the property the whole table exists
// for (contract §4): one account, one port, on every run and whatever order the
// candidates arrive in.
func TestAllocateReusesPortForTheSameAccount(t *testing.T) {
	dir := t.TempDir()
	ports := leasePorts(3)

	first, err := Allocate(dir, "acct-01", ports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Allocate(dir, "acct-01", ports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same account got %d then %d", first, second)
	}
	// A different candidate order must not move it either.
	shuffled := []int{ports[2], ports[0], ports[1]}
	third, err := Allocate(dir, "acct-01", shuffled, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("a reordered candidate list moved the account from %d to %d", first, third)
	}
}

// TestAllocateGivesAccountsDifferentPorts is the other half: two accounts on one
// port would be one exit wearing two names.
func TestAllocateGivesAccountsDifferentPorts(t *testing.T) {
	dir := t.TempDir()
	ports := leasePorts(4)
	seen := map[int]bool{}
	for i, account := range []string{"acct-01", "acct-02", "acct-03"} {
		port, err := Allocate(dir, account, ports, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[port] {
			t.Fatalf("%s landed on port %d, which is already taken", account, port)
		}
		seen[port] = true
		if want := ports[i]; port != want {
			t.Fatalf("%s got %d, want the first free candidate %d", account, port, want)
		}
	}
}

// TestAllocateReportsExhaustion: nothing is overwritten when every candidate is
// leased, and the caller is told instead of being handed someone else's port.
func TestAllocateReportsExhaustion(t *testing.T) {
	dir := t.TempDir()
	ports := []int{DataImpulseLeaseStart, DataImpulseLeaseStart + 1}
	if _, err := Allocate(dir, "acct-01", ports, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(dir, "acct-02", ports, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(dir, "acct-03", ports, time.Minute); !errors.Is(err, ErrNoFreePort) {
		t.Fatalf("err = %v, want ErrNoFreePort", err)
	}
	// The first account still holds its port.
	port, err := Allocate(dir, "acct-01", ports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if port != DataImpulseLeaseStart {
		t.Fatalf("acct-01 now holds %d", port)
	}
}

// TestAllocateReusesExpiredLease: a lease past its ttl is not a reservation any
// more, so the port goes to the next account without being cleared by hand.
func TestAllocateReusesExpiredLease(t *testing.T) {
	dir := t.TempDir()
	ports := []int{DataImpulseLeaseStart}
	port, err := Allocate(dir, "acct-01", ports, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if port != ports[0] {
		t.Fatalf("port = %d", port)
	}
	time.Sleep(120 * time.Millisecond)
	again, err := Allocate(dir, "acct-02", ports, time.Minute)
	if err != nil {
		t.Fatalf("an expired lease was not reusable: %v", err)
	}
	if again != ports[0] {
		t.Fatalf("acct-02 got %d, want the expired %d", again, ports[0])
	}
}

// TestAllocateTakesOverStaleLock is the crash path: a run killed between create
// and remove leaves the lock behind, and the next run has to take it over
// instead of waiting forever.
func TestAllocateTakesOverStaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, portLockName)
	if err := os.WriteFile(lockPath, []byte("pid=999999 at=2000-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleLockAge)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	port, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart}, time.Minute)
	if err != nil {
		t.Fatalf("stale lock was not taken over: %v", err)
	}
	if port != DataImpulseLeaseStart {
		t.Fatalf("port = %d", port)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("the lock file is still there after the run: %v", err)
	}
}

// TestLockStaleness pins the takeover window, including the restored-directory
// case where the file's mtime is old but its own stamp says it is live.
func TestLockStaleness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stale := now.Add(-2 * staleLockAge)

	fresh := filepath.Join(dir, "fresh.lock")
	if err := os.WriteFile(fresh, []byte("pid=1 at="+now.Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockIsStale(fresh) {
		t.Fatal("a lock just written was called stale")
	}

	abandoned := filepath.Join(dir, "abandoned.lock")
	if err := os.WriteFile(abandoned, []byte("pid=1 at="+stale.Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abandoned, stale, stale); err != nil {
		t.Fatal(err)
	}
	if !lockIsStale(abandoned) {
		t.Fatalf("a %s old lock was not called stale", 2*staleLockAge)
	}

	restored := filepath.Join(dir, "restored.lock")
	if err := os.WriteFile(restored, []byte("pid=1 at="+now.Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(restored, stale, stale); err != nil {
		t.Fatal(err)
	}
	if lockIsStale(restored) {
		t.Fatal("an old mtime with a fresh stamp was taken over")
	}
}

// TestAllocateHoldsTheLockAcrossConcurrentAllocators is the two-process case
// from §4 collapsed into goroutines over one state directory: eight allocators
// racing on eight candidate ports must produce eight different ports.
func TestAllocateHoldsTheLockAcrossConcurrentAllocators(t *testing.T) {
	dir := t.TempDir()
	ports := leasePorts(8)
	const workers = 8

	var (
		wg      sync.WaitGroup
		results = make([]int, workers)
		errs    = make([]error, workers)
		start   = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = Allocate(dir, "acct-"+strconv.Itoa(i), ports, time.Minute)
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int]string, workers)
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		account := "acct-" + strconv.Itoa(i)
		if other, taken := seen[results[i]]; taken {
			t.Fatalf("port %d was handed to both %s and %s", results[i], other, account)
		}
		seen[results[i]] = account
	}
	if len(seen) != workers {
		t.Fatalf("%d workers used %d ports", workers, len(seen))
	}

	// The table on disk agrees with what the workers were told.
	table, err := loadPortTable(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Ports) != workers {
		t.Fatalf("table holds %d entries, want %d", len(table.Ports), workers)
	}
	for port, lease := range table.Ports {
		value, err := strconv.Atoi(port)
		if err != nil {
			t.Fatalf("table key %q is not a port", port)
		}
		if seen[value] != lease.Account {
			t.Fatalf("port %s is held by %q on disk, %q in memory", port, lease.Account, seen[value])
		}
	}
}

// TestReleaseFreesThePort: a released port is a free port, immediately.
func TestReleaseFreesThePort(t *testing.T) {
	dir := t.TempDir()
	ports := []int{DataImpulseLeaseStart, DataImpulseLeaseStart + 1}
	first, err := Allocate(dir, "acct-01", ports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, first); err != nil {
		t.Fatal(err)
	}
	next, err := Allocate(dir, "acct-02", ports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next != first {
		t.Fatalf("acct-02 got %d, want the released %d", next, first)
	}
	// The released account is free to take the next still-free port.
	if port, err := Allocate(dir, "acct-01", ports, time.Minute); err != nil || port == next {
		t.Fatalf("acct-01 = %d, %v", port, err)
	}
}

// TestReleaseIsIdempotent keeps -release usable as a cleanup command: releasing
// a port nobody holds, or the same port twice, is not a failure.
func TestReleaseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := Release(dir, DataImpulseLeaseStart); err != nil {
		t.Fatalf("release of an unleased port = %v", err)
	}
	if _, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, DataImpulseLeaseStart); err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, DataImpulseLeaseStart); err != nil {
		t.Fatalf("second release = %v", err)
	}
	for _, port := range []int{0, -1, 70000} {
		if err := Release(dir, port); !errors.Is(err, ErrBadPort) {
			t.Fatalf("release(%d) = %v, want ErrBadPort", port, err)
		}
	}
}

// TestAllocateRefusesALeaseOutsideThePool: a live lease the caller's candidate
// list no longer covers is a configuration the operator has to see, not a reason
// to hand the account a second port.
func TestAllocateRefusesALeaseOutsideThePool(t *testing.T) {
	dir := t.TempDir()
	if _, err := Allocate(dir, "acct-01", leasePorts(2), time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart + 1}, time.Minute)
	if !errors.Is(err, ErrLeaseOutsidePool) {
		t.Fatalf("err = %v, want ErrLeaseOutsidePool", err)
	}
}

// TestAllocateRejectsBadInput keeps the obvious mistakes out of the table.
func TestAllocateRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := Allocate(dir, "   ", []int{DataImpulseLeaseStart}, time.Minute); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("empty account: %v", err)
	}
	if _, err := Allocate(dir, "acct-01", nil, time.Minute); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("no candidates: %v", err)
	}
	if _, err := Allocate(dir, "acct-01", []int{0}, time.Minute); !errors.Is(err, ErrBadPort) {
		t.Fatalf("port 0: %v", err)
	}
	if _, err := Allocate("   ", "acct-01", []int{DataImpulseLeaseStart}, time.Minute); !errors.Is(err, ErrPortTable) {
		t.Fatalf("empty state dir: %v", err)
	}
}

// TestPortTableRefusesToGuess: a broken table is not reset. Resetting it would
// silently free every leased port and hand one exit to two accounts.
func TestPortTableRefusesToGuess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, portTableName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart}, time.Minute); !errors.Is(err, ErrPortTable) {
		t.Fatalf("err = %v, want ErrPortTable", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{not json" {
		t.Fatalf("the broken table was overwritten: %q", data)
	}
	// A lock must not be left behind by the failed run either.
	if _, err := os.Stat(filepath.Join(dir, portLockName)); !os.IsNotExist(err) {
		t.Fatalf("a failed run left its lock behind: %v", err)
	}
}

// TestPortTableOnDiskShape pins the file format §4 documents, so an operator (or
// another program) can read it.
func TestPortTableOnDiskShape(t *testing.T) {
	dir := t.TempDir()
	port, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart + 5}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if port != DataImpulseLeaseStart+5 {
		t.Fatalf("port = %d", port)
	}
	data, err := os.ReadFile(filepath.Join(dir, portTableName))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Ports map[string]struct {
			Account    string `json:"account"`
			LeaseUntil string `json:"lease_until"`
		} `json:"ports"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	entry, ok := payload.Ports[strconv.Itoa(port)]
	if !ok {
		t.Fatalf("table has no entry for %d: %s", port, data)
	}
	if entry.Account != "acct-01" {
		t.Fatalf("account = %q", entry.Account)
	}
	until, err := time.Parse(time.RFC3339, entry.LeaseUntil)
	if err != nil {
		t.Fatalf("lease_until = %q: %v", entry.LeaseUntil, err)
	}
	if until.Before(time.Now()) {
		t.Fatalf("lease_until %q is already in the past", entry.LeaseUntil)
	}
	if _, err := os.Stat(filepath.Join(dir, portLockName)); !os.IsNotExist(err) {
		t.Fatalf("the lock file outlived the run: %v", err)
	}
}

// TestPeekDoesNotTouchTheStateDir is what makes -dry-run honest: it reports the
// port a real run would take without creating a directory or spending a lease.
func TestPeekDoesNotTouchTheStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	ports := leasePorts(2)

	port, leased := Peek(stateDir, "acct-01", ports)
	if port != ports[0] || leased {
		t.Fatalf("Peek = %d, %t; want %d, false", port, leased, ports[0])
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("Peek created %s", stateDir)
	}

	if _, err := Allocate(stateDir, "acct-01", ports, time.Minute); err != nil {
		t.Fatal(err)
	}
	if port, leased = Peek(stateDir, "acct-01", ports); port != ports[0] || !leased {
		t.Fatalf("account lease: Peek = %d, %t; want %d, true", port, leased, ports[0])
	}
	if port, leased = Peek(stateDir, "acct-02", ports); port != ports[1] || leased {
		t.Fatalf("second account: Peek = %d, %t; want %d, false", port, leased, ports[1])
	}
}

// TestDefaultLeaseTTLIsShortOnPurpose documents the one number that differs from
// the workbench contract: §4's 7200 s is that program's retention window, while
// this package is a short-lived lease holder.
func TestDefaultLeaseTTLIsShortOnPurpose(t *testing.T) {
	if DefaultLeaseTTL <= 0 {
		t.Fatalf("DefaultLeaseTTL = %s", DefaultLeaseTTL)
	}
	if DefaultLeaseTTL >= 7200*time.Second {
		t.Fatalf("DefaultLeaseTTL = %s, that is the workbench retention, not a short lease", DefaultLeaseTTL)
	}
	// ttl <= 0 means the default, not "no expiry".
	dir := t.TempDir()
	if _, err := Allocate(dir, "acct-01", []int{DataImpulseLeaseStart}, 0); err != nil {
		t.Fatal(err)
	}
	table, err := loadPortTable(dir)
	if err != nil {
		t.Fatal(err)
	}
	lease := table.Ports[strconv.Itoa(DataImpulseLeaseStart)]
	if lease.LeaseUntil.Before(time.Now().Add(DefaultLeaseTTL / 2)) {
		t.Fatalf("lease_until = %s, want about now+%s", lease.LeaseUntil, DefaultLeaseTTL)
	}
}
