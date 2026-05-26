// Package launcher implements `shim run`: locate `claude`, inject the
// ANTHROPIC_BASE_URL + ANTHROPIC_API_KEY env vars pointing at the local
// shim server, and exec it with the user's args. Stdio is wired through.
package launcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Options configures a Run invocation. Bin defaults to "claude" when empty.
// Stderr defaults to os.Stderr; tests inject a buffer.
type Options struct {
	Bin     string
	BaseURL string
	APIKey  string
	Args    []string
	Stderr  io.Writer
}

// Run resolves the target binary via exec.LookPath, prints a one-line
// breadcrumb to Stderr (so users see *something* before the subprocess
// produces output), then execs the binary with env injected. Returns the
// subprocess exit code as (code, nil) on a normal-but-non-zero exit, or
// (0, err) when the launcher itself failed (lookup, fork, IO).
func Run(opts Options) (int, error) {
	bin := opts.Bin
	if bin == "" {
		bin = "claude"
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	path, err := exec.LookPath(bin)
	if err != nil {
		return 0, fmt.Errorf("%s not found in PATH: %w", bin, err)
	}

	if _, werr := fmt.Fprintf(stderr, "shim run → %s=%s, base=%s\n", bin, path, opts.BaseURL); werr != nil {
		// best-effort stderr; do not fail run on stderr flush
		_ = werr
	}

	cmd := exec.Command(path, opts.Args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+opts.BaseURL,
		"ANTHROPIC_API_KEY="+opts.APIKey,
	)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, fmt.Errorf("%s: %w", bin, err)
	}
	return 0, nil
}
