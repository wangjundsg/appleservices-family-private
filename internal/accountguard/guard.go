// Package accountguard is the durable, account-directory-scoped stop gate.
// Only an explicit operator release can clear it; session/cache writes cannot.
package accountguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Laky-64/appleservices/internal/privatefs"
)

const FileName = "account-guard.json"
const Message = "账号已锁定或处于人工保护中；已停止自动认证和发码，等待用户确认恢复。"

var ErrBlocked = errors.New("account_protected")

type Record struct {
	Version    int       `json:"version"`
	Blocked    bool      `json:"blocked"`
	Reason     string    `json:"reason"`
	Source     string    `json:"source"`
	BlockedAt  time.Time `json:"blocked_at"`
	ReleasedAt time.Time `json:"released_at,omitempty"`
}

func Check(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if os.IsNotExist(err) {
		return nil
	}
	var record Record
	if err != nil || json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Blocked || record.ReleasedAt.IsZero() {
		return ErrBlocked
	}
	return nil
}

// Release is deliberately not called by any automatic or web login flow.
// It does not start a worker or clear legacy verification warnings.
func Release(dir string, confirmedRecovered bool) error {
	if !confirmedRecovered {
		return errors.New("explicit recovery confirmation required")
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return errors.New("protection record unavailable")
	}
	var record Record
	if json.Unmarshal(data, &record) != nil || record.Version != 1 {
		return errors.New("invalid protection record")
	}
	if err := privatefs.EnsureDir(dir); err != nil {
		return err
	}
	record.Blocked, record.Source, record.ReleasedAt = false, "operator_confirmed_recovery", time.Now().UTC()
	data, _ = json.Marshal(record)
	f, err := os.CreateTemp(dir, ".account-guard-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, FileName))
}
