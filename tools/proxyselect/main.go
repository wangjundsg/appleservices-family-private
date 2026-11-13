// Command proxyselect picks the egress endpoint one account has to use and
// freezes that choice in the account's profile.
//
// The pipeline reaches the same account directory from several stages, so the
// choice has to be reproducible from the account alone: the account id is the
// directory name, and the port table (proxypool.Allocate) maps it to one
// DataImpulse lease port, locked across processes and persisted under -state. A
// run therefore does four things and nothing else — validate the template,
// allocate a port, optionally prove the exit still works, record what was
// picked.
//
// The walk after a banned exit is a walk over *ports*, never over routes
// (docs/PROXY-USAGE-CONTRACT-2026-11-13.md §4, §7): a target that answers 403 on a
// residential lease usually means that address is burned for that site, so the
// run releases the port and takes the next candidate. A 503 is different: the
// edge accepted the connection and refused the address segment, so the same
// lease is not retried and the run reports status=unavailable
// (docs/run-20261113.md §4.5). Nothing here ever falls back to the machine's own
// proxy or to a direct connection — a retry that changed the route would make
// the run unable to say which address the account left from. Every attempt is a
// paid lease, which is why the walk is short and why -dry-run exists: it answers
// "which exit would this account get?" without spending one and without touching
// the network. A directory with no frozen profile gets the same treatment: the
// pick is printed with bound=false and the caller runs profilegen next (§4.1).
//
// Credentials never reach a log line: the pick is printed as its Ref (the lease
// reference, no credentials at all) next to Redacted (masked username, no
// password), and the template URL itself is never echoed (§7).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/profile"
	"github.com/Laky-64/appleservices/internal/proxypool"
)

const (
	exitOK = 0
	// exitUsage marks a run the operator has to fix: a missing flag, an
	// unparseable template, a provider that disagrees with the URL.
	exitUsage = 1
	// exitFailure marks a run that was configured correctly and still could
	// not deliver a usable exit.
	exitFailure = 2

	// maxAttempts bounds how many leases one run may burn after a 403.
	maxAttempts = 5

	// probeTimeout bounds one preflight request.
	probeTimeout = 20 * time.Second

	// localTemplate is what -provider local falls back to: the Clash HTTP port.
	localTemplate = "http://127.0.0.1:7897"

	// defaultStateDir holds the port table and its lock. It is a cache, not
	// state worth backing up: a lost table only means the next run has to pick a
	// free port again.
	defaultStateDir = ".cache/proxy"

	// defaultLeaseTTL is how long one account keeps its DataImpulse port. It is
	// shorter than the workbench's 7200 s retention on purpose — that window
	// keeps a recently used exit out of rotation for the next job, while this
	// command only has to hold one port for one account creation. See
	// proxypool.DefaultLeaseTTL.
	defaultLeaseTTL = proxypool.DefaultLeaseTTL
)

const usageText = `usage: proxyselect -dir <account-dir> [options]

  -dir <dir>            account directory (required); its base name is the account id
  -provider <name>      dataimpulse | 711 | local (default dataimpulse)
  -url <template>       proxy template with credentials, or set PROXY_TEMPLATE
  -country <cc>         exit country, two letters (default US)
  -ports <spec>         10000-10020 or 10000,10011 (dataimpulse lease ports)
  -state <dir>          directory holding the port table and its lock (default .cache/proxy)
  -ttl <duration>       how long an account keeps its lease port (default 30m)
  -release <port>       release one leased port and exit (no -dir needed)
  -preflight-url <url>  probe target; without it the run only binds (status=0, ip=empty)
  -account <id>         account id for the pick (default: base name of -dir)
  -attempts <n>         exits one run may try before giving up (default 5)
  -dry-run              print the pick and stop: no network, no writes
  (no profile.json)     print the pick as bound=false and stop; run profilegen next
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process: flags, environment and streams are passed in
// so a test can drive the whole command in-process.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("proxyselect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usageText) }

	var (
		dir          = flags.String("dir", "", "account directory")
		provider     = flags.String("provider", "dataimpulse", "dataimpulse | 711 | local")
		templateFlag = flags.String("url", "", "proxy template with credentials (default: $PROXY_TEMPLATE)")
		country      = flags.String("country", "US", "two-letter exit country")
		ports        = flags.String("ports", "", "lease ports: 10000-10020 or 10000,10011")
		state        = flags.String("state", defaultStateDir, "directory holding the port table and its lock")
		ttl          = flags.Duration("ttl", defaultLeaseTTL, "how long an account keeps its lease port")
		releaseFlag  = flags.Int("release", 0, "release one leased port and exit")
		preflightURL = flags.String("preflight-url", "", "probe target; preflight runs only when set")
		account      = flags.String("account", "", "account id used for the pick (default: base name of -dir)")
		attempts     = flags.Int("attempts", maxAttempts, "how many exits one run may try")
		dryRun       = flags.Bool("dry-run", false, "print the pick and stop, without network or writes")
	)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	fail := func(code string, err error) int {
		fmt.Fprintf(stderr, "proxyselect: error=%s detail=%q\n", code, err.Error())
		return exitFailure
	}
	usage := func(err error) int {
		fmt.Fprintf(stderr, "proxyselect: error=usage detail=%q\n", err.Error())
		flags.Usage()
		return exitUsage
	}

	if flags.NArg() > 0 {
		return usage(fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " ")))
	}

	stateDir, err := resolveStateDir(*state)
	if err != nil {
		return usage(err)
	}

	// -release is the manual half of the lease: it needs a port and a table, not
	// an account directory, so it runs before -dir is required.
	if *releaseFlag != 0 {
		if err := proxypool.Release(stateDir, *releaseFlag); err != nil {
			return fail("release", err)
		}
		fmt.Fprintf(stdout, "released=%d\n", *releaseFlag)
		return exitOK
	}

	if *ttl <= 0 {
		return usage(fmt.Errorf("-ttl must be positive, got %s", *ttl))
	}

	accountDir := strings.TrimSpace(*dir)
	if accountDir == "" {
		return usage(errors.New("-dir is required"))
	}

	template := strings.TrimSpace(*templateFlag)
	if template == "" {
		template = strings.TrimSpace(getenv("PROXY_TEMPLATE"))
	}
	pool, err := buildPool(*provider, template, *country, *ports)
	if err != nil {
		return usage(err)
	}

	accountID := strings.TrimSpace(*account)
	if accountID == "" {
		accountID = filepath.Base(filepath.Clean(accountDir))
	}

	// The candidates are the ports this run may lease. They shrink as attempts
	// fail, so a retry can never re-lease the port it just released.
	var remaining []int
	if pool.SelectsPort() {
		remaining, err = pool.Candidates()
		if err != nil {
			return usage(err)
		}
	}

	// A directory without a frozen profile cannot hold the pick, but the pick
	// itself is still the answer the operator asked for: print it as unbound
	// instead of refusing to run (run-20261113 §4.1 — the documented pipeline
	// order selects the exit before profilegen has ever run).
	bound := true
	if !*dryRun {
		if _, err := os.Stat(profile.Path(accountDir)); err != nil {
			bound = false
			fmt.Fprintf(stderr, "proxyselect: notice=unbound detail=%q\n", "未绑定（缺少 profile.json）")
		}
	}

	if *dryRun {
		return reportDryRun(stdout, pool, stateDir, accountID, remaining, fail)
	}

	limit := *attempts
	if limit < 1 {
		limit = 1
	}
	probeTarget := strings.TrimSpace(*preflightURL)

	ctx := context.Background()
	probe := proxypool.HTTPProbe{Timeout: probeTimeout}

	var (
		chosen  proxypool.Endpoint
		result  proxypool.ProbeResult
		checked bool
		tried   []string
		held    int
	)
	for attempt := 0; attempt < limit; attempt++ {
		if pool.SelectsPort() && len(remaining) == 0 {
			// Every candidate was released again, i.e. every one of them was
			// banned. The run has no second exit left to offer.
			last := "no exit"
			if len(tried) > 0 {
				last = chosen.Ref()
			}
			return fail("blocked", fmt.Errorf("%s: no candidate port left after %d attempt(s)", last, len(tried)))
		}
		endpoint, port, err := leaseEndpoint(pool, stateDir, accountID, remaining, *ttl)
		if err != nil {
			return fail("allocate", err)
		}
		if slices.Contains(tried, endpoint.Ref()) {
			// 711 pins its lease in the username, so "the next port" would be the
			// same exit again: the pool has no second exit to offer.
			if err := releasePort(stateDir, port); err != nil {
				return fail("release", err)
			}
			return fail("blocked", fmt.Errorf("%s: no second exit after %d attempt(s)", endpoint.Ref(), len(tried)))
		}
		tried = append(tried, endpoint.Ref())
		chosen, held = endpoint, port

		if !bound || probeTarget == "" {
			break
		}
		measured, err := proxypool.PreflightResult(ctx, probe, endpoint, probeTarget)
		result, checked = measured, true
		if err == nil {
			break
		}
		// Whatever the verdict, this port is done for this run. Releasing it is
		// what makes the next attempt a different lease instead of a route
		// change (§7: never switch between direct and forwarded on a failure).
		if err := releasePort(stateDir, port); err != nil {
			return fail("release", err)
		}
		if errors.Is(err, proxypool.ErrBlocked) && attempt+1 < limit {
			fmt.Fprintf(stderr, "proxyselect: notice=blocked ref=%s lease_port=%d status=%d retry=true\n", endpoint.Ref(), port, measured.Status)
			remaining = withoutPort(remaining, port)
			continue
		}
		return fail(codeOf(err), err)
	}

	if !bound {
		// Only the pick is printed: probing costs a lease and there is nowhere
		// to record the result yet. The caller runs profilegen and repeats.
		fmt.Fprintf(stdout, "ref=%s kind=%s sticky=%t country=%s lease_port=%d redacted=%s bound=false\n",
			chosen.Ref(), chosen.Kind, chosen.Sticky, chosen.Country, held, chosen.Redacted())
		return exitOK
	}

	if err := proxypool.Bind(accountDir, chosen); err != nil {
		return fail("bind", err)
	}
	recorded := false
	if checked && result.IP != "" {
		if err := proxypool.RecordActual(accountDir, result.IP); err != nil {
			return fail("record", err)
		}
		recorded = true
	}
	fmt.Fprintf(stdout, "ref=%s status=%d ip=%s attempts=%d lease_port=%d redacted=%s recorded=%t\n",
		chosen.Ref(), result.Status, result.IP, len(tried), held, chosen.Redacted(), recorded)
	return exitOK
}

// reportDryRun answers "which exit would this account get?" without a write, a
// lock or a socket. Peek reads the table if one exists and never creates it, so
// the printed port is the one the real run would take.
func reportDryRun(stdout io.Writer, pool proxypool.Pool, stateDir, accountID string, candidates []int, fail func(string, error) int) int {
	port, leased := 0, false
	if pool.SelectsPort() {
		port, leased = proxypool.Peek(stateDir, accountID, candidates)
		if port == 0 {
			return fail("allocate", proxypool.ErrNoFreePort)
		}
	}
	endpoint, err := pool.At(port)
	if err != nil {
		return fail("select", err)
	}
	fmt.Fprintf(stdout, "dry-run=true ref=%s kind=%s sticky=%t country=%s lease_port=%d leased=%t redacted=%s\n",
		endpoint.Ref(), endpoint.Kind, endpoint.Sticky, endpoint.Country, port, leased, endpoint.Redacted())
	return exitOK
}

// leaseEndpoint turns one account's port decision into a dialable endpoint. On
// DataImpulse the port comes from the table, never from a hash, so two runs
// cannot land on one port by accident (contract §4).
func leaseEndpoint(pool proxypool.Pool, stateDir, accountID string, candidates []int, ttl time.Duration) (proxypool.Endpoint, int, error) {
	if !pool.SelectsPort() {
		endpoint, err := pool.At(0)
		return endpoint, 0, err
	}
	port, err := proxypool.Allocate(stateDir, accountID, candidates, ttl)
	if err != nil {
		return proxypool.Endpoint{}, 0, err
	}
	endpoint, err := pool.At(port)
	if err != nil {
		// The table now holds a port this endpoint cannot use; giving it back
		// keeps a bad template from burning a lease.
		_ = proxypool.Release(stateDir, port)
		return proxypool.Endpoint{}, 0, err
	}
	return endpoint, port, nil
}

// releasePort gives a lease back, and treats "no lease at all" as nothing to do:
// 711 and local pools never allocate one.
func releasePort(stateDir string, port int) error {
	if port == 0 {
		return nil
	}
	return proxypool.Release(stateDir, port)
}

// withoutPort returns candidates without port, in a fresh slice so the caller's
// list is not mutated under it.
func withoutPort(candidates []int, port int) []int {
	kept := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != port {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// resolveStateDir decides where the port table lives. A relative -state resolves
// against the working directory; the directory itself is created on first use, so
// a dry run never has to.
func resolveStateDir(spec string) (string, error) {
	dir := strings.TrimSpace(spec)
	if dir == "" {
		dir = defaultStateDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("state dir %q: %w", dir, err)
	}
	return abs, nil
}

// buildPool turns the flags into the routing decision. The provider is checked
// against the template because a 711 URL pasted into -provider dataimpulse
// would otherwise be silently rewritten into a host it never belonged to.
func buildPool(provider, template, country, ports string) (proxypool.Pool, error) {
	name := strings.ToLower(strings.TrimSpace(provider))
	switch name {
	case "dataimpulse", "di":
		name = "dataimpulse"
	case "711", "trustsource":
		name = "711"
	case "local", "clash":
		name = "local"
	default:
		return proxypool.Pool{}, fmt.Errorf("unknown provider %q (want dataimpulse, 711 or local)", provider)
	}

	country = strings.ToUpper(strings.TrimSpace(country))
	if country != "" && len(country) != 2 {
		return proxypool.Pool{}, fmt.Errorf("country %q is not a two-letter code", country)
	}

	if template == "" {
		if name != "local" {
			return proxypool.Pool{}, fmt.Errorf("provider %s needs a template: pass -url or set PROXY_TEMPLATE", name)
		}
		template = localTemplate
	}
	base, err := proxypool.Parse(template)
	if err != nil {
		return proxypool.Pool{}, err
	}

	switch name {
	case "local":
		if base.Kind != proxypool.LOCAL {
			return proxypool.Pool{}, errors.New("provider local takes a bare http://127.0.0.1:<port> template")
		}
	case "dataimpulse":
		if base.Provider() != "dataimpulse" {
			return proxypool.Pool{}, errors.New("template does not look like DataImpulse (no __cr.<cc> in the username)")
		}
	case "711":
		if base.Provider() != "711" {
			return proxypool.Pool{}, errors.New("template does not look like 711 (no -region-/-session- in the username)")
		}
	}

	leasePorts, err := proxypool.ParsePorts(ports)
	if err != nil {
		return proxypool.Pool{}, err
	}
	return proxypool.Pool{
		Country: country,
		Sticky:  name != "local",
		Ports:   leasePorts,
		Base:    base,
	}, nil
}

// codeOf names the failure for the structured error line.
func codeOf(err error) string {
	switch {
	case errors.Is(err, proxypool.ErrBlocked):
		return "blocked"
	case errors.Is(err, proxypool.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, proxypool.ErrRetryable):
		return "retryable"
	case errors.Is(err, proxypool.ErrUnexpected):
		return "unexpected"
	default:
		return "failed"
	}
}
