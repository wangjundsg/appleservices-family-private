package proxypool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The port table is what keeps DataImpulse candidates apart. Contract §4 says
// three things about it, and all three are why this file exists:
//
//   - a candidate *is* its sticky port, so the port an account holds is its
//     exit, and two accounts on one port are one exit wearing two names;
//   - the table has to be locked across processes and persisted, because two
//     independent allocators would otherwise each believe every port is free
//     ("另一程序若共用同一代理认证账号，必须协调端口分配…两个独立分配器不能各自认定全部端口空闲");
//   - an already bound task reuses its saved port instead of allocating again.
//
// The table is a small JSON file, not a database: it holds a handful of entries
// and is read once per run. The lock is a file created with O_CREATE|O_EXCL, so
// creating it is the test-and-set, and a lock left behind by a killed process is
// taken over once it is older than staleLockAge.
const (
	// portTableName is the ledger file inside the state directory.
	portTableName = "proxy-ports.json"
	// portLockName guards every read-modify-write of portTableName.
	portLockName = "proxy-ports.lock"

	// DefaultLeaseTTL is how long a leased port stays reserved for one account.
	//
	// It is deliberately much shorter than the 7200 s retention the workbench
	// uses (contract §4). That retention exists to keep a recently used port out
	// of rotation so the next job does not land on the exit the previous one just
	// burned; this program only needs the port to survive the gap between
	// choosing an exit and finishing one account, and it shares a small number of
	// ports between few accounts. Callers that want the long window pass it as
	// the ttl argument — the value is a parameter, not a constant of the design.
	DefaultLeaseTTL = 30 * time.Minute

	// staleLockAge is when a lock file stops being trusted. The lock is created,
	// stamped and removed within microseconds, so anything this old belongs to a
	// run that died between those two steps.
	staleLockAge = 5 * time.Minute

	// lockWait bounds how long one Allocate waits for a live lock. The holder
	// rewrites a handful of JSON entries, so this is two orders of magnitude of
	// headroom, not a queue.
	lockWait = 10 * time.Second

	// lockPoll is how often a waiting Allocate retries the lock.
	lockPoll = 5 * time.Millisecond
)

// portLease is one account's claim on one port.
type portLease struct {
	Account    string    `json:"account"`
	LeaseUntil time.Time `json:"lease_until"`
}

// portTable is the on-disk ledger, keyed by the decimal port:
//
//	{"ports":{"10005":{"account":"acct-01","lease_until":"2026-11-13T18:00:00Z"}}}
type portTable struct {
	Ports map[string]portLease `json:"ports"`
}

// Allocate reserves one lease port for accountID and returns it.
//
// The rules, in the order they are applied:
//
//  1. A live lease this account already holds is returned as is — same account,
//     same port — even if the candidate list would now offer a different one.
//  2. Otherwise the first candidate that is unleased or whose lease has expired
//     is taken and written to the table.
//  3. Otherwise nothing is free and the caller gets ErrNoFreePort. No entry is
//     overwritten and no account is moved to a port it did not get.
//
// ttl <= 0 means DefaultLeaseTTL.
func Allocate(stateDir, accountID string, ports []int, ttl time.Duration) (int, error) {
	account := strings.TrimSpace(accountID)
	if account == "" {
		return 0, fmt.Errorf("%w: empty account id would hand one port to every caller", ErrNoAccount)
	}
	if len(ports) == 0 {
		return 0, ErrNoPorts
	}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return 0, fmt.Errorf("%w: %d", ErrBadPort, port)
		}
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	dir, err := prepareStateDir(stateDir)
	if err != nil {
		return 0, err
	}

	unlock, err := lockPortTable(dir)
	if err != nil {
		return 0, err
	}
	defer unlock()

	table, err := loadPortTable(dir)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()

	// One account, one port. The live lease wins over the candidate order: moving
	// an account to a second port would give it a second exit, which is the one
	// thing this table exists to prevent.
	for _, port := range ports {
		if lease, held := table.Ports[portKey(port)]; held && lease.Account == account && lease.LeaseUntil.After(now) {
			return port, nil
		}
	}
	// A live lease outside the candidate list is a caller error, not a hint to
	// allocate a second port for the same account.
	for key, lease := range table.Ports {
		if lease.Account != account || !lease.LeaseUntil.After(now) {
			continue
		}
		held, _ := strconv.Atoi(key)
		return 0, fmt.Errorf("%w: %s holds %d, which is not in the %d candidate(s) offered",
			ErrLeaseOutsidePool, account, held, len(ports))
	}

	for _, port := range ports {
		if lease, taken := table.Ports[portKey(port)]; taken && lease.LeaseUntil.After(now) {
			continue
		}
		table.Ports[portKey(port)] = portLease{Account: account, LeaseUntil: now.Add(ttl)}
		if err := savePortTable(dir, table); err != nil {
			return 0, err
		}
		return port, nil
	}
	return 0, fmt.Errorf("%w: %d candidate(s) all leased", ErrNoFreePort, len(ports))
}

// Release drops one port from the table. An account creation is short: once the
// run that held a port is done, nothing needs the reservation any more, so the
// CLI releases both on a failed preflight and on -release. Releasing a port
// nobody holds is not an error, which keeps the flag idempotent.
func Release(stateDir string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: %d", ErrBadPort, port)
	}
	dir, err := prepareStateDir(stateDir)
	if err != nil {
		return err
	}
	unlock, err := lockPortTable(dir)
	if err != nil {
		return err
	}
	defer unlock()

	table, err := loadPortTable(dir)
	if err != nil {
		return err
	}
	key := portKey(port)
	if _, held := table.Ports[key]; !held {
		return nil
	}
	delete(table.Ports, key)
	return savePortTable(dir, table)
}

// Peek answers "which port would this account get?" without taking the lock,
// writing the table or creating the state directory. -dry-run uses it: a dry run
// must not spend a lease or leave a file behind, but it still has to name the
// port the real run would pick.
func Peek(stateDir, accountID string, ports []int) (int, bool) {
	account := strings.TrimSpace(accountID)
	table, err := loadPortTable(strings.TrimSpace(stateDir))
	if err != nil {
		return 0, false
	}
	now := time.Now().UTC()
	for _, port := range ports {
		if lease, held := table.Ports[portKey(port)]; held && lease.Account == account && lease.LeaseUntil.After(now) {
			return port, true
		}
	}
	for _, port := range ports {
		if lease, held := table.Ports[portKey(port)]; held && lease.LeaseUntil.After(now) {
			continue
		}
		return port, false
	}
	return 0, false
}

// portKey is the table key for one port.
func portKey(port int) string { return strconv.Itoa(port) }

// prepareStateDir makes sure the state directory exists and returns it. Every
// entry point goes through it, so a missing cache directory is created on first
// use instead of failing the run.
func prepareStateDir(stateDir string) (string, error) {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		return "", fmt.Errorf("%w: empty state directory", ErrPortTable)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	return dir, nil
}

// loadPortTable reads the ledger. A missing or empty file is an empty table: the
// first run on a machine has to start somewhere.
func loadPortTable(dir string) (portTable, error) {
	table := portTable{Ports: map[string]portLease{}}
	if strings.TrimSpace(dir) == "" {
		return table, nil
	}
	path := filepath.Join(dir, portTableName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return table, nil
	}
	if err != nil {
		return table, fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return table, nil
	}
	var decoded portTable
	if err := json.Unmarshal(data, &decoded); err != nil {
		// The file is ours, so a broken one is a bug or a half-finished copy. It
		// is not guessed at: overwriting it would silently free every leased port.
		return table, fmt.Errorf("%w: %s: %v", ErrPortTable, path, err)
	}
	if decoded.Ports == nil {
		decoded.Ports = map[string]portLease{}
	}
	return decoded, nil
}

// savePortTable writes the ledger through a temporary file, so a reader in
// another process sees either the old table or the new one.
func savePortTable(dir string, table portTable) error {
	if table.Ports == nil {
		table.Ports = map[string]portLease{}
	}
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	tmp, err := os.CreateTemp(dir, portTableName+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, portTableName)); err != nil {
		cleanup()
		return fmt.Errorf("%w: %v", ErrPortTable, err)
	}
	return nil
}

// lockPortTable takes the cross-process table lock and returns the function that
// drops it. O_CREATE|O_EXCL makes the creation itself the test-and-set: exactly
// one process can succeed, and the file's existence is the lock.
func lockPortTable(dir string) (func(), error) {
	path := filepath.Join(dir, portLockName)
	deadline := time.Now().Add(lockWait)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		switch {
		case err == nil:
			// pid and time are for the operator who finds this file after a crash;
			// staleness (below) is decided by age, not by trusting the content.
			_, _ = fmt.Fprintf(file, "pid=%d at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			if err := file.Close(); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("%w: %v", ErrPortLock, err)
			}
			return func() { unlockPortTable(path) }, nil
		case !errors.Is(err, os.ErrExist):
			return nil, fmt.Errorf("%w: %v", ErrPortLock, err)
		}

		if lockIsStale(path) {
			// A run that died between create and remove left this behind. Removing
			// it is the takeover; the next iteration creates its own file. A failed
			// removal means a waiter is reading the file right now, so this falls
			// through to the retry below instead of giving up.
			if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s held for longer than %s", ErrPortLock, path, lockWait)
		}
		time.Sleep(lockPoll)
	}
}

// unlockPortTable removes the lock file. Windows refuses to delete a file
// another handle has open without FILE_SHARE_DELETE, and the waiters are reading
// the lock to decide whether it is stale — so a single attempt can lose a race
// that a retry wins. A lock that survived every retry would block later runs
// until it aged out, which is why this does not simply ignore the error.
func unlockPortTable(path string) {
	for attempt := 0; attempt < 200; attempt++ {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// lockIsStale reports whether a lock file was abandoned. The stamp inside is
// only trusted when it is newer than the file, because a copied or restored
// directory can carry an old mtime for a lock that is actually live.
func lockIsStale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	newest := info.ModTime()
	if data, err := os.ReadFile(path); err == nil {
		if at, ok := lockStamp(string(data)); ok && at.After(newest) {
			newest = at
		}
	}
	return newest.Before(time.Now().Add(-staleLockAge))
}

// lockStamp reads the "at=<RFC3339Nano>" field a lock file is stamped with.
func lockStamp(data string) (time.Time, bool) {
	for _, line := range strings.Split(data, "\n") {
		_, value, ok := strings.Cut(strings.TrimSpace(line), "at=")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err != nil {
			return time.Time{}, false
		}
		return at, true
	}
	return time.Time{}, false
}
