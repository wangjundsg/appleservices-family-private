package main

// Profile mode: give a task a frozen browser profile and a transport that
// actually applies it.
//
// Contract points implemented here:
//   - the profile is chosen and persisted before the first network request;
//   - retries, exit code 4, code waiting and process restarts reuse it;
//   - template upgrades cannot rewrite an existing task (validation compares
//     the stored signature and the stored template id);
//   - a task that already has an intent refuses to continue when the profile is
//     missing or corrupted, instead of silently minting a new one;
//   - receipts report the chosen profile and the applied engine separately.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Laky-64/appleservices/internal/engine"
	"github.com/Laky-64/appleservices/internal/fingerprint"
)

const (
	profileModeLegacy = "legacy"
	profileModeNew    = "new"
)

// profileConfig is what the CLI collected about the profile request.
type profileConfig struct {
	Mode          string
	Kind          string
	EgressCountry string
	EgressSource  string
	Insecure      bool
	// MigrateLegacy rewrites an unversioned preset alias (HelloChrome_Auto …) to
	// the pinned preset for the profile's exact version. Off by default: a
	// migration must be an explicit act, never a side effect of running.
	MigrateLegacy bool
}

// ErrProfileBlocked means the task must stop and be reviewed by a human; it is
// never retried with a freshly minted profile.
var ErrProfileBlocked = errors.New("profile_blocked")

// hasIntent reports whether this task has already told Apple something.
func hasIntent(st *state) bool {
	if st.Body != nil || st.Created || st.VerificationID != nil {
		return true
	}
	for _, op := range st.Operations {
		if op.Status != "" {
			return true
		}
	}
	return false
}

// installProfile freezes or restores the task profile, then swaps the session
// transport for the engine adapter. Legacy mode returns a nil profile and
// leaves the session untouched.
func installProfile(st *state, session *Session, cfg profileConfig, proxy string, timeout time.Duration) (*fingerprint.Profile, map[string]any, error) {
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" || mode == profileModeLegacy {
		return nil, nil, nil
	}
	if mode != profileModeNew {
		return nil, nil, fmt.Errorf("profile: unknown mode %q", mode)
	}
	if session.UseCurl {
		return nil, nil, errors.New("profile: the curl transport cannot apply an engine hello; drop -curl or use -profile-mode legacy")
	}

	cap := engine.Capabilities()
	migratedNote := ""

	// Frozen profile: verify, never re-select.
	if st.Profile != nil {
		profile := *st.Profile
		if profile.Signature != fingerprint.Signature(profile) {
			return nil, nil, fmt.Errorf("%w: saved profile signature mismatch", ErrProfileBlocked)
		}
		if err := fingerprint.Validate(profile, cap); err != nil {
			if _, legacy := fingerprint.LegacyAliases[profile.Engine.Name]; legacy && cfg.MigrateLegacy {
				migrated, migrateErr := fingerprint.MigrateLegacyProfile(profile, cap)
				if migrateErr != nil {
					return nil, nil, fmt.Errorf("%w: %v", ErrProfileBlocked, migrateErr)
				}
				// A migration rewrites one field; the rest of the profile still has
				// to pass the complete check. Otherwise a broken archive (wrong
				// schema, bad UA, missing hints) would ride the migration through.
				if revalidateErr := fingerprint.Validate(migrated, cap); revalidateErr != nil {
					return nil, nil, fmt.Errorf("%w: migration left an invalid profile: %v", ErrProfileBlocked, revalidateErr)
				}
				profile = migrated
				st.Profile = &profile
				migratedNote = migrated.Engine.Name
			} else {
				return nil, nil, fmt.Errorf("%w: saved profile is invalid: %v", ErrProfileBlocked, err)
			}
		}
		if want := strings.TrimSpace(cfg.Kind); want != "" && want != "auto" && fingerprint.Kind(want) != profile.Kind && !(fingerprint.Kind(want) == fingerprint.KindSafari && (profile.Kind == fingerprint.KindMacSafari || profile.Kind == fingerprint.KindIOSSafari)) {
			return nil, nil, fmt.Errorf("%w: saved profile is %s, requested %s", ErrProfileBlocked, profile.Kind, want)
		}
		transport, err := engine.New(engine.Options{Profile: profile, ProxyURL: proxy, Timeout: timeout, Insecure: cfg.Insecure, AllowLoopbackHTTP: loopbackBase(session.base)})
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrProfileBlocked, err)
		}
		if err := applyToSession(session, profile, transport); err != nil {
			return nil, nil, err
		}
		st.ProfileMode = profileModeNew
		describe := transport.Describe()
		if migratedNote != "" {
			describe["migratedFrom"] = "HelloAlias"
			describe["migratedTo"] = migratedNote
		}
		return &profile, describe, nil
	}

	// No profile yet: a task with intent must not mint one.
	if hasIntent(st) {
		return nil, nil, fmt.Errorf("%w: task already submitted but carries no profile", ErrProfileBlocked)
	}

	kind := fingerprint.Kind(strings.TrimSpace(cfg.Kind))
	if kind == "" {
		kind = fingerprint.Kind("auto")
	}
	if kind != "auto" {
		switch kind {
		case fingerprint.KindChrome, fingerprint.KindFirefox, fingerprint.KindMacSafari, fingerprint.KindIOSSafari, fingerprint.KindSafari:
		default:
			return nil, nil, fmt.Errorf("profile: unknown kind %q", cfg.Kind)
		}
	}
	country := strings.ToUpper(strings.TrimSpace(cfg.EgressCountry))
	source := strings.TrimSpace(cfg.EgressSource)
	if country == "" || source == "" {
		return nil, nil, errors.New("profile: -egress-country and -egress-source are required in new profile mode")
	}
	profile, err := fingerprint.Select(fingerprint.Options{
		Kind:     kind,
		Locale:   session.locale,
		Timezone: session.timezone,
		Region:   fingerprint.Region{Country: country, Source: source},
		Engine:   cap,
	})
	if err != nil {
		return nil, nil, err
	}
	transport, err := engine.New(engine.Options{Profile: profile, ProxyURL: proxy, Timeout: timeout, Insecure: cfg.Insecure, AllowLoopbackHTTP: loopbackBase(session.base)})
	if err != nil {
		return nil, nil, err
	}
	if err := applyToSession(session, profile, transport); err != nil {
		return nil, nil, err
	}
	st.Profile = &profile
	st.ProfileMode = profileModeNew
	return &profile, transport.Describe(), nil
}

// loopbackBase reports whether the session talks to an offline fixture. Only then
// may the engine accept plain http.
func loopbackBase(base string) bool {
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// applyToSession makes the session claim the profile: identity headers, client
// hints (Chrome only) and the engine transport.
func applyToSession(session *Session, profile fingerprint.Profile, transport http.RoundTripper) error {
	if session.fdClientInfo != "" {
		if profile.Kind != fingerprint.KindChrome {
			return errors.New("profile: fd-client-info is a Chrome artifact and cannot be paired with " + string(profile.Kind))
		}
		// A FD blob carries the UA and locale it was minted with. Once the profile
		// changes the session identity, the old blob is a contradiction, so it is
		// re-checked here rather than trusted from when it was supplied.
		var fd struct {
			U, L string
		}
		if err := json.Unmarshal([]byte(session.fdClientInfo), &fd); err != nil {
			return errors.New("profile: fd-client-info is not valid JSON")
		}
		if fd.U != profile.UserAgent {
			return errors.New("profile: fd-client-info was minted for a different user agent")
		}
		if fd.L != "" && fd.L != profile.Locale {
			return errors.New("profile: fd-client-info was minted for a different locale")
		}
	}
	session.userAgent = profile.UserAgent
	session.locale = profile.Locale
	session.timezone = profile.Timezone
	session.profileHints = profile.ClientHints
	session.CHHints = profile.ClientHints != nil
	if session.client == nil {
		session.client = &http.Client{}
	}
	session.client.Transport = transport
	session.client.CheckRedirect = engine.CheckRedirect
	session.engineApplied = true
	return nil
}

// profileReceipt is the redacted shape printed with -json. It names the profile
// and the engine separately so a receipt cannot imply that a template was
// applied when only its metadata was read.
func profileReceipt(p *fingerprint.Profile, engineInfo map[string]any) map[string]any {
	if p == nil {
		return map[string]any{"mode": profileModeLegacy}
	}
	out := map[string]any{
		"mode":     profileModeNew,
		"id":       p.ID,
		"template": p.TemplateID,
		"kind":     string(p.Kind),
		"os":       p.OS,
		"major":    p.Major,
		"locale":   p.Locale,
		"timezone": p.Timezone,
		"region":   p.Region,
		"source":   p.Source.ContractVersion,
		"hints":    p.ClientHints != nil,
	}
	if engineInfo != nil {
		out["engine"] = engineInfo
	}
	return out
}
