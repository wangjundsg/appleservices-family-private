package dashboard

// I1-A: the registration path is a property of the panel task, not of the
// executor binary. The panel validates the choice, freezes it in request.json,
// and refuses a later request that asks for another path. Browser tasks keep
// using the pipeline; protocol tasks are handed to signup.exe with explicit
// private paths under the job folder.

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	methodBrowser  = "browser"
	methodProtocol = "protocol"
)

var errUnknownRegistrationMethod = errors.New("未知的注册方式；只允许 browser 或 protocol")

// normalizeRegistrationMethod turns the request value into the frozen one.
// An empty value means the historical behaviour: browser.
func normalizeRegistrationMethod(requested string) (string, error) {
	switch requested {
	case "", methodBrowser:
		return methodBrowser, nil
	case methodProtocol:
		return methodProtocol, nil
	default:
		return "", errUnknownRegistrationMethod
	}
}

// frozenRegistrationMethod reads the value already stored for a job. A manifest
// that exists but cannot be read is an error, not a new task: silently starting
// over would create a second registration for the same job folder.
func frozenRegistrationMethod(jobDir string) (string, error) {
	file := filepath.Join(jobDir, "request.json")
	var stored struct {
		RegistrationMethod string `json:"registrationMethod"`
	}
	if _, statErr := os.Stat(file); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", nil // no job yet
		}
		return "", statErr
	}
	if err := readRegisterJSON(file, &stored); err != nil {
		return "", errors.New("任务清单不可读，拒绝按新任务处理")
	}
	if stored.RegistrationMethod == "" {
		return methodBrowser, nil
	}
	if _, err := normalizeRegistrationMethod(stored.RegistrationMethod); err != nil {
		return "", err
	}
	return stored.RegistrationMethod, nil
}

// registrationMethodFor decides the method for a new job or a resumed one.
// A resume may repeat the stored value, omit it, or fail; it may never switch.
func registrationMethodFor(jobDir, requested string) (string, error) {
	wanted, err := normalizeRegistrationMethod(requested)
	if err != nil {
		return "", err
	}
	stored, err := frozenRegistrationMethod(jobDir)
	if err != nil {
		return "", err
	}
	if stored != "" {
		if requested != "" && requested != stored {
			return "", errors.New("任务已冻结为 " + stored + "，不能切换到 " + requested)
		}
		return stored, nil
	}
	return wanted, nil
}

// protocolStatePath is the private state path handed to signup.exe. The executor
// is told explicitly where to write, so it never has to guess a directory.
func protocolStatePath(jobDir string) string {
	return filepath.Join(jobDir, "protocol", "state.json")
}

// registrationProgram returns the executable and arguments for a job. The panel
// ALWAYS drives the pipeline: the pipeline owns purchasing, refunds, cleanup and
// cancellation, so a protocol job must not bypass it by calling signup.exe
// directly. The method travels in the manifest and the pipeline picks the
// adapter.
func (s *Server) registrationProgram(jobDir, method string) (string, []string) {
	return s.config.Python, append([]string{filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")},
		"run", "--input", filepath.Join(jobDir, "request.json"))
}

// protocolPaths are the private paths a protocol adapter is given explicitly.
func protocolPaths(jobDir string) map[string]string {
	base := filepath.Join(jobDir, "protocol")
	return map[string]string{
		"state":     filepath.Join(base, "state.json"),
		"deviceDir": filepath.Join(base, "devices"),
		"captcha":   filepath.Join(base, "captcha.png"),
		"emailCode": filepath.Join(base, "email-code.txt"),
		"phoneCode": filepath.Join(base, "phone-code.txt"),
	}
}

// ensureProtocolDir creates the private directory before the executor starts.
func ensureProtocolDir(jobDir string) error {
	return os.MkdirAll(filepath.Join(jobDir, "protocol"), 0o700)
}
