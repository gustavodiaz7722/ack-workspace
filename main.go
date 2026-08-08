// Command ack-workspace automates the fork-based contributor workflow for AWS
// Controllers for Kubernetes (ACK).
//
// This entrypoint runs the cobra root command, renders the aggregated
// repository summary the batch commands produce, and maps the outcome to a
// process exit code. The mapping lives in the small, dependency-free
// exitCodeFor helper so it can be unit-tested without spawning the process.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws-controllers-k8s/ack-workspace/cmd"
	"github.com/aws-controllers-k8s/ack-workspace/internal/adder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/attributor"
	"github.com/aws-controllers-k8s/ack-workspace/internal/builder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/cli"
	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/releaser"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// Process exit codes. A usage/validation error gets its own code so callers can
// tell it from a runtime failure; every other failure (a pre-flight error or
// any repository that failed) uses the generic failure code, and a clean run
// exits zero.
const (
	// exitOK indicates the command completed and no repository failed.
	exitOK = 0
	// exitFailure indicates a non-usage pre-flight error occurred or at least one
	// repository failed.
	exitFailure = 1
	// exitUsage indicates an argument/validation error, such as an out-of-range
	// concurrency value or an empty add identifier list.
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

// report renders the command's output and returns its exit code. A pre-flight
// or usage error is printed to stderr; otherwise the batch summary (when one
// was produced) is rendered to stdout. The exit code is derived independently
// of rendering by exitCode so the mapping stays unit-testable.
func report(stdout, stderr io.Writer, res *cmd.Result, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitCode(res, err)
	}

	if res != nil {
		if summary, ok := res.Summary(); ok {
			// Render the created/skipped/failed summary for the batch commands. The
			// status command stashes a neutral (empty) summary because it already
			// rendered its own table/JSON, so there is nothing to print for it here; the
			// config command stashes no summary at all.
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

// exitCodeFor is the pure exit-code policy, split out so it can be unit-tested
// without constructing a cobra command or spawning the process:
//
//   - a non-nil usage error   -> exitUsage
//   - any other non-nil error -> exitFailure
//   - a summary with failures  -> exitFailure
//   - otherwise                -> exitOK
//
// Dry-run produces failure-free summaries, so a dry-run invocation exits zero,
// as does a command that produces no summary at all (config).
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

// isUsageError reports whether err is (or wraps) one of the typed usage errors
// a component returns for bad arguments: invalid concurrency and other root
// validation, an empty add or attribution identifier list, a missing service
// identifier for build, deploy, or release, or an invalid release version.
// Every component that can reject its arguments before doing work must appear
// here, or its usage error is reported as a generic runtime failure.
func isUsageError(err error) bool {
	var cmdUsage *cmd.UsageError
	var adderUsage *adder.UsageError
	var releaserUsage *releaser.UsageError
	var builderUsage *builder.UsageError
	var deployerUsage *deployer.UsageError
	var attributorUsage *attributor.UsageError
	return errors.As(err, &cmdUsage) || errors.As(err, &adderUsage) ||
		errors.As(err, &releaserUsage) || errors.As(err, &builderUsage) ||
		errors.As(err, &deployerUsage) || errors.As(err, &attributorUsage)
}
