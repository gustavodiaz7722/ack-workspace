package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/cmd"
	"github.com/aws-controllers-k8s/ack-workspace/internal/adder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// failureSummary returns a summary that contains a failed result.
func failureSummary() workspace.Summary {
	return workspace.Summary{Results: []workspace.Result{
		{Repo: "runtime", Outcome: workspace.OutcomeCreated},
		{Repo: "test-infra", Outcome: workspace.OutcomeFailed, Reason: "boom"},
	}}
}

// cleanSummary returns a failure-free summary (the shape dry-run also produces).
func cleanSummary() workspace.Summary {
	return workspace.Summary{Results: []workspace.Result{
		{Repo: "runtime", Outcome: workspace.OutcomeCreated},
		{Repo: "code-generator", Outcome: workspace.OutcomeSkipped, Reason: "directory already exists"},
	}}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name       string
		summary    workspace.Summary
		hasSummary bool
		err        error
		want       int
	}{
		{
			name:       "nil error and failure-free summary exits zero",
			summary:    cleanSummary(),
			hasSummary: true,
			err:        nil,
			want:       exitOK,
		},
		{
			name:       "nil error and summary with failures exits failure",
			summary:    failureSummary(),
			hasSummary: true,
			err:        nil,
			want:       exitFailure,
		},
		{
			name:       "cmd usage error exits usage code",
			hasSummary: false,
			err:        &cmd.UsageError{Msg: "invalid concurrency 33: accepted range is 1 to 32 inclusive"},
			want:       exitUsage,
		},
		{
			name:       "adder usage error exits usage code",
			hasSummary: false,
			err:        &adder.UsageError{Msg: "at least one service identifier is required"},
			want:       exitUsage,
		},
		{
			name:       "generic pre-flight error exits failure",
			hasSummary: false,
			err:        errors.New("config file is unparsable"),
			want:       exitFailure,
		},
		{
			name:       "no summary and nil error exits zero (config command)",
			hasSummary: false,
			err:        nil,
			want:       exitOK,
		},
		{
			name:       "dry-run failure-free summary exits zero",
			summary:    cleanSummary(),
			hasSummary: true,
			err:        nil,
			want:       exitOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.summary, tc.hasSummary, tc.err); got != tc.want {
				t.Errorf("exitCodeFor(...) = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExitCodeFor_WrappedUsageError(t *testing.T) {
	// A usage error wrapped by another error must still map to the usage code.
	wrapped := errors.Join(errors.New("context"), &cmd.UsageError{Msg: "bad flag"})
	if got := exitCodeFor(workspace.Summary{}, false, wrapped); got != exitUsage {
		t.Errorf("exitCodeFor(wrapped usage) = %d, want %d", got, exitUsage)
	}
}

func TestExitCode_NilResult(t *testing.T) {
	// A nil Result with a generic error must not panic and maps to failure.
	if got := exitCode(nil, errors.New("boom")); got != exitFailure {
		t.Errorf("exitCode(nil, err) = %d, want %d", got, exitFailure)
	}
	// A nil Result with no error exits zero.
	if got := exitCode(nil, nil); got != exitOK {
		t.Errorf("exitCode(nil, nil) = %d, want %d", got, exitOK)
	}
}

func TestReport_ErrorWrittenToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := report(&stdout, &stderr, nil, &cmd.UsageError{Msg: "at least one service identifier is required"})

	if code != exitUsage {
		t.Errorf("report(...) code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "at least one service identifier is required") {
		t.Errorf("error not written to stderr; stderr=%q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on a pre-flight error; got %q", stdout.String())
	}
}

func TestReport_NoSummaryNoError(t *testing.T) {
	// The config command produces neither an error nor a summary: nothing is
	// rendered and the process exits zero.
	var stdout, stderr bytes.Buffer
	if code := report(&stdout, &stderr, nil, nil); code != exitOK {
		t.Errorf("report(nil, nil) code = %d, want %d", code, exitOK)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("expected no output; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
