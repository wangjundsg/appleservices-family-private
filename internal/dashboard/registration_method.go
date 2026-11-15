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

// frozenRegistrationMethod reads the value already stored for a job, if any.
func frozenRegistrationMethod(jobDir string) (string, bool) {
	var stored struct {
		RegistrationMethod string `json:"registrationMethod"`
	}
	if readRegisterJSON(filepath.Join(jobDir, "request.json"), &stored) != nil {
		return "", false
	}
	if stored.RegistrationMethod == "" {
		return methodBrowser, true
	}
	return stored.RegistrationMethod, true
}

// registrationMethodFor decides the method for a new job or a resumed one.
// A resume may repeat the stored value, omit it, or fail; it may never switch.
func registrationMethodFor(jobDir, requested string) (string, error) {
	wanted, err := normalizeRegistrationMethod(requested)
	if err != nil {
		return "", err
	}
	if stored, ok := frozenRegistrationMethod(jobDir); ok {
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

// registrationProgram returns the executable and arguments for a job. Browser
// jobs keep the pipeline; protocol jobs call signup.exe with explicit paths.
func (s *Server) registrationProgram(jobDir, method string) (string, []string) {
	if method == methodProtocol {
		binary := filepath.Join(s.config.RepoRoot, "bin", "signup.exe")
		// Every private path is named explicitly: nothing is inferable from the
		// current directory, and two jobs never share a file.
		args := []string{
			"-registration-method", methodProtocol,
			"-state", protocolStatePath(jobDir),
			"-device-dir", filepath.Join(jobDir, "protocol", "devices"),
			"-cap-image", filepath.Join(jobDir, "protocol", "captcha.png"),
			"-email-code-file", filepath.Join(jobDir, "protocol", "email-code.txt"),
			"-phone-code-file", filepath.Join(jobDir, "protocol", "phone-code.txt"),
			"-json",
		}
		return binary, args
	}
	return s.config.Python, append([]string{filepath.Join(s.config.RepoRoot, "tools", "apple-pipeline", "pipeline.py")},
		"run", "--input", filepath.Join(jobDir, "request.json"))
}

// ensureProtocolDir creates the private directory before the executor starts.
func ensureProtocolDir(jobDir string) error {
	return os.MkdirAll(filepath.Join(jobDir, "protocol"), 0o700)
}
