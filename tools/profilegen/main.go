// Command profilegen creates, checks and updates the frozen fingerprint of one
// account directory (see internal/profile).
//
// The tool exists so the account creation form and the later protocol stages
// read one file instead of each generating its own browser identity: the first
// stage that runs freezes the profile, every stage after it reads the same
// values back.
//
// Usage:
//
//	go run ./tools/profilegen -dir .cache/acct-01
//	go run ./tools/profilegen -dir .cache/acct-01 -json
//	go run ./tools/profilegen -dir .cache/acct-01 -check
//	go run ./tools/profilegen -dir .cache/acct-01 -observe-engine chrome-142
//	go run ./tools/profilegen -dir .cache/acct-01 -observe-engine chrome-142 \
//	    -observe-egress-ref proxy-us-3 -observe-egress-ip 203.0.113.7
//
// Failures are reported as one JSON object on stderr with a non-zero exit, so a
// driving script can tell a rejected profile from a broken invocation.
//
// This layer holds fingerprint fields only, and the tool keeps it that way: it
// prints no cookies, tokens or passwords, because a profile has none.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Laky-64/appleservices/internal/profile"
)

func main() {
	dir := flag.String("dir", "", "account directory that holds profile.json (required)")
	seed := flag.String("seed", "", "generation seed (default: the directory name)")
	asJSON := flag.Bool("json", false, "print the full profile JSON")
	checkOnly := flag.Bool("check", false, "only validate the frozen profile and print ok or the error")
	engine := flag.String("observe-engine", "", "engine actually used, e.g. chrome-142")
	egressRef := flag.String("observe-egress-ref", "", "egress reference from the configuration")
	egressIP := flag.String("observe-egress-ip", "", "measured egress address")
	flag.Parse()

	if strings.TrimSpace(*dir) == "" {
		fail("", fmt.Errorf("-dir is required"))
	}
	accountDir := filepath.Clean(*dir)

	if *checkOnly {
		p, err := profile.Load(accountDir)
		if err != nil {
			fail(accountDir, err)
		}
		fmt.Printf("ok %s version=%d chrome=%s platform=%s timezone=%s\n",
			profile.Path(accountDir), p.Version, p.ChromeMajor(), p.Platform, p.Timezone)
		return
	}

	p, created, err := profile.LoadOrCreate(accountDir, *seed)
	if err != nil {
		fail(accountDir, err)
	}

	if *engine != "" || *egressRef != "" || *egressIP != "" {
		route := profile.EngineRoute{Engine: *engine, EgressRef: *egressRef, EgressIP: *egressIP}
		if err := profile.Observe(accountDir, route); err != nil {
			fail(accountDir, err)
		}
		// Read back instead of assuming the merge: what a later stage sees is
		// the file, not our intent.
		if p, err = profile.Load(accountDir); err != nil {
			fail(accountDir, err)
		}
	}

	if *asJSON {
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			fail(accountDir, err)
		}
		fmt.Println(string(data))
		return
	}
	printSummary(accountDir, p, created)
}

func printSummary(dir string, p profile.Profile, created bool) {
	fmt.Printf("%-9s %s\n", "profile", profile.Path(dir))
	fmt.Printf("%-9s %t\n", "created", created)
	fmt.Printf("%-9s %d\n", "version", p.Version)
	fmt.Printf("%-9s %s\n", "ua", p.UA)
	fmt.Printf("%-9s %s\n", "ua_ch", p.UACH)
	fmt.Printf("%-9s %s (mobile %s)\n", "platform", p.Platform, p.UAMobile)
	fmt.Printf("%-9s %s %v\n", "locale", p.Locale, p.Languages)
	fmt.Printf("%-9s %s\n", "timezone", p.Timezone)
	fmt.Printf("%-9s %dx%d\n", "screen", p.Screen[0], p.Screen[1])
	fmt.Printf("%-9s %d\n", "cores", p.Cores)
	fmt.Printf("%-9s %d\n", "memory", p.Memory)
	fmt.Printf("%-9s %s / %s\n", "webgl", p.WebGL.Vendor, p.WebGL.Renderer)
	fmt.Printf("%-9s %s\n", "expected", routeLine(p.Expected))
	fmt.Printf("%-9s %s\n", "actual", routeLine(p.Actual))
}

func routeLine(r profile.EngineRoute) string {
	parts := make([]string, 0, 4)
	if r.Engine != "" {
		parts = append(parts, "engine="+r.Engine)
	}
	if r.EgressRef != "" {
		parts = append(parts, "egress_ref="+r.EgressRef)
	}
	if r.EgressIP != "" {
		parts = append(parts, "egress_ip="+r.EgressIP)
	}
	if r.ObservedAt != "" {
		parts = append(parts, "observed_at="+r.ObservedAt)
	}
	if len(parts) == 0 {
		return "not observed"
	}
	return strings.Join(parts, " ")
}

// failure is the structured error a driving script parses.
type failure struct {
	Tool  string `json:"tool"`
	Dir   string `json:"dir,omitempty"`
	Error string `json:"error"`
}

func fail(dir string, err error) {
	data, marshalErr := json.Marshal(failure{Tool: "profilegen", Dir: dir, Error: err.Error()})
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "profilegen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, string(data))
	os.Exit(1)
}
