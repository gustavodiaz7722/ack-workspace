// Command ack-workspace automates the fork-based contributor workflow for
// AWS Controllers for Kubernetes (ACK).
//
// This entrypoint runs the cobra root command (see internal cmd package),
// renders the aggregated repository summary the batch commands produce, and maps
// the outcome to a process exit code. Keeping the mapping in the small,
// dependency-free exitCodeFor helper lets it be unit-tested without spawning the
// process (Requirements 4.9, 7.5, 7.6, 8.5; design "Exit code policy",
// Property 8).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws-controllers-k8s/ack-workspace/cmd"
	"github.com/aws-controllers-k8s/ack-workspace/internal/adder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/builder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/cli"
	"github.com/aws-controllers-k8s/ack-workspace/internal/releaser"
	"github.com/aws-controllers-k8s/ack-workspace/internal/scanner"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// Process exit codes. The policy (design "Exit code policy", Property 8) maps a
// usage/validation error to a distinct code so callers can distinguish it from a
// runtime failure; every other failure (a pre-flight error or any repository
// that failed) uses the generic failure code, and a clean run exits zero.
const (
	// exitOK indicates the command completed and no repository failed.
	exitOK = 0
	// exitFailure indicates a non-usage pre-flight error occurred or at least
	// one repository failed (Requirements 7.5, 4.9).
	exitFailure = 1
	// exitUsage indicates an argument/validation error (a *cmd.UsageError or
	// *adder.UsageError), such as an out-of-range concurrency value or an empty
	// add identifier list (Requirements 7.3, 4.2).
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

// run executes the root command and reports the result, returning the process
// exit code. stdout receives the rendered summary; stderr receives error
// messages. Splitting run from main keeps os.Exit at the very top so deferred
// cleanup elsewhere is never skipped, and lets the rendering/exit behavior be
// driven with in-memory writers in tests.
func run(stdout, stderr io.Writer) int {
	res, err := cmd.Execute()
	return report(stdout, stderr, res, err)
}

// report renders the command's output and returns its exit code. A pre-flight or
// usage error is printed to stderr; otherwise the batch summary (when one was
// produced) is rendered to stdout. The exit code is derived independently of
// rendering by exitCode so the mapping stays unit-testable.
func report(stdout, stderr io.Writer, res *cmd.Result, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitCode(res, err)
	}

	if res != nil {
		if summary, ok := res.Summary(); ok {
			// Render the created/skipped/failed summary for the batch commands.
			// The status command stashes a neutral (empty) summary because it
			// already rendered its own table/JSON, so there is nothing to print
			// for it here; the config command stashes no summary at all.
			if len(summary.Results) > 0 {
				_ = cli.RenderSummary(stdout, summary, cli.RenderOptions{CreatedLabel: res.CreatedLabel()})
			}
		}
	}

	return exitCode(res, err)
}

// exitCode maps the (Result, error) returned by cmd.Execute to a process exit
// code. It defers to exitCodeFor after decomposing the Result so the policy is
// expressed in one dependency-light place.
func exitCode(res *cmd.Result, err error) int {
	var (
		summary    workspace.Summary
		hasSummary bool
	)
	if res != nil {
		summary, hasSummary = res.Summary()
	}
	return exitCodeFor(summary, hasSummary, err)
}

// exitCodeFor is the pure exit-code policy (design "Exit code policy",
// Property 8). It is split out so the mapping can be unit-tested without
// constructing a cobra command or spawning the process:
//
//   - a non-nil usage error  -> exitUsage   (Requirements 7.3, 4.2)
//   - any other non-nil error -> exitFailure (Requirement 7.5, pre-flight)
//   - a summary with failures -> exitFailure (Requirements 7.5, 4.9)
//   - otherwise               -> exitOK      (Requirement 7.6)
//
// Dry-run produces failure-free summaries, so a dry-run invocation falls through
// to exitOK (Requirement 8.5). A command that produces no summary (config) with
// no error also exits zero.
func exitCodeFor(summary workspace.Summary, hasSummary bool, err error) int {
	if err != nil {
		if isUsageError(err) {
			return exitUsage
		}
		return exitFailure
	}
	if hasSummary && summary.HasFailures() {
		return exitFailure
	}
	return exitOK
}

// isUsageError reports whether err is (or wraps) one of the tool's typed usage
// errors: a *cmd.UsageError (invalid concurrency and other root validation), a
// *adder.UsageError (the empty add identifier list), a *releaser.UsageError
// (a missing service identifier or invalid release version), a
// *builder.UsageError (a missing build service identifier), or a
// *scanner.UsageError (an unknown or unparsable issue selector). These map to a
// distinct exit code from runtime failures.
func isUsageError(err error) bool {
	var cmdUsage *cmd.UsageError
	var adderUsage *adder.UsageError
	var releaserUsage *releaser.UsageError
	var builderUsage *builder.UsageError
	var scannerUsage *scanner.UsageError
	return errors.As(err, &cmdUsage) || errors.As(err, &adderUsage) ||
		errors.As(err, &releaserUsage) || errors.As(err, &builderUsage) ||
		errors.As(err, &scannerUsage)
}
