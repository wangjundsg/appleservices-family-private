package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestGenerateValidates is the base promise: whatever the seed, the profile the
// generator freezes passes its own validation and the signals agree with each
// other by construction, not only by the validator's good will.
func TestGenerateValidates(t *testing.T) {
	for _, seed := range []string{"acct-01", "acct-02", "acct-03", "acct-04", "acct-05", "acct-06", "", "seed with spaces"} {
		p, err := Generate(seed)
		if err != nil {
			t.Fatalf("seed %q: generate: %v", seed, err)
		}
		if err := Validate(p); err != nil {
			t.Fatalf("seed %q: validate: %v", seed, err)
		}
		major := p.ChromeMajor()
		if major == "" || !strings.Contains(p.UACH, `v="`+major+`"`) {
			t.Fatalf("seed %q: ua and ua_ch disagree: ua=%q ua_ch=%q", seed, p.UA, p.UACH)
		}
		switch p.Platform {
		case "Win32":
			if !strings.Contains(p.UA, "Windows NT 10.0") {
				t.Fatalf("seed %q: Win32 without a Windows ua: %q", seed, p.UA)
			}
		case "MacIntel":
			if !strings.Contains(p.UA, "Macintosh") {
				t.Fatalf("seed %q: MacIntel without a Macintosh ua: %q", seed, p.UA)
			}
		default:
			t.Fatalf("seed %q: unexpected platform %q", seed, p.Platform)
		}
		if p.UAMobile != "?0" || strings.Contains(p.UA, "Mobile") {
			t.Fatalf("seed %q: desktop profile claims mobile: %q / %q", seed, p.UAMobile, p.UA)
		}
		if !strings.HasPrefix(p.Timezone, "America/") || p.Locale != "en-US" || p.Languages[0] != "en-US" {
			t.Fatalf("seed %q: us profile is not us: timezone=%q locale=%q languages=%v", seed, p.Timezone, p.Locale, p.Languages)
		}
	}
}

// TestGenerateIsDeterministic keeps "one seed, one machine" true: without it a
// retry would silently mint a second fingerprint for the same account.
func TestGenerateIsDeterministic(t *testing.T) {
	first, err := Generate("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate("acct-01")
	if err != nil {
		t.Fatal(err)
	}
	first.CreatedAt, second.CreatedAt = "", ""
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced two different profiles")
	}
	other, err := Generate("acct-02")
	if err != nil {
		t.Fatal(err)
	}
	if other.UA == first.UA && other.Screen == first.Screen && other.Timezone == first.Timezone && other.Cores == first.Cores {
		t.Fatal("different seeds produced the same fingerprint")
	}
}

// TestValidateRejectsTamperedSignals walks the rules one by one: every signal
// the frozen profile promises must be able to fail loudly on its own.
func TestValidateRejectsTamperedSignals(t *testing.T) {
	base, err := Generate("acct-tamper")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{
			name:   "ua version",
			mutate: func(p *Profile) { p.UA = uaChromeMajor.ReplaceAllString(p.UA, "Chrome/99") },
			want:   "ua_ch does not list the UA chrome major 99",
		},
		{
			name:   "ua without chrome token",
			mutate: func(p *Profile) { p.UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Safari/537.36" },
			want:   `ua has no "Chrome/<major>" token`,
		},
		{
			name:   "ua_ch without entries",
			mutate: func(p *Profile) { p.UACH = "" },
			want:   `ua_ch has no "Brand";v="<major>" client-hint entry`,
		},
		{
			// The GREASE brand must never confirm a UA: its version is noise.
			name:   "ua_ch grease only",
			mutate: func(p *Profile) { p.UACH = `"Not?A_Brand";v="` + p.ChromeMajor() + `"` },
			want:   "ua_ch does not list the UA chrome major",
		},
		{
			name: "platform contradicts ua",
			mutate: func(p *Profile) {
				if p.Platform == "Win32" {
					p.Platform = "MacIntel"
				} else {
					p.Platform = "Win32"
				}
			},
			want: "in the ua",
		},
		{
			name:   "platform value",
			mutate: func(p *Profile) { p.Platform = "Linux x86_64" },
			want:   "neither Win32 nor MacIntel",
		},
		{
			name:   "ua_mobile",
			mutate: func(p *Profile) { p.UAMobile = "?1" },
			want:   "contradicts desktop platform",
		},
		{
			name:   "mobile ua",
			mutate: func(p *Profile) { p.UA = p.UA + " Mobile/15E148" },
			want:   "contradicts a mobile ua",
		},
		{
			name:   "timezone",
			mutate: func(p *Profile) { p.Timezone = "Europe/London" },
			want:   `timezone "Europe/London" is not one of`,
		},
		{
			name:   "locale",
			mutate: func(p *Profile) { p.Locale = "en-GB" },
			want:   `locale "en-GB" is not en-US`,
		},
		{
			name:   "languages",
			mutate: func(p *Profile) { p.Languages = []string{"en-GB", "en"} },
			want:   "must start with en-US",
		},
		{
			name:   "empty languages",
			mutate: func(p *Profile) { p.Languages = nil },
			want:   "must start with en-US",
		},
		{
			name:   "screen",
			mutate: func(p *Profile) { p.Screen = [2]int{1280, 720} },
			want:   "screen 1280x720 is not one of",
		},
		{
			name:   "cores",
			mutate: func(p *Profile) { p.Cores = 6 },
			want:   "cores 6 is not one of",
		},
		{
			name:   "memory",
			mutate: func(p *Profile) { p.Memory = 12 },
			want:   "memory 12 is not one of",
		},
		{
			name:   "webgl empty",
			mutate: func(p *Profile) { p.WebGL.Vendor = "" },
			want:   "must both be set",
		},
		{
			name: "webgl mismatch",
			mutate: func(p *Profile) {
				p.WebGL.Vendor = "Google Inc. (Intel)"
				p.WebGL.Renderer = gpus[1].renderer // NVIDIA renderer
			},
			want: "does not match renderer family",
		},
		{
			name:   "webgl unknown family",
			mutate: func(p *Profile) { p.WebGL.Vendor = "Google Inc. (Apple)" },
			want:   "names no known GPU family",
		},
		{
			name:   "schema version",
			mutate: func(p *Profile) { p.Version = 2 },
			want:   "version 2 is not supported",
		},
	}
	if err := Validate(base); err != nil {
		t.Fatalf("base profile does not validate: %v", err)
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := base
			tampered.Languages = append([]string(nil), base.Languages...)
			testCase.mutate(&tampered)
			err := Validate(tampered)
			if err == nil {
				t.Fatalf("%s: tampered profile was accepted", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("%s: error %q does not mention %q", testCase.name, err, testCase.want)
			}
		})
	}
}

// TestLoadOrCreateIsIdempotent: the second call reads the frozen file instead of
// minting a new fingerprint, and says so through the created flag.
func TestLoadOrCreateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, created, err := LoadOrCreate(dir, "acct-01")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !created {
		t.Fatal("first call did not report creation")
	}
	rawFirst := readFile(t, Path(dir))

	second, created, err := LoadOrCreate(dir, "a-different-seed")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if created {
		t.Fatal("second call regenerated an already frozen profile")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second call returned another profile: %+v != %+v", first, second)
	}
	if rawSecond := readFile(t, Path(dir)); !bytes.Equal(rawFirst, rawSecond) {
		t.Fatal("second call rewrote the frozen file")
	}

	// An empty seed falls back to the directory name.
	named := filepath.Join(t.TempDir(), "acct-07")
	fromDir, created, err := LoadOrCreate(named, "")
	if err != nil {
		t.Fatalf("directory-seeded call: %v", err)
	}
	if !created {
		t.Fatal("directory-seeded call did not create the profile")
	}
	fromSeed, err := Generate("acct-07")
	if err != nil {
		t.Fatal(err)
	}
	if fromDir.UA != fromSeed.UA || fromDir.Platform != fromSeed.Platform ||
		fromDir.Timezone != fromSeed.Timezone || fromDir.Screen != fromSeed.Screen ||
		fromDir.Cores != fromSeed.Cores || fromDir.Memory != fromSeed.Memory {
		t.Fatalf("empty seed did not use the directory name: %+v vs %+v", fromDir, fromSeed)
	}
}

// TestLoadOrCreateRefusesBrokenProfile: a profile edited after freezing (or a
// corrupt one) must be refused, not silently trusted by the next stage.
func TestLoadOrCreateRefusesBrokenProfile(t *testing.T) {
	dir := t.TempDir()
	frozen, created, err := LoadOrCreate(dir, "acct-01")
	if err != nil || !created {
		t.Fatalf("setup: created=%t err=%v", created, err)
	}

	frozen.UAMobile = "?1"
	data, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dir, "acct-01"); err == nil {
		t.Fatal("tampered profile was accepted")
	}

	if err := os.WriteFile(Path(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dir, "acct-01"); err == nil {
		t.Fatal("corrupt profile was accepted")
	}
}

// TestObserveTouchesOnlyActual keeps Expected and Actual separate: recording
// what ran must not disturb the frozen expectations around it.
func TestObserveTouchesOnlyActual(t *testing.T) {
	dir := t.TempDir()
	frozen, _, err := LoadOrCreate(dir, "acct-01")
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, Path(dir))

	route := EngineRoute{
		Engine:    "chrome-" + frozen.ChromeMajor(),
		EgressRef: "proxy-us-3",
		EgressIP:  "203.0.113.7",
	}
	if err := Observe(dir, route); err != nil {
		t.Fatalf("observe: %v", err)
	}
	after := readFile(t, Path(dir))

	if !bytes.Equal(withoutActual(t, before), withoutActual(t, after)) {
		t.Fatalf("observe changed fields outside actual:\nbefore=%s\nafter=%s", before, after)
	}

	observed, err := Load(dir)
	if err != nil {
		t.Fatalf("load after observe: %v", err)
	}
	if observed.Actual.Engine != route.Engine || observed.Actual.EgressRef != route.EgressRef || observed.Actual.EgressIP != route.EgressIP {
		t.Fatalf("actual section does not hold the observation: %+v", observed.Actual)
	}
	if observed.Actual.ObservedAt == "" {
		t.Fatal("observe did not stamp observed_at")
	}

	// A later stage that only knows the engine keeps the measured exit address.
	if err := Observe(dir, EngineRoute{Engine: "go-tls"}); err != nil {
		t.Fatalf("second observe: %v", err)
	}
	merged, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Actual.Engine != "go-tls" || merged.Actual.EgressIP != route.EgressIP || merged.Actual.EgressRef != route.EgressRef {
		t.Fatalf("second observe dropped earlier evidence: %+v", merged.Actual)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(Path(dir))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("profile mode is %v, want 0600", info.Mode().Perm())
		}
	}
}

func readFile(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// withoutActual re-encodes every top-level field except "actual" with its
// original raw bytes, so the comparison is byte-exact per field rather than
// merely structural.
func withoutActual(t *testing.T, raw []byte) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	delete(fields, "actual")
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("re-encode profile: %v", err)
	}
	return out
}
