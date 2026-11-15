package main

// Device state: the difference between "a browser" and "a browser that has been
// used".
//
// Measured 2026-11-15: the same US number and mailbox passed in Edge's normal
// profile and failed in an incognito Chrome window on the same network. The
// variable is browser history — cookies, storage and past visits. A protocol run
// that mints a fresh cookie jar per task looks exactly like incognito, so this
// file keeps a per-device record across tasks:
//
//   - cookies (domain/path/expiry, same shape the session already snapshots)
//   - the frozen fingerprint profile, so every visit claims the same machine
//   - age and visit counters, and a bounded path history
//
// A device file is private (0600) and contains cookie values, so it lives under
// .cache and is never committed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Laky-64/appleservices/internal/fingerprint"
)

const (
	deviceSchemaVersion = 1
	deviceHistoryLimit  = 64
	deviceTaskLimit     = 64
)

type deviceState struct {
	SchemaVersion int                  `json:"schemaVersion"`
	ID            string               `json:"id"`
	CreatedAt     time.Time            `json:"createdAt"`
	LastUsedAt    time.Time            `json:"lastUsedAt"`
	Visits        int                  `json:"visits"`
	Tasks         []string             `json:"tasks,omitempty"`
	History       []string             `json:"history,omitempty"`
	Profile       *fingerprint.Profile `json:"profile,omitempty"`
	Cookies       []cookieEvent        `json:"cookies,omitempty"`
}

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,48}$`)

// devicePath resolves the state file for one device id.
func devicePath(dir, id string) (string, error) {
	if !deviceIDPattern.MatchString(id) {
		return "", fmt.Errorf("device: id %q must be 1-48 of [A-Za-z0-9_.-]", id)
	}
	if dir == "" {
		dir = filepath.Join(".cache", "devices")
	}
	return filepath.Join(dir, id+".json"), nil
}

// loadDevice reads a device record. A missing file yields a fresh device (age
// zero); a corrupt file is an error, because silently replacing a used browser
// with a blank one would throw away exactly the signal we are trying to keep.
func loadDevice(dir, id string, now time.Time) (*deviceState, error) {
	path, err := devicePath(dir, id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &deviceState{SchemaVersion: deviceSchemaVersion, ID: id, CreatedAt: now.UTC(), LastUsedAt: now.UTC()}, nil
	}
	if err != nil {
		return nil, err
	}
	var device deviceState
	if err := json.Unmarshal(raw, &device); err != nil {
		return nil, fmt.Errorf("device: %s is not valid JSON: %w", path, err)
	}
	if device.SchemaVersion != deviceSchemaVersion {
		return nil, fmt.Errorf("device: schema %d is not supported", device.SchemaVersion)
	}
	if device.ID != id {
		return nil, fmt.Errorf("device: file belongs to %q, requested %q", device.ID, id)
	}
	return &device, nil
}

// save writes the record atomically enough for one process (write then rename).
func (d *deviceState) save(dir string) error {
	path, err := devicePath(dir, d.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// ageDays is the receipt-friendly age of this browser profile.
func (d *deviceState) ageDays(now time.Time) int {
	if d.CreatedAt.IsZero() {
		return 0
	}
	days := int(now.Sub(d.CreatedAt).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// noteVisit records one warm-up pass and the paths it walked.
func (d *deviceState) noteVisit(paths []string, now time.Time) {
	d.Visits++
	d.LastUsedAt = now.UTC()
	d.History = append(d.History, paths...)
	if len(d.History) > deviceHistoryLimit {
		d.History = d.History[len(d.History)-deviceHistoryLimit:]
	}
}

// noteTask remembers which accounts this device created, bounded.
func (d *deviceState) noteTask(email string) {
	for _, existing := range d.Tasks {
		if existing == email {
			return
		}
	}
	d.Tasks = append(d.Tasks, email)
	if len(d.Tasks) > deviceTaskLimit {
		d.Tasks = d.Tasks[len(d.Tasks)-deviceTaskLimit:]
	}
}

// captureCookies copies the session's cookie journal into the device record.
func (d *deviceState) captureCookies(session *Session) {
	if session == nil || session.client == nil {
		return
	}
	jar, ok := session.client.Jar.(*recordedJar)
	if !ok {
		return
	}
	d.Cookies = append([]cookieEvent(nil), jar.Events...)
}

// warmSession replays the device's cookies into a fresh session before the
// first request, so the visit starts with history behind it.
func (d *deviceState) warmSession(session *Session) (int, error) {
	if len(d.Cookies) == 0 || session == nil {
		return 0, nil
	}
	jar, err := newRecordedJar(d.Cookies)
	if err != nil {
		return 0, err
	}
	if session.client == nil {
		session.client = &http.Client{}
	}
	session.client.Jar = jar
	total := 0
	for _, event := range d.Cookies {
		total += len(event.Cookies)
	}
	return total, nil
}

// deviceReceipt is what a task reports about the browser it borrowed.
func deviceReceipt(d *deviceState, now time.Time, cookiesLoaded int, reusedProfile bool) map[string]any {
	if d == nil {
		return nil
	}
	profileID := ""
	if d.Profile != nil {
		profileID = d.Profile.ID
	}
	return map[string]any{
		"id":            d.ID,
		"ageDays":       d.ageDays(now),
		"visits":        d.Visits,
		"history":       len(d.History),
		"cookies":       len(d.Cookies),
		"cookiesLoaded": cookiesLoaded,
		"profile":       profileID,
		"profileReused": reusedProfile,
		"distinctTasks": len(d.Tasks),
	}
}

// ErrDeviceBlocked marks a device record that must be looked at by a human.
var ErrDeviceBlocked = errors.New("device_blocked")
