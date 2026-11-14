package appleweb

import (
	"encoding/json"
	"os"
)

func browserClientInfo(ua string) string {
	z := os.Getenv("APPLESERVICES_GMT")
	if z == "" {
		z = "GMT+08:00"
	}
	b, _ := json.Marshal(map[string]string{"U": ua, "L": langValue(), "Z": z, "V": "1.1", "F": ""})
	return string(b)
}

// Identity overrides let a caller bind the account-level fingerprint profile to
// the browser-shaped HTTP stages (web login, keepalive). The GSA path keeps its
// own anisette identity and is deliberately not affected.
//
// Empty override means "use the compiled default", so existing behaviour is
// unchanged when no profile is supplied.
var (
	identityUserAgentOverride string
	identityLanguageOverride  string
)

// SetIdentityOverride installs the per-account identity used by browser-shaped
// requests. Values are taken from the frozen profile; empty strings keep the
// defaults.
func SetIdentityOverride(userAgent, language string) {
	identityUserAgentOverride = userAgent
	identityLanguageOverride = language
}

// IdentityFromEnv reads APPLESERVICES_UA / APPLESERVICES_LOCALE so external
// callers can pass the profile without linking this package.
func IdentityFromEnv() (string, string) {
	return os.Getenv("APPLESERVICES_UA"), os.Getenv("APPLESERVICES_LOCALE")
}

func uaValue() string {
	if identityUserAgentOverride != "" {
		return identityUserAgentOverride
	}
	return appleAccountManageUserAgent
}

func langValue() string {
	if identityLanguageOverride != "" {
		return identityLanguageOverride
	}
	return appleAccountManageLanguage
}
