package fingerprint

// The registry carries two layers of templates:
//
//  1. A's declared templates, verbatim in version and weight. They describe the
//     browser population A samples from: Chrome 150-154, Firefox 133/144,
//     macOS Safari 15.3-18.0, iOS Safari 17.2/18.0.
//  2. Engine-reachable fallbacks for browsers newer than the engine can
//     emulate. utls 1.8.2 tops out at Chrome 133 / Firefox 120 / Safari 16.0 /
//     iOS 14, so with that engine every layer-1 template is filtered out and
//     only these survive — which is exactly the contract's rule "filter by what
//     the runtime supports, then renormalise the weights".
//
// A profile always records the template it came from, so an audit can tell
// whether a task ran on A's population or on an engine fallback.

const (
	chromeUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36"
	firefoxUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%s.0) Gecko/20100101 Firefox/%s.0"
	macSafariUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%s Safari/605.1.15"
	iosSafariUA = "Mozilla/5.0 (iPhone; CPU iPhone OS %s like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%s Mobile/15E148 Safari/604.1"
)

func chromeTemplate(version string, major int, weight int, hello string) Template {
	return Template{
		ID:             "chrome-win-" + version,
		Kind:           KindChrome,
		OS:             "Windows",
		BrowserVersion: version,
		Major:          major,
		Weight:         weight,
		UserAgent:      sprintf(chromeUA, version),
		Platform:       "Win32",
		ClientHints: &ClientHints{
			Brands:   `"Not-A.Brand";v="99", "Google Chrome";v="` + version + `", "Chromium";v="` + version + `"`,
			Mobile:   false,
			Platform: `"Windows"`,
		},
		Language:  "en-US",
		Timezones: []string{"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles"},
		Screen:    Screen{Width: 1920, Height: 1080, Scale: 1},
		Engineering: EngineHello{
			Name:    hello,
			Minimum: "1.6",
		},
	}
}

func firefoxTemplate(version string, major int, weight int) Template {
	return Template{
		ID:             "firefox-win-" + version,
		Kind:           KindFirefox,
		OS:             "Windows",
		BrowserVersion: version,
		Major:          major,
		Weight:         weight,
		UserAgent:      sprintf(firefoxUA, version, version),
		Platform:       "Win32",
		Language:       "en-US",
		Timezones:      []string{"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles"},
		Screen:         Screen{Width: 1920, Height: 1080, Scale: 1},
		Engineering:    EngineHello{Name: "HelloFirefox_120", Minimum: "1.6"},
	}
}

func macSafariTemplate(version string, major int, weight int) Template {
	return Template{
		ID:             "mac-safari-" + version,
		Kind:           KindMacSafari,
		OS:             "macOS",
		BrowserVersion: version,
		Major:          major,
		Weight:         weight,
		UserAgent:      sprintf(macSafariUA, version),
		Platform:       "MacIntel",
		Language:       "en-US",
		Timezones:      []string{"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles"},
		Screen:         Screen{Width: 1680, Height: 1050, Scale: 2},
		Engineering:    EngineHello{Name: "HelloSafari_16_0", Minimum: "1.6"},
	}
}

func iosSafariTemplate(version string, osVersion string, major int, weight int) Template {
	return Template{
		ID:             "ios-safari-" + version,
		Kind:           KindIOSSafari,
		OS:             "iOS",
		BrowserVersion: version,
		Major:          major,
		Weight:         weight,
		UserAgent:      sprintf(iosSafariUA, osVersion, version),
		Platform:       "iPhone",
		Language:       "en-US",
		Timezones:      []string{"America/New_York", "America/Chicago", "America/Denver"},
		Screen:         Screen{Width: 390, Height: 844, Scale: 3},
		Engineering:    EngineHello{Name: "HelloIOS_14", Minimum: "1.6"},
	}
}

// registry is the full table. Order matters only for deterministic iteration.
var registry = []Template{
	// Layer 1: A's declared population (contract §3).
	// Layer 1 keeps A's versions. They sit above what utls can pose as today, so
	// selection filters them out; the pinned preset is only a placeholder for the
	// day an engine can reach them.
	chromeTemplate("150", 150, 2, "HelloChrome_133"),
	chromeTemplate("151", 151, 3, "HelloChrome_133"),
	chromeTemplate("152", 152, 5, "HelloChrome_133"),
	chromeTemplate("153", 153, 10, "HelloChrome_133"),
	chromeTemplate("154", 154, 80, "HelloChrome_133"),
	firefoxTemplate("133", 133, 50),
	firefoxTemplate("144", 144, 50),
	macSafariTemplate("15.3", 15, 25),
	macSafariTemplate("15.5", 15, 25),
	macSafariTemplate("17.0", 17, 25),
	macSafariTemplate("18.0", 18, 25),
	iosSafariTemplate("17.2", "17_2", 17, 50),
	iosSafariTemplate("18.0", "18_0", 18, 50),

	// Layer 2: engine-reachable fallbacks (same shape, older major version).
	chromeTemplate("131", 131, 50, "HelloChrome_131"),
	chromeTemplate("133", 133, 50, "HelloChrome_133"),
	firefoxTemplate("120", 120, 100),
	macSafariTemplate("16.0", 16, 100),
	iosSafariTemplate("14.0", "14_0", 14, 100),
}

// Catalogue returns a copy of every template, for receipts and tests.
func Catalogue() []Template {
	out := make([]Template, len(registry))
	copy(out, registry)
	return out
}
