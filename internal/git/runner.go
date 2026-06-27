// Package git defines the git.Runner interface, its os/exec implementation and
// recording mock, and (in later tasks) the high-level Repo operations (Clone,
// SetRemote, Fetch, FastForward, Push, Status, AheadBehind) used by the feature
// components.
//
// All git invocations flow through the Runner abstraction so command
// construction is centralized, arguments are passed without shell
// interpolation (avoiding command injection), and tests can assert the exact
// git argument vectors a component issues (Requirements 1.1, 8.3).
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Runner executes git commands. It is the single low-level seam through which
// every git invocation passes, so it can be mocked in tests and so argument
// construction stays centralized and injection-safe.
type Runner interface {
	// Run executes `git <args...>` in dir and returns the combined
	// stdout+stderr output together with any error. Arguments are passed
	// directly to the git process (no shell), so values never undergo shell
	// interpolation.
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// ExecRunner is the production Runner. It invokes the system `git` executable
// resolved on the PATH (whose presence the Prerequisite_Checker guarantees,
// Requirement 1.1) and returns its combined output.
type ExecRunner struct {
	// Bin is the git executable to invoke. When empty it defaults to "git",
	// resolved against the PATH.
	Bin string
}

// Ensure ExecRunner satisfies the interface at compile time.
var _ Runner = (*ExecRunner)(nil)

// NewExecRunner returns an ExecRunner that invokes the system `git` binary.
func NewExecRunner() *ExecRunner {
	return &ExecRunner{Bin: "git"}
}

// Run executes `git <args...>` in dir using os/exec. Arguments are supplied as
// a slice (never a shell string), so caller-provided values such as repository
// URLs or branch names cannot be interpreted by a shell. It returns the
// combined stdout+stderr; on a non-zero exit the returned error wraps the
// underlying exec error while the string still carries any git output for
// diagnostics.
func (r *ExecRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := r.Bin
	if bin == "" {
		bin = "git"
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir

	// Capture stdout and stderr into a single buffer so callers see the
	// combined output regardless of which stream git wrote to.
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	out := combined.String()
	if err != nil {
		return out, fmt.Errorf("git %v: %w", args, err)
	}
	return out, nil
}
