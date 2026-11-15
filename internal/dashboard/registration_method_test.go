package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The choice is made once, at creation, and stored with the job.
func TestRegistrationMethodFreezesAtCreation(t *testing.T) {
	dir := t.TempDir()

	method, err := registrationMethodFor(dir, "")
	if err != nil || method != methodBrowser {
		t.Fatalf("empty request = %q, %v; want browser", method, err)
	}
	method, err = registrationMethodFor(dir, methodProtocol)
	if err != nil || method != methodProtocol {
		t.Fatalf("explicit protocol = %q, %v", method, err)
	}
	if _, err := registrationMethodFor(dir, "carrier-pigeon"); err == nil {
		t.Fatal("an unknown method was accepted")
	}

	// Frozen value wins on resume.
	job := t.TempDir()
	if err := writeRegisterJSON(filepath.Join(job, "request.json"), map[string]any{"registrationMethod": methodProtocol}); err != nil {
		t.Fatal(err)
	}
	if got, err := registrationMethodFor(job, ""); err != nil || got != methodProtocol {
		t.Fatalf("resume without a value = %q, %v", got, err)
	}
	if got, err := registrationMethodFor(job, methodProtocol); err != nil || got != methodProtocol {
		t.Fatalf("resume repeating the value = %q, %v", got, err)
	}
	if _, err := registrationMethodFor(job, methodBrowser); err == nil {
		t.Fatal("a resume switched the frozen method")
	}

	// Legacy job without the field keeps working as browser.
	legacy := t.TempDir()
	if err := writeRegisterJSON(filepath.Join(legacy, "request.json"), map[string]any{"email": "x@example.test"}); err != nil {
		t.Fatal(err)
	}
	if got, err := registrationMethodFor(legacy, ""); err != nil || got != methodBrowser {
		t.Fatalf("legacy job = %q, %v", got, err)
	}
}

// Protocol jobs get an explicit private state path and never touch the pipeline.
func TestProtocolJobGetsItsOwnExecutor(t *testing.T) {
	srv := &Server{config: Config{RepoRoot: `C:\repo`, Python: "python"}}
	job := filepath.Join(t.TempDir(), "reg-1")

	program, args := srv.registrationProgram(job, methodProtocol)
	if !strings.HasSuffix(program, "signup.exe") {
		t.Fatalf("protocol job runs %q", program)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-registration-method protocol") {
		t.Fatalf("protocol flag missing: %v", args)
	}
	want := filepath.Join(job, "protocol", "state.json")
	if !strings.Contains(joined, want) {
		t.Fatalf("explicit state path missing: %v (want %s)", args, want)
	}
	for _, arg := range args {
		if strings.Contains(arg, "pipeline.py") {
			t.Fatalf("protocol job fell back to the pipeline: %v", args)
		}
	}
	// Every private artefact lives under the job's protocol directory.
	for _, name := range []string{"devices", "captcha.png", "email-code.txt", "phone-code.txt"} {
		if !strings.Contains(joined, filepath.Join(job, "protocol", name)) {
			t.Fatalf("%s is not under the job's private directory: %v", name, args)
		}
	}

	browserProgram, browserArgs := srv.registrationProgram(job, methodBrowser)
	if !strings.Contains(browserProgram, "python") || !strings.Contains(strings.Join(browserArgs, " "), "pipeline.py") {
		t.Fatalf("browser job no longer uses the pipeline: %s %v", browserProgram, browserArgs)
	}
}

func TestProtocolDirIsPrivate(t *testing.T) {
	job := t.TempDir()
	if err := ensureProtocolDir(job); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(job, "protocol"))
	if err != nil || !info.IsDir() {
		t.Fatalf("protocol dir: %v", err)
	}
}
