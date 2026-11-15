// Package fingerprint builds the browser profile a registration task claims.
//
// It is a deliberately small, self-contained port of the profile configuration
// described in the protocol-A contract
// (D:\Workbench\apps\gpt-free-service\docs\PROTOCOL-A-FINGERPRINT-CONTRACT-2026-11-13.md,
// contract version 2026-11-13). Only the configuration model is shared:
// templates, weights, UA/Client-Hints/locale/timezone coupling and the profile
// identity/signature. A's registration logic, retry policy, global patches and
// runtime state are out of scope, and nothing here reads that repository at
// runtime.
//
// Two things this package does NOT claim:
//   - that the weights below predict Apple's acceptance (they are A's sampling
//     weights, carried over unchanged);
//   - that a profile alone makes a request look like a browser. Applying it is
//     the engine adapter's job; a profile that no engine consumes is metadata.
package fingerprint

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// ContractVersion and ContractPath record where this model came from, so a
// later audit can tell which rules a stored profile was created under.
const (
	SchemaVersion   = 1
	ContractVersion = "2026-11-13"
	ContractPath    = `D:\Workbench\apps\gpt-free-service\docs\PROTOCOL-A-FINGERPRINT-CONTRACT-2026-11-13.md`
)

// Kind is one profile type. Safari is split by operating system because the UA,
// the Client-Hints rules and the engine hello differ.
type Kind string

const (
	KindChrome    Kind = "chrome"
	KindFirefox   Kind = "firefox"
	KindMacSafari Kind = "mac_safari"
	KindIOSSafari Kind = "ios_safari"
)

// KindSafari is accepted on input only: the contract resolves an explicit
// "safari" request to mac_safari/ios_safari with equal chance.
const KindSafari Kind = "safari"

// AllKinds is the sampling order; it also pins the iteration order of any
// place that walks the family table.
var AllKinds = []Kind{KindChrome, KindFirefox, KindMacSafari, KindIOSSafari}

// FamilyWeight is A's family sampling weight (percent). It is a sampling
// probability, not a batch quota, and it is renormalised over the families the
// engine can actually emulate.
var FamilyWeight = map[Kind]int{
	KindChrome:    95,
	KindFirefox:   2,
	KindMacSafari: 2,
	KindIOSSafari: 1,
}

// ClientHints is what a Chrome build sends. Firefox and both Safari types must
// leave it nil â€?sending Chrome hints under a Firefox UA is the exact
// inconsistency the contract's validation rejects.
type ClientHints struct {
	Brands          string `json:"brands"`
	Mobile          bool   `json:"mobile"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platformVersion,omitempty"`
}

// Screen is configuration metadata only. Nothing in the request path consumes
// it, so a receipt must not present it as "applied".
type Screen struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Scale  int `json:"scale"`
}

// EngineHello is the engine-level profile a kind needs. The engine adapter
// refuses to build a transport for an unknown hello, which is how "the profile
// was applied" stays checkable instead of a claim.
type EngineHello struct {
	Name    string `json:"name"`    // e.g. utls HelloChrome_Auto
	Minimum string `json:"minimum"` // minimum engine version that can emulate it
}

// Template is one selectable configuration.
type Template struct {
	ID             string       `json:"id"`
	Kind           Kind         `json:"kind"`
	OS             string       `json:"os"`
	BrowserVersion string       `json:"browserVersion"`
	Major          int          `json:"major"`
	Weight         int          `json:"weight"`
	UserAgent      string       `json:"userAgent"`
	Platform       string       `json:"platform"`
	ClientHints    *ClientHints `json:"clientHints,omitempty"`
	Language       string       `json:"language"`
	Timezones      []string     `json:"timezones"`
	Screen         Screen       `json:"screen"`
	Engineering    EngineHello  `json:"engine"`
}

// Region records where the account's egress is believed to be, and how that was
// established. It never comes from a proxy hostname or port.
type Region struct {
	Country string `json:"country"` // ISO-3166 alpha-2, upper case
	Source  string `json:"source"`  // explicit | verified-egress
}

// Source pins the model version and a signature of the template table, so a
// template change cannot silently rewrite what an old task was created under.
type Source struct {
	ContractVersion string `json:"contractVersion"`
	ContractPath    string `json:"contractPath"`
	TemplatesSHA256 string `json:"templatesSHA256"`
}

// Profile is the frozen per-task identity.
type Profile struct {
	SchemaVersion  int          `json:"schemaVersion"`
	ID             string       `json:"id"`
	TemplateID     string       `json:"templateId"`
	Kind           Kind         `json:"kind"`
	OS             string       `json:"os"`
	BrowserVersion string       `json:"browserVersion"`
	Major          int          `json:"major"`
	UserAgent      string       `json:"userAgent"`
	Platform       string       `json:"platform"`
	ClientHints    *ClientHints `json:"clientHints,omitempty"`
	Locale         string       `json:"locale"`   // en-US
	Language       string       `json:"language"` // en_US, for the account payload
	Timezone       string       `json:"timezone"` // America/New_York
	AcceptLanguage string       `json:"acceptLanguage"`
	Region         Region       `json:"region"`
	Engine         EngineHello  `json:"engine"`
	Screen         Screen       `json:"screen"`
	CreatedAt      time.Time    `json:"createdAt"`
	Signature      string       `json:"signature"`
	Source         Source       `json:"source"`
}

// Capability is what the request engine can actually do. Selection filters
// templates through it before any weight is applied, exactly as the contract
// requires (filter first, then renormalise).
type Capability struct {
	Name    string   `json:"name"`    // e.g. utls
	Version string   `json:"version"` // e.g. 1.8.2
	Hellos  []string `json:"hellos"`  // supported EngineHello names
	// MaxBrowser caps the browser major version the engine can emulate, per
	// kind. Zero means "no declared cap"; templates above it are filtered out
	// before weighting.
	MaxBrowser map[Kind]int `json:"maxBrowser,omitempty"`
	// HelloVersions is the exact browser version a preset poses as. A template
	// whose version differs is not reachable: "lower than the cap" is not the
	// same as "the engine has a preset for this version".
	HelloVersions map[string]string `json:"helloVersions,omitempty"`
}

// Supports reports whether this engine can emulate the given hello at all.
func (c Capability) Supports(hello string) bool {
	for _, h := range c.Hellos {
		if h == hello {
			return true
		}
	}
	return false
}

// Options drive Select. Kind may be empty (auto) or an explicit type.
type Options struct {
	Kind     Kind
	Locale   string // default en-US
	Timezone string // required
	Region   Region
	Engine   Capability
	Seed     int64 // tests pin this; production leaves it 0 (crypto random)
}

// Select returns one validated profile.
func Select(opts Options) (Profile, error) {
	locale := strings.TrimSpace(opts.Locale)
	if locale == "" {
		locale = "en-US"
	}
	if strings.TrimSpace(opts.Timezone) == "" {
		return Profile{}, errors.New("profile: timezone is required")
	}
	if opts.Region.Country == "" {
		return Profile{}, errors.New("profile: egress country is required")
	}
	if opts.Region.Source != "explicit" && opts.Region.Source != "verified-egress" {
		return Profile{}, fmt.Errorf("profile: region source %q must be explicit or verified-egress", opts.Region.Source)
	}

	candidates, err := candidatesFor(opts)
	if err != nil {
		return Profile{}, err
	}
	if len(candidates) == 0 {
		return Profile{}, errors.New("profile: no template available for the requested kind and engine")
	}

	chosen, err := pickWeighted(candidates, opts.Seed)
	if err != nil {
		return Profile{}, err
	}

	profile := Profile{
		SchemaVersion:  SchemaVersion,
		ID:             newID(),
		TemplateID:     chosen.ID,
		Kind:           chosen.Kind,
		OS:             chosen.OS,
		BrowserVersion: chosen.BrowserVersion,
		Major:          chosen.Major,
		UserAgent:      chosen.UserAgent,
		Platform:       chosen.Platform,
		ClientHints:    chosen.ClientHints,
		Locale:         locale,
		Language:       strings.ReplaceAll(locale, "-", "_"),
		Timezone:       opts.Timezone,
		AcceptLanguage: acceptLanguage(locale),
		Region:         opts.Region,
		Engine:         chosen.Engineering,
		Screen:         chosen.Screen,
		CreatedAt:      time.Now().UTC(),
		Source: Source{
			ContractVersion: ContractVersion,
			ContractPath:    ContractPath,
			TemplatesSHA256: TemplatesHash(),
		},
	}
	profile.Signature = signatureOf(profile)
	if err := Validate(profile, opts.Engine); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

// candidatesFor applies the contract's ordering: explicit kind first, engine
// support next, weights last.
func candidatesFor(opts Options) ([]Template, error) {
	kind := opts.Kind
	var familyWeights map[Kind]int

	switch kind {
	case "", "auto":
		familyWeights = map[Kind]int{}
		for _, k := range AllKinds {
			familyWeights[k] = FamilyWeight[k]
		}
	case KindSafari:
		// Explicit "safari" is split evenly between the two Safari types; the
		// 2:1 ratio belongs to auto only.
		familyWeights = map[Kind]int{KindMacSafari: 1, KindIOSSafari: 1}
	case KindChrome, KindFirefox, KindMacSafari, KindIOSSafari:
		familyWeights = map[Kind]int{kind: 1}
	default:
		return nil, fmt.Errorf("profile: unknown kind %q", kind)
	}

	// Filter 1: engine must be able to emulate the family at all — both the
	// hello and the browser major version.
	usable := map[Kind]int{}
	for k, w := range familyWeights {
		if len(reachableTemplates(k, opts.Engine)) == 0 {
			continue
		}
		usable[k] = w
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("profile: engine %s cannot emulate any requested kind", opts.Engine.Name)
	}

	family, err := pickFamily(usable, opts.Seed)
	if err != nil {
		return nil, err
	}

	// Filter 2: templates of that family the engine can actually pose as.
	out := reachableTemplates(family, opts.Engine)
	if len(out) == 0 {
		return nil, fmt.Errorf("profile: no %s template is supported by engine %s", family, opts.Engine.Name)
	}
	return out, nil
}

// reachableTemplates applies both engine limits: the hello must exist and the
// template must not be newer than the engine can emulate.
func reachableTemplates(kind Kind, engine Capability) []Template {
	var out []Template
	for _, t := range templatesOf(kind) {
		if engine.Name != "" && !engine.Supports(t.Engineering.Name) {
			continue
		}
		if limit, ok := engine.MaxBrowser[kind]; ok && limit > 0 && t.Major > limit {
			continue
		}
		if want, ok := engine.HelloVersions[t.Engineering.Name]; ok && want != t.BrowserVersion {
			continue
		}
		out = append(out, t)
	}
	return out
}

func pickFamily(weights map[Kind]int, seed int64) (Kind, error) {
	kinds := make([]Kind, 0, len(weights))
	for k := range weights {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	total := 0
	for _, k := range kinds {
		total += weights[k]
	}
	if total <= 0 {
		return "", errors.New("profile: family weights sum to zero")
	}
	n, err := roll(total, seed)
	if err != nil {
		return "", err
	}
	for _, k := range kinds {
		n -= weights[k]
		if n < 0 {
			return k, nil
		}
	}
	return kinds[len(kinds)-1], nil
}

func pickWeighted(templates []Template, seed int64) (Template, error) {
	total := 0
	for _, t := range templates {
		if t.Weight <= 0 {
			return Template{}, fmt.Errorf("profile: template %s has non-positive weight", t.ID)
		}
		total += t.Weight
	}
	n, err := roll(total, seed)
	if err != nil {
		return Template{}, err
	}
	for _, t := range templates {
		n -= t.Weight
		if n < 0 {
			return t, nil
		}
	}
	return templates[len(templates)-1], nil
}

// roll returns [0,total). A non-zero seed makes the selection reproducible for
// tests; production uses crypto randomness so two tasks do not correlate.
func roll(total int, seed int64) (int, error) {
	if total <= 0 {
		return 0, errors.New("profile: empty weight table")
	}
	if seed != 0 {
		return int((seed%int64(total) + int64(total)) % int64(total)), nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(total)))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Validate enforces the coupling rules the contract lists: UA shape per kind,
// no Chrome hints on non-Chrome kinds, locale/timezone consistency and a hello
// the engine can actually build.
func Validate(p Profile, engine Capability) error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("profile: schema %d is not supported", p.SchemaVersion)
	}
	if !strings.HasPrefix(p.ID, "fp_") {
		return fmt.Errorf("profile: id %q is not a task profile id", p.ID)
	}
	if p.TemplateID == "" {
		return errors.New("profile: template id is missing")
	}
	if !strings.Contains(p.UserAgent, p.BrowserVersion) {
		return fmt.Errorf("profile: user agent does not carry browser version %s", p.BrowserVersion)
	}
	switch p.Kind {
	case KindChrome:
		if p.OS != "Windows" {
			return fmt.Errorf("profile: chrome template must be Windows, got %s", p.OS)
		}
		if !strings.Contains(p.UserAgent, "Windows NT 10.0; Win64; x64") || !strings.Contains(p.UserAgent, "Chrome/") {
			return errors.New("profile: chrome user agent is inconsistent with Windows")
		}
		if p.ClientHints == nil {
			return errors.New("profile: chrome profile must carry client hints")
		}
		if p.ClientHints.Platform != `"Windows"` {
			return fmt.Errorf("profile: chrome client hints platform %q is not Windows", p.ClientHints.Platform)
		}
	case KindFirefox:
		if p.OS != "Windows" {
			return fmt.Errorf("profile: firefox template must be Windows, got %s", p.OS)
		}
		if !strings.Contains(p.UserAgent, "Firefox/") || !strings.Contains(p.UserAgent, "Windows NT 10.0") {
			return errors.New("profile: firefox user agent is inconsistent with Windows")
		}
		if p.ClientHints != nil {
			return errors.New("profile: firefox must not carry Chrome client hints")
		}
	case KindMacSafari:
		if p.OS != "macOS" {
			return fmt.Errorf("profile: mac_safari template must be macOS, got %s", p.OS)
		}
		if !strings.Contains(p.UserAgent, "Macintosh") || !strings.Contains(p.UserAgent, "Safari/") {
			return errors.New("profile: mac_safari user agent is inconsistent with macOS")
		}
		if p.ClientHints != nil {
			return errors.New("profile: mac_safari must not carry Chrome client hints")
		}
	case KindIOSSafari:
		if p.OS != "iOS" {
			return fmt.Errorf("profile: ios_safari template must be iOS, got %s", p.OS)
		}
		if !strings.Contains(p.UserAgent, "iPhone") || !strings.Contains(p.UserAgent, "Mobile/") {
			return errors.New("profile: ios_safari user agent is inconsistent with iOS")
		}
		if p.ClientHints != nil {
			return errors.New("profile: ios_safari must not carry Chrome client hints")
		}
	default:
		return fmt.Errorf("profile: unknown kind %q", p.Kind)
	}
	if !validLocale(p.Locale) {
		return fmt.Errorf("profile: locale %q is not xx-YY", p.Locale)
	}
	if !strings.HasPrefix(p.AcceptLanguage, p.Locale) {
		return fmt.Errorf("profile: accept-language %q does not start with %s", p.AcceptLanguage, p.Locale)
	}
	if p.Timezone == "" {
		return errors.New("profile: timezone is missing")
	}
	if p.Region.Country == "" || (p.Region.Source != "explicit" && p.Region.Source != "verified-egress") {
		return fmt.Errorf("profile: region %+v is not acceptable", p.Region)
	}
	if p.Engine.Name == "" {
		return errors.New("profile: engine hello is missing")
	}
	if engine.Name != "" {
		if !engine.Supports(p.Engine.Name) {
			return fmt.Errorf("profile: engine %s cannot emulate %s", engine.Name, p.Engine.Name)
		}
		// Selection filters by exact preset version; a restored profile has to be
		// held to the same rule, or an old档案 (Safari 15.3 riding the 16.0 preset)
		// would be waved through on resume.
		if want, ok := engine.HelloVersions[p.Engine.Name]; ok && want != p.BrowserVersion {
			return fmt.Errorf("profile: hello %s poses as %s but the profile claims %s", p.Engine.Name, want, p.BrowserVersion)
		}
	}
	if signatureOf(p) != p.Signature {
		return errors.New("profile: signature mismatch (content was modified)")
	}
	return nil
}

// MajorHint is the browser major version this profile claims, used by the
// engine adapter to refuse a hello it cannot pair with.
func (p Profile) MajorHint() int {
	if p.Major > 0 {
		return p.Major
	}
	return 0
}

// LegacyAliases are the unversioned preset names earlier builds wrote into
// profiles. They are not accepted on restore: an alias follows the library
// upgrade, so a profile minted as "Auto" no longer means anything specific.
var LegacyAliases = map[string]string{
	"HelloChrome_Auto":  "chrome",
	"HelloFirefox_Auto": "firefox",
	"HelloSafari_Auto":  "mac_safari",
	"HelloIOS_Auto":     "ios_safari",
}

// MigrateLegacyProfile rewrites an alias to the pinned preset for the profile's
// exact version. It only succeeds when a preset with that version exists, and
// the caller must persist the result: nothing here is silent.
func MigrateLegacyProfile(p Profile, engine Capability) (Profile, error) {
	kind, ok := LegacyAliases[p.Engine.Name]
	if !ok {
		return Profile{}, fmt.Errorf("profile: %s is not a legacy alias", p.Engine.Name)
	}
	if kind != string(p.Kind) {
		return Profile{}, fmt.Errorf("profile: alias %s belongs to %s, profile is %s", p.Engine.Name, kind, p.Kind)
	}
	for hello, version := range engine.HelloVersions {
		if version != p.BrowserVersion {
			continue
		}
		if !engine.Supports(hello) {
			continue
		}
		// Only pair with a preset of the same family.
		if len(hello) >= len("Hello") && containsKind(hello, kind) {
			p.Engine = EngineHello{Name: hello, Minimum: p.Engine.Minimum}
			p.Signature = signatureOf(p)
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("profile: no pinned preset for %s %s; refusing to re-roll the fingerprint", p.Kind, p.BrowserVersion)
}

func containsKind(hello, kind string) bool {
	switch kind {
	case "chrome":
		return strings.Contains(hello, "Chrome")
	case "firefox":
		return strings.Contains(hello, "Firefox")
	case "mac_safari":
		return strings.Contains(hello, "Safari")
	case "ios_safari":
		return strings.Contains(hello, "IOS")
	}
	return false
}

// Signature is the exported form, used by the task state to detect tampering.
func Signature(p Profile) string { return signatureOf(p) }

// signatureOf hashes the profile without its id and creation time, as the
// contract specifies (the signature must not cover the per-task id).
func signatureOf(p Profile) string {
	copyable := p
	copyable.ID = ""
	copyable.CreatedAt = time.Time{}
	copyable.Signature = ""
	raw, err := json.Marshal(copyable)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TemplatesHash signs the template table so a later template change is visible
// in every new profile's Source block.
func TemplatesHash() string {
	raw, err := json.Marshal(registry)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "fp_" + hex.EncodeToString([]byte(fmt.Sprint(time.Now().UnixNano())))
	}
	return "fp_" + hex.EncodeToString(buf)
}

func acceptLanguage(locale string) string {
	base := locale
	if idx := strings.Index(locale, "-"); idx > 0 {
		base = locale[:idx]
	}
	return locale + "," + base + ";q=0.9"
}

func validLocale(locale string) bool {
	parts := strings.Split(locale, "-")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return false
	}
	for _, r := range parts[0] + parts[1] {
		if r < 'A' || (r > 'Z' && r < 'a') || r > 'z' {
			return false
		}
	}
	return true
}

func templatesOf(kind Kind) []Template {
	var out []Template
	for _, t := range registry {
		if t.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// sprintf keeps the template literals readable without importing fmt in that file.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
