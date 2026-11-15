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

// Protocol jobs still go through the pipeline: it owns purchasing, refunds,
// cleanup and cancellation, and hands the adapter its explicit private paths.
func TestProtocolJobRunsThroughThePipeline(t *testing.T) {
	srv := &Server{config: Config{RepoRoot: `C:\repo`, Python: "python"}}
	job := filepath.Join(t.TempDir(), "reg-1")

	program, args := srv.registrationProgram(job, methodProtocol)
	if !strings.Contains(program, "python") || !strings.Contains(strings.Join(args, " "), "pipeline.py") {
		t.Fatalf("protocol job bypassed the pipeline: %s %v", program, args)
	}
	paths := protocolPaths(job)
	for _, key := range []string{"state", "deviceDir", "captcha", "emailCode", "phoneCode"} {
		if !strings.HasPrefix(paths[key], filepath.Join(job, "protocol")) {
			t.Fatalf("%s escapes the job directory: %s", key, paths[key])
		}
	}
	browserProgram, browserArgs := srv.registrationProgram(job, methodBrowser)
	if program != browserProgram || strings.Join(args, " ") != strings.Join(browserArgs, " ") {
		t.Fatalf("both paths must use the same management entry: %s %v", browserProgram, browserArgs)
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

// A manifest that exists but cannot be parsed must not be treated as a new task.
func TestBrokenManifestIsNotANewTask(t *testing.T) {
	job := t.TempDir()
	if err := os.WriteFile(filepath.Join(job, "request.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registrationMethodFor(job, ""); err == nil {
		t.Fatal("a broken manifest was accepted as a new task")
	}
	if _, err := registrationMethodFor(job, methodProtocol); err == nil {
		t.Fatal("a broken manifest accepted a new path")
	}
}

// A stored value this panel cannot run is refused, not echoed back.
func TestStoredInvalidMethodIsRefused(t *testing.T) {
	job := t.TempDir()
	if err := writeRegisterJSON(filepath.Join(job, "request.json"), map[string]any{"registrationMethod": "carrier-pigeon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := registrationMethodFor(job, ""); err == nil {
		t.Fatal("a stored invalid method was accepted")
	}
}
