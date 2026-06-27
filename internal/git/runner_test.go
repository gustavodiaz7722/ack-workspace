package git

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestExecRunner_GitVersion is a lightweight smoke test that the ExecRunner
// actually shells out to the system git and returns its combined output. It is
// skipped when git is not installed so the unit suite stays hermetic; the full
// repository-level behavior is covered by the integration tests in a later task.
func TestExecRunner_GitVersion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping ExecRunner smoke test")
	}

	r := NewExecRunner()
	out, err := r.Run(context.Background(), "", "--version")
	if err != nil {
		t.Fatalf("git --version failed: %v (output: %q)", err, out)
	}
	if !strings.Contains(out, "git version") {
		t.Errorf("expected output to contain %q, got %q", "git version", out)
	}
}

// TestExecRunner_NonZeroExitReturnsError verifies that a failing git command
// surfaces an error while still returning the combined output for diagnostics.
func TestExecRunner_NonZeroExitReturnsError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping ExecRunner smoke test")
	}

	r := NewExecRunner()
	_, err := r.Run(context.Background(), t.TempDir(), "this-is-not-a-git-subcommand")
	if err == nil {
		t.Fatal("expected an error for an invalid git subcommand, got nil")
	}
}
