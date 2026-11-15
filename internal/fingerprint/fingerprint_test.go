package fingerprint

import (
	"testing"
)

// utlsCap mirrors what internal/engine reports today; the numbers here are the
// contract's "filter by runtime support" input.
func utlsCap() Capability {
	return Capability{
		Name:    "utls",
		Version: "1.8.2",
		Hellos:  []string{"HelloChrome_131", "HelloChrome_133", "HelloFirefox_120", "HelloSafari_16_0", "HelloIOS_14"},
		HelloVersions: map[string]string{
			"HelloChrome_131": "131", "HelloChrome_133": "133",
			"HelloFirefox_120": "120", "HelloSafari_16_0": "16.0", "HelloIOS_14": "14.0",
		},
		MaxBrowser: map[Kind]int{
			KindChrome:    133,
			KindFirefox:   120,
			KindMacSafari: 16,
			KindIOSSafari: 14,
		},
	}
}

func TestFamilyWeightsMatchContract(t *testing.T) {
	sum := 0
	for _, k := range AllKinds {
		sum += FamilyWeight[k]
	}
	if sum != 100 {
		t.Fatalf("family weights sum to %d, want 100", sum)
	}
	for _, pair := range []struct {
		kind Kind
		want int
	}{{KindChrome, 95}, {KindFirefox, 2}, {KindMacSafari, 2}, {KindIOSSafari, 1}} {
		if FamilyWeight[pair.kind] != pair.want {
			t.Fatalf("%s weight = %d, want %d", pair.kind, FamilyWeight[pair.kind], pair.want)
		}
	}
}

func TestVersionWeightsMatchContract(t *testing.T) {
	// Layer 1 only: A's declared versions.
	want := map[string]int{
		"chrome-win-150": 2, "chrome-win-151": 3, "chrome-win-152": 5,
		"chrome-win-153": 10, "chrome-win-154": 80,
		"firefox-win-133": 50, "firefox-win-144": 50,
		"mac-safari-15.3": 25, "mac-safari-15.5": 25, "mac-safari-17.0": 25, "mac-safari-18.0": 25,
		"ios-safari-17.2": 50, "ios-safari-18.0": 50,
	}
	seen := map[string]int{}
	for _, tpl := range Catalogue() {
		if w, ok := want[tpl.ID]; ok {
			seen[tpl.ID] = tpl.Weight
			if tpl.Weight != w {
				t.Fatalf("%s weight = %d, want %d", tpl.ID, tpl.Weight, w)
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("registry is missing declared templates: got %d of %d", len(seen), len(want))
	}
}

func TestSelectProducesAllFourKinds(t *testing.T) {
	opts := Options{
		Locale:   "en-US",
		Timezone: "America/New_York",
		Region:   Region{Country: "US", Source: "verified-egress"},
		Engine:   utlsCap(),
	}
	for _, kind := range AllKinds {
		o := opts
		o.Kind = kind
		p, err := Select(o)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if p.Kind != kind {
			t.Fatalf("%s: got kind %s", kind, p.Kind)
		}
		if err := Validate(p, opts.Engine); err != nil {
			t.Fatalf("%s: validate: %v", kind, err)
		}
		if p.Major > utlsCap().MaxBrowser[kind] {
			t.Fatalf("%s: selected major %d above engine cap %d", kind, p.Major, utlsCap().MaxBrowser[kind])
		}
	}
}

func TestNonChromeKindsCarryNoClientHints(t *testing.T) {
	opts := Options{
		Locale: "en-US", Timezone: "America/New_York",
		Region: Region{Country: "US", Source: "explicit"},
		Engine: utlsCap(),
	}
	for _, kind := range []Kind{KindFirefox, KindMacSafari, KindIOSSafari, KindSafari} {
		o := opts
		o.Kind = kind
		p, err := Select(o)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if p.ClientHints != nil {
			t.Fatalf("%s: profile carries client hints", p.Kind)
		}
	}
	o := opts
	o.Kind = KindChrome
	p, err := Select(o)
	if err != nil {
		t.Fatalf("chrome: %v", err)
	}
	if p.ClientHints == nil || p.ClientHints.Platform != `"Windows"` {
		t.Fatalf("chrome profile must carry Windows client hints, got %+v", p.ClientHints)
	}
}

func TestExplicitKindNeedsEngineSupport(t *testing.T) {
	chromeOnly := Capability{
		Name: "utls-fake", Version: "0",
		Hellos:     []string{"HelloChrome_131"},
		MaxBrowser: map[Kind]int{KindChrome: 133},
	}
	if _, err := Select(Options{Kind: KindChrome, Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: chromeOnly}); err != nil {
		t.Fatalf("chrome should be reachable: %v", err)
	}
	if _, err := Select(Options{Kind: KindFirefox, Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: chromeOnly}); err == nil {
		t.Fatal("firefox must be refused when the engine has no Firefox hello")
	}
}

// TestTemplatesNeedAnExactPreset pins the review finding: "not newer than the
// cap" is not enough. Safari 15.3/15.5 used to be selectable while the only
// reachable preset was Safari 16.0, so the profile claimed a version the socket
// could not pose as.
func TestTemplatesNeedAnExactPreset(t *testing.T) {
	engine := utlsCap()

	cases := []struct {
		kind      Kind
		wantVer   string
		wantHello string
	}{
		{KindMacSafari, "16.0", "HelloSafari_16_0"},
		{KindIOSSafari, "14.0", "HelloIOS_14"},
		{KindFirefox, "120", "HelloFirefox_120"},
	}
	for _, tc := range cases {
		opts := Options{Kind: tc.kind, Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: engine}
		for seed := int64(1); seed <= 20; seed++ {
			opts.Seed = seed
			p, err := Select(opts)
			if err != nil {
				t.Fatalf("%s seed %d: %v", tc.kind, seed, err)
			}
			if p.BrowserVersion != tc.wantVer {
				t.Fatalf("%s seed %d: selected %s, want %s", tc.kind, seed, p.BrowserVersion, tc.wantVer)
			}
			if p.Engine.Name != tc.wantHello {
				t.Fatalf("%s seed %d: hello %s does not match %s", tc.kind, seed, p.Engine.Name, tc.wantVer)
			}
		}
	}

	// Chrome: only 131/133 have presets, and the hello must match the version.
	opts := Options{Kind: KindChrome, Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: engine}
	seen := map[string]string{}
	for seed := int64(1); seed <= 60; seed++ {
		opts.Seed = seed
		p, err := Select(opts)
		if err != nil {
			t.Fatalf("chrome seed %d: %v", seed, err)
		}
		seen[p.BrowserVersion] = p.Engine.Name
		if p.Major > 133 {
			t.Fatalf("chrome %d has no engine preset but was selected", p.Major)
		}
	}
	if seen["131"] != "HelloChrome_131" || seen["133"] != "HelloChrome_133" {
		t.Fatalf("chrome version/hello pairing is wrong: %v", seen)
	}
	if len(seen) != 2 {
		t.Fatalf("chrome selection reached versions without a preset: %v", seen)
	}
}
func TestAutoSelectionRenormalisesAroundUnsupportedKinds(t *testing.T) {
	// An engine with Chrome only: auto must produce chrome every time.
	cap := Capability{Name: "utls-fake", Hellos: []string{"HelloChrome_131"}, MaxBrowser: map[Kind]int{KindChrome: 133}}
	for seed := int64(1); seed <= 20; seed++ {
		p, err := Select(Options{Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: cap, Seed: seed})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if p.Kind != KindChrome {
			t.Fatalf("seed %d: auto picked %s without an engine hello for it", seed, p.Kind)
		}
	}
}

func TestSelectionIsDeterministicForASeed(t *testing.T) {
	opts := Options{Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: utlsCap(), Seed: 4242}
	a, err := Select(opts)
	if err != nil {
		t.Fatalf("%v", err)
	}
	b, err := Select(opts)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if a.TemplateID != b.TemplateID {
		t.Fatalf("same seed produced %s and %s", a.TemplateID, b.TemplateID)
	}
	if a.ID == b.ID {
		t.Fatal("two tasks must not share a profile id")
	}
}

func TestValidateRejectsTamperingAndBadInputs(t *testing.T) {
	p, err := Select(Options{Kind: KindChrome, Timezone: "America/New_York", Region: Region{Country: "US", Source: "explicit"}, Engine: utlsCap()})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := Validate(p, utlsCap()); err != nil {
		t.Fatalf("baseline profile should validate: %v", err)
	}

	tampered := p
	tampered.UserAgent = "Mozilla/5.0 (X11; Linux x86_64) Chrome/133.0.0.0"
	if err := Validate(tampered, utlsCap()); err == nil {
		t.Fatal("a mutated user agent must fail validation")
	}

	badRegion := p
	badRegion.Region = Region{Country: "US", Source: "guessed"}
	badRegion.Signature = Signature(badRegion)
	if err := Validate(badRegion, utlsCap()); err == nil {
		t.Fatal("region source must be explicit or verified-egress")
	}

	noHints := p
	noHints.ClientHints = nil
	noHints.Signature = Signature(noHints)
	if err := Validate(noHints, utlsCap()); err == nil {
		t.Fatal("a chrome profile without client hints must be refused")
	}

	hintsOnFirefox := p
	firefox := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) Gecko/20100101 Firefox/120.0"
	hintsOnFirefox.Kind = KindFirefox
	hintsOnFirefox.OS = "Windows"
	hintsOnFirefox.UserAgent = firefox
	hintsOnFirefox.BrowserVersion = "120"
	hintsOnFirefox.Engine = EngineHello{Name: "HelloFirefox_120", Minimum: "1.6"}
	hintsOnFirefox.Signature = Signature(hintsOnFirefox)
	if err := Validate(hintsOnFirefox, utlsCap()); err == nil {
		t.Fatal("firefox must not carry chrome client hints")
	}
}

func TestSelectRequiresRegionAndTimezone(t *testing.T) {
	base := Options{Region: Region{Country: "US", Source: "explicit"}, Timezone: "America/New_York", Engine: utlsCap()}
	noTZ := base
	noTZ.Timezone = ""
	if _, err := Select(noTZ); err == nil {
		t.Fatal("timezone is required")
	}
	noCountry := base
	noCountry.Region = Region{Country: "", Source: "explicit"}
	if _, err := Select(noCountry); err == nil {
		t.Fatal("egress country is required")
	}
}

func TestTemplatesHashIsStableAndProvenanced(t *testing.T) {
	h1, h2 := TemplatesHash(), TemplatesHash()
	if h1 == "" || h1 != h2 {
		t.Fatalf("template hash is not stable: %q vs %q", h1, h2)
	}
	p, err := Select(Options{Kind: KindChrome, Timezone: "America/New_York", Region: Region{Country: "US", Source: "verified-egress"}, Engine: utlsCap()})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if p.Source.ContractVersion != ContractVersion || p.Source.TemplatesSHA256 != h1 {
		t.Fatalf("profile provenance is incomplete: %+v", p.Source)
	}
}
