package launcher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStub creates a chmod-+x shell script at <dir>/<name> that prints its
// argv and ANTHROPIC_* env vars to stdout, then exits with $STUB_EXIT (or 0
// if unset).
func writeStub(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `#!/bin/sh
echo "argv: $@"
echo "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL"
echo "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY"
exit "${STUB_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_MissingBinary(t *testing.T) {
	code, err := Run(Options{
		Bin:     "definitely-not-a-real-binary-xyz",
		BaseURL: "http://localhost:8082",
		APIKey:  "shim",
		Stderr:  &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on lookup failure", code)
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error should mention PATH: %v", err)
	}
}

func TestRun_InjectsEnvAndArgs(t *testing.T) {
	dir := t.TempDir()
	stubPath := writeStub(t, dir, "stub-claude")

	// Capture stdout via pipe.
	r, w, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	stderrBuf := &bytes.Buffer{}
	code, err := Run(Options{
		Bin:     stubPath,
		BaseURL: "http://127.0.0.1:8082",
		APIKey:  "shim",
		Args:    []string{"hello", "world"},
		Stderr:  stderrBuf,
	})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("code = %d", code)
	}

	out := &bytes.Buffer{}
	_, _ = out.ReadFrom(r)
	got := out.String()
	if !strings.Contains(got, "argv: hello world") {
		t.Errorf("args not forwarded: %s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_BASE_URL=http://127.0.0.1:8082") {
		t.Errorf("BASE_URL not injected: %s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=shim") {
		t.Errorf("API_KEY not injected: %s", got)
	}

	// Breadcrumb went to Stderr.
	if !strings.Contains(stderrBuf.String(), "shim run →") {
		t.Errorf("breadcrumb missing from stderr: %s", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "base=http://127.0.0.1:8082") {
		t.Errorf("breadcrumb missing base URL: %s", stderrBuf.String())
	}
}

func TestRun_PropagatesExitCode(t *testing.T) {
	dir := t.TempDir()
	stubPath := writeStub(t, dir, "stub-claude")

	// Set STUB_EXIT in process env so the subprocess inherits.
	t.Setenv("STUB_EXIT", "7")

	code, err := Run(Options{
		Bin:     stubPath,
		BaseURL: "http://localhost",
		APIKey:  "shim",
		Stderr:  &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestRun_DefaultBin(t *testing.T) {
	// When Bin == "", default is "claude". On this test box claude IS in
	// PATH but we don't actually want to invoke it — we just want to
	// confirm the default-binary resolution kicks in. Use a deliberately
	// short exec by passing --help-style args is risky; safer: confirm
	// the lookup runs by setting Bin to "" and pointing PATH at our stub
	// dir so "claude" resolves to our stub.
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(stubPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	code, err := Run(Options{
		Bin:     "",
		BaseURL: "x",
		APIKey:  "y",
		Stderr:  &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d", code)
	}
}
