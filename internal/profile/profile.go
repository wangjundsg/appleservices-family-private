// Package profile stores the per-account browser fingerprint that every stage
// of an Apple Account flow has to agree on.
//
// Apple cross-checks the user agent, the client hints and navigator.platform
// against each other, so one account must keep presenting one machine from the
// creation form all the way into the later protocol stages. The fingerprint is
// therefore generated once from a seed, validated, and frozen as
// <account-dir>/profile.json: later stages read that file instead of inventing
// their own UA, which is how two stages of one account stop looking like two
// different computers.
//
// What we intend to send (Expected) and what we actually measured (Actual) are
// deliberately separate structs. A disagreement between them is evidence worth
// reporting; it must not be silently papered over by a later write.
//
// This package holds fingerprint fields only. Cookies, tokens and passwords
// belong to the protocol layers and never enter a profile file.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// Version is the schema version this package writes and accepts. A file
	// written by another version is refused rather than half-understood.
	Version = 1

	// FileName is the frozen profile file inside an account directory.
	FileName = "profile.json"
)

// Profile is the frozen fingerprint of one account.
type Profile struct {
	Version   int      `json:"version"` // 1
	CreatedAt string   `json:"created_at"`
	UA        string   `json:"ua"`
	UACH      string   `json:"ua_ch"`     // Sec-CH-UA raw value
	UAMobile  string   `json:"ua_mobile"` // "?0" / "?1"
	Platform  string   `json:"platform"`  // "Win32" / "MacIntel"
	Locale    string   `json:"locale"`    // en-US
	Languages []string `json:"languages"`
	Timezone  string   `json:"timezone"` // America/New_York
	Screen    [2]int   `json:"screen"`
	Cores     int      `json:"cores"`
	Memory    int      `json:"memory"`
	WebGL     struct {
		Vendor   string `json:"vendor"`
		Renderer string `json:"renderer"`
	} `json:"webgl"`
	Expected EngineRoute `json:"expected"`
	Actual   EngineRoute `json:"actual"`
}

// EngineRoute records which engine and which egress address a stage used.
// Expected comes from the configuration, Actual from the running process; the
// pair is what lets a later stage ask "did we really leave from where the
// profile said we would?".
type EngineRoute struct {
	Engine     string `json:"engine,omitempty"`     // e.g. chrome-142 / go-tls
	EgressRef  string `json:"egress_ref,omitempty"` // configuration's egress reference
	EgressIP   string `json:"egress_ip,omitempty"`  // measured exit address, may stay empty
	ObservedAt string `json:"observed_at,omitempty"`
}

// Allowed values. The tables are the US-resident desktop population the
// creation form is meant to look like; keeping them closed is what makes the
// cross-checks below decidable.
var (
	platforms = []struct {
		name string
		ua   string // %V% is replaced by the full chrome version
	}{
		// Windows only. The pipeline drives a real Windows Chrome; a macOS UA
		// would advertise MacIntel while navigator.platform still reports
		// Win32, which is the exact contradiction the rules below reject.
		{"Win32", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%V% Safari/537.36"},
	}

	// Chrome build distribution mirrored from the reference workbench: five
	// majors with a long tail, 154 carrying most of the mass. The table is
	// expanded by weight so the picker stays a plain index draw and the JS
	// mirror in create-account.cjs can build the identical slice.
	chromeVersionWeights = []struct {
		version string
		weight  int
	}{
		{"150.0.0.0", 2},
		{"151.0.0.0", 3},
		{"152.0.0.0", 5},
		{"153.0.0.0", 10},
		{"154.0.0.0", 80},
	}

	chromeVersions    = expandChromeVersions()
	knownChromeMajors = chromeMajors(chromeVersions)

	gpus = []struct {
		family   string
		vendor   string
		renderer string
	}{
		{"Intel", "Google Inc. (Intel)", "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
		{"NVIDIA", "Google Inc. (NVIDIA)", "ANGLE (NVIDIA, NVIDIA GeForce GTX 1650 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
		{"AMD", "Google Inc. (AMD)", "ANGLE (AMD, AMD Radeon(TM) Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	}

	allowedScreens   = [][2]int{{1366, 768}, {1440, 900}, {1536, 864}, {1600, 900}, {1920, 1080}}
	allowedTimezones = []string{"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles"}
	allowedCores     = []int{4, 8, 12, 16}
	allowedMemory    = []int{8, 16}
)

// The two parsers below are the only places a version is read out of a UA or a
// client-hint string. Validate, ChromeMajor and every caller that builds a
// "chrome-<major>" engine label go through them, so the label can never
// disagree with the UA it was derived from.
var (
	uaChromeMajor = regexp.MustCompile(`Chrome/(\d+)`)
	// chBrand reads one "Brand";v="<version>" pair. The brand is kept because
	// the GREASE entry ("Not?A_Brand";v="99") carries a version too, and a UA
	// that happens to say 99 would otherwise be "confirmed" by a hint that
	// never described Chrome at all.
	chBrand = regexp.MustCompile(`"([^"]*)"\s*;\s*v="(\d+)"`)
)

// Path returns the frozen profile path inside an account directory.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// ChromeMajor returns the Chrome major version the UA advertises, or "" when
// the UA carries no Chrome/<version> token.
func (p Profile) ChromeMajor() string {
	match := uaChromeMajor.FindStringSubmatch(p.UA)
	if match == nil {
		return ""
	}
	return match[1]
}

// Validate enforces the coherence the frozen profile promises: the signals
// Apple compares must describe the same machine, and the values must stay
// inside the closed tables the generator draws from. Each rule below exists
// because a stage that reads this file will ship these values as if they were
// one device — a mismatch here becomes a contradiction in the wire traffic.
func Validate(p Profile) error {
	if p.Version != Version {
		return fmt.Errorf("profile: version %d is not supported, want %d", p.Version, Version)
	}

	// 1. UA and UA-CH must advertise the same Chrome build. A UA that says 142
	//    while the hints say 140 is a plain contradiction Apple can read.
	major := p.ChromeMajor()
	if major == "" {
		return errors.New(`profile: ua has no "Chrome/<major>" token`)
	}
	brands := chBrand.FindAllStringSubmatch(p.UACH, -1)
	if len(brands) == 0 {
		return errors.New(`profile: ua_ch has no "Brand";v="<major>" client-hint entry`)
	}
	hintHasMajor := false
	for _, brand := range brands {
		if isGREASE(brand[1]) {
			continue
		}
		if brand[2] == major {
			hintHasMajor = true
			break
		}
	}
	if !hintHasMajor {
		return fmt.Errorf("profile: ua_ch does not list the UA chrome major %s (entries: %s)", major, strings.Join(hintVersions(brands), ", "))
	}

	// 2. navigator.platform must match the system token in the UA.
	// Roxy/Forge generates the fingerprint, so a coherent macOS profile is now a
	// legitimate input: the UA and navigator.platform come from the same source
	// instead of being patched together by us.
	switch p.Platform {
	case "Win32":
		if !strings.Contains(p.UA, "Windows NT 10.0") {
			return fmt.Errorf("profile: platform %s needs %q in the ua", p.Platform, "Windows NT 10.0")
		}
	case "MacIntel":
		if !strings.Contains(p.UA, "Macintosh") {
			return fmt.Errorf("profile: platform %s needs %q in the ua", p.Platform, "Macintosh")
		}
	default:
		return fmt.Errorf("profile: platform %q is neither Win32 nor MacIntel", p.Platform)
	}
	// The major must come from the frozen table: a hand-edited file must not be
	// able to invent a build no stage ever measured.
	if !knownChromeMajors[major] {
		return fmt.Errorf("profile: chrome major %s is outside the frozen table", major)
	}

	// 3. The UA-CH mobile hint has to agree with the UA itself. Both allowed
	//    platforms are desktops, so "?1" and a mobile UA are both contradictions.
	if p.UAMobile != "?0" && p.UAMobile != "?1" {
		return fmt.Errorf(`profile: ua_mobile %q is neither "?0" nor "?1"`, p.UAMobile)
	}
	if p.UAMobile == "?1" {
		return fmt.Errorf(`profile: ua_mobile "?1" contradicts desktop platform %s, want "?0"`, p.Platform)
	}
	if strings.Contains(p.UA, "Mobile") {
		return fmt.Errorf(`profile: ua_mobile "?0" contradicts a mobile ua`)
	}

	// 4. A US account must sit in a US timezone; an off-continent clock next to
	//    en-US is one of the cheapest inconsistencies to spot.
	if !contains(allowedTimezones, p.Timezone) {
		return fmt.Errorf("profile: timezone %q is not one of %s", p.Timezone, strings.Join(allowedTimezones, ", "))
	}

	// 5. Locale and the language list must both be US English, same reason.
	if p.Locale != "en-US" {
		return fmt.Errorf("profile: locale %q is not en-US", p.Locale)
	}
	if len(p.Languages) == 0 || p.Languages[0] != "en-US" {
		return fmt.Errorf("profile: languages %v must start with en-US", p.Languages)
	}

	// 6. Screen size comes from a closed list of ordinary laptop panels, so a
	//    screen no real panel has cannot leak in.
	if !containsScreen(p.Screen) {
		return fmt.Errorf("profile: screen %dx%d is not one of %s", p.Screen[0], p.Screen[1], screenList())
	}

	// 7. Core and memory counts are the consumer configurations the UA claims.
	if !contains(allowedCores, p.Cores) {
		return fmt.Errorf("profile: cores %d is not one of %v", p.Cores, allowedCores)
	}
	if !contains(allowedMemory, p.Memory) {
		return fmt.Errorf("profile: memory %d is not one of %v", p.Memory, allowedMemory)
	}

	// 8. Vendor and renderer are two halves of one GPU string; when they name
	//    different vendors the WebGL readout contradicts itself.
	if err := validateWebGL(p.WebGL.Vendor, p.WebGL.Renderer); err != nil {
		return err
	}
	return nil
}

func validateWebGL(vendor, renderer string) error {
	if vendor == "" || renderer == "" {
		return fmt.Errorf("profile: webgl vendor %q and renderer %q must both be set", vendor, renderer)
	}
	vendorFamily := ""
	for _, gpu := range gpus {
		if strings.Contains(vendor, gpu.family) {
			vendorFamily = gpu.family
			break
		}
	}
	if vendorFamily == "" {
		return fmt.Errorf("profile: webgl vendor %q names no known GPU family (Intel, NVIDIA, AMD)", vendor)
	}
	rendererFamily := ""
	for _, gpu := range gpus {
		if strings.HasPrefix(renderer, "ANGLE ("+gpu.family+",") {
			rendererFamily = gpu.family
			break
		}
	}
	if rendererFamily == "" {
		return fmt.Errorf("profile: webgl renderer %q is not an ANGLE string for Intel, NVIDIA or AMD", renderer)
	}
	if rendererFamily != vendorFamily {
		return fmt.Errorf("profile: webgl vendor %q does not match renderer family %s", vendor, rendererFamily)
	}
	return nil
}

// isGREASE reports whether a client-hint brand is one of the deliberately fake
// GREASE entries ("Not?A_Brand", "Not-A.Brand", "Not)A;Brand"). Their version
// is noise, so it never satisfies the UA/UA-CH agreement.
func isGREASE(brand string) bool {
	return strings.HasPrefix(brand, "Not") && strings.Contains(brand, "Brand")
}

func hintVersions(brands [][]string) []string {
	out := make([]string, 0, len(brands))
	for _, brand := range brands {
		out = append(out, brand[1]+"="+brand[2])
	}
	return out
}

func contains[T comparable](list []T, want T) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func containsScreen(want [2]int) bool { return contains(allowedScreens, want) }

func screenList() string {
	parts := make([]string, 0, len(allowedScreens))
	for _, screen := range allowedScreens {
		parts = append(parts, fmt.Sprintf("%dx%d", screen[0], screen[1]))
	}
	return strings.Join(parts, ", ")
}

// picker is a deterministic stream over a seed. The same seed always produces
// the same profile, so a re-run of one account directory cannot mint a second
// machine for it.
type picker struct{ state uint32 }

func newPicker(seed string) *picker {
	// FNV-1a keeps the seed -> stream mapping stable across Go versions and is
	// the same mixing create-account.cjs uses, so both generators agree.
	state := uint32(2166136261)
	for i := 0; i < len(seed); i++ {
		state ^= uint32(seed[i])
		state *= 16777619
	}
	return &picker{state: state}
}

// next advances the stream and returns an index in [0,n).
func (p *picker) next(n int) int {
	p.state = p.state*1664525 + 1013904223
	return int(p.state>>16) % n
}

// Generate builds a self-consistent profile from seed: the Chrome version in
// the UA and in the hints, the platform token and navigator.platform, and the
// US timezone next to en-US all come from one pick each, so they cannot drift
// apart.
//
// The pick order is part of the contract: create-account.cjs walks the same
// tables in the same order, so a given seed yields the same profile from
// either tool.
func Generate(seed string) (Profile, error) {
	stream := newPicker(seed)
	platform := platforms[stream.next(len(platforms))]
	version := chromeVersions[stream.next(len(chromeVersions))]
	major, _, _ := strings.Cut(version, ".")
	gpu := gpus[stream.next(len(gpus))]
	screen := allowedScreens[stream.next(len(allowedScreens))]
	timezone := allowedTimezones[stream.next(len(allowedTimezones))]
	cores := allowedCores[stream.next(len(allowedCores))]
	memory := allowedMemory[stream.next(len(allowedMemory))]

	p := Profile{
		Version:   Version,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UA:        strings.ReplaceAll(platform.ua, "%V%", version),
		UACH:      fmt.Sprintf(`"Chromium";v=%q, "Google Chrome";v=%q, "Not?A_Brand";v="99"`, major, major),
		UAMobile:  "?0",
		Platform:  platform.name,
		Locale:    "en-US",
		Languages: []string{"en-US", "en"},
		Timezone:  timezone,
		Screen:    screen,
		Cores:     cores,
		Memory:    memory,
		// Expected starts as the engine the UA already claims; the egress
		// reference is filled in by whoever holds the proxy configuration.
		Expected: EngineRoute{Engine: "chrome-" + major},
	}
	p.WebGL.Vendor = gpu.vendor
	p.WebGL.Renderer = gpu.renderer

	if err := Validate(p); err != nil {
		// The tables above are closed, so an invalid result means they were
		// edited into an inconsistent state. Fail here instead of freezing a
		// file every later stage would reject.
		return Profile{}, fmt.Errorf("profile: generated an invalid profile: %w", err)
	}
	return p, nil
}

// Load reads and validates the frozen profile in dir. Validation happens on
// read as well as on write: a file edited after freezing must be refused by
// every stage, not quietly steer one of them away from the others.
func Load(dir string) (Profile, error) {
	file := Path(dir)
	data, err := os.ReadFile(file)
	if err != nil {
		return Profile{}, fmt.Errorf("profile: read: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("profile: decode %s: %w", file, err)
	}
	if err := Validate(p); err != nil {
		return Profile{}, fmt.Errorf("%s: %w", file, err)
	}
	return p, nil
}

// Save freezes p into dir. The write goes to a temporary file in the same
// directory and is renamed into place, so a crash or a concurrent reader never
// sees half a profile; 0600 keeps the fingerprint out of other accounts that
// share the volume.
func Save(dir string, p Profile) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("profile: empty account directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("profile: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("profile: encode: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".profile-*.json")
	if err != nil {
		return fmt.Errorf("profile: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	discard := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)
		return cause
	}
	if err := tmp.Chmod(0o600); err != nil {
		return discard(fmt.Errorf("profile: chmod %s: %w", tmpName, err))
	}
	if _, err := tmp.Write(data); err != nil {
		return discard(fmt.Errorf("profile: write %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return discard(fmt.Errorf("profile: sync %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("profile: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, Path(dir)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("profile: rename into %s: %w", Path(dir), err)
	}
	return nil
}

// LoadOrCreate returns the frozen profile of dir, generating and freezing one
// from seed when the file does not exist yet. created reports whether this call
// wrote the file, so a caller can tell "the fingerprint of this account" from
// "a fingerprint this run just minted".
func LoadOrCreate(dir string, seed string) (Profile, bool, error) {
	if strings.TrimSpace(dir) == "" {
		return Profile{}, false, errors.New("profile: empty account directory")
	}
	p, err := Load(dir)
	switch {
	case err == nil:
		return p, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return Profile{}, false, err
	}
	if seed == "" {
		seed = filepath.Base(filepath.Clean(dir))
	}
	p, err = Generate(seed)
	if err != nil {
		return Profile{}, false, err
	}
	if err := Save(dir, p); err != nil {
		return Profile{}, false, err
	}
	return p, true, nil
}

// Observe records what actually ran into the Actual section. Only the fields
// the caller supplies overwrite anything, so observing the engine from one
// stage does not erase an egress address another stage already measured; the
// rest of the profile is written back untouched.
func Observe(dir string, actual EngineRoute) error {
	p, err := Load(dir)
	if err != nil {
		return err
	}
	if actual.Engine != "" {
		p.Actual.Engine = actual.Engine
	}
	if actual.EgressRef != "" {
		p.Actual.EgressRef = actual.EgressRef
	}
	if actual.EgressIP != "" {
		p.Actual.EgressIP = actual.EgressIP
	}
	if actual.ObservedAt != "" {
		p.Actual.ObservedAt = actual.ObservedAt
	} else {
		p.Actual.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return Save(dir, p)
}

// expandChromeVersions materialises the weighted table into a plain slice.
func expandChromeVersions() []string {
	var out []string
	for _, entry := range chromeVersionWeights {
		for i := 0; i < entry.weight; i++ {
			out = append(out, entry.version)
		}
	}
	return out
}

// chromeMajors returns the set of majors the table can produce.
func chromeMajors(versions []string) map[string]bool {
	out := make(map[string]bool, len(versions))
	for _, version := range versions {
		major, _, _ := strings.Cut(version, ".")
		out[major] = true
	}
	return out
}
