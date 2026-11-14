package appleweb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ProfileIdentity reads the account-level frozen profile written by the
// producer pipeline. Missing or unreadable profile means "no override".
func ProfileIdentity(dir string) (ua, lang string) {
	raw, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		return "", ""
	}
	var doc struct {
		UA     string `json:"ua"`
		Locale string `json:"locale"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", ""
	}
	return doc.UA, doc.Locale
}

// RecordActual stores what this run actually used (engine derived from the UA)
// without touching the expected section.
func RecordActual(dir, engine string) {
	path := filepath.Join(dir, "profile.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	actual, _ := doc["actual"].(map[string]any)
	if actual == nil {
		actual = map[string]any{}
	}
	if engine != "" {
		actual["engine"] = engine
	}
	actual["observed_at"] = time.Now().UTC().Format(time.RFC3339)
	doc["actual"] = actual
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0o600)
}

// EngineFromUA turns the UA's Chrome major into the engine label used in the
// profile's actual section.
func EngineFromUA(ua string) string {
	m := regexp.MustCompile(`Chrome/(\d+)`).FindStringSubmatch(ua)
	if len(m) != 2 {
		return ""
	}
	return "chrome-" + m[1]
}
