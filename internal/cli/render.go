// Package cli renders command output (human-readable summaries) for the
// ack-workspace batch commands and centralizes the labels used when presenting
// repository outcomes.
//
// The status command renders its own table or JSON through internal/inspector, so
// it does not come through here. main.go maps the same Summary to an exit code;
// see workspace.Summary.HasFailures.
package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// defaultSuccessLabel labels the success bucket when a command supplies no
// override of its own.
const defaultSuccessLabel = "created"

// RenderOptions configures how a Summary is rendered.
type RenderOptions struct {
	// SuccessLabel overrides the label used for the success bucket in both the
	// count header and the per-repository lines, so each command's summary reads in
	// its own terms: add passes "added", deploy "deployed", remove "removed". When
	// empty, "created" is used.
	SuccessLabel string
}

// RenderSummary writes summary to w: a count header, then one line per
// repository giving its name, outcome, and reason when it has one. Skipped and
// failed always use their literal names; only the success bucket is relabeled.
func RenderSummary(w io.Writer, summary workspace.Summary, opts RenderOptions) error {
	successLabel := opts.SuccessLabel
	if successLabel == "" {
		successLabel = defaultSuccessLabel
	}

	// Count header. OutcomeSucceeded is reported under the (possibly overridden)
	// created label so the add summary reads "added: N".
	if _, err := fmt.Fprintf(w, "%s: %d, skipped: %d, failed: %d\n",
		successLabel,
		summary.Count(workspace.OutcomeSucceeded),
		summary.Count(workspace.OutcomeSkipped),
		summary.Count(workspace.OutcomeFailed),
	); err != nil {
		return err
	}

	if len(summary.Results) == 0 {
		return nil
	}

	// Per-repository lines, column-aligned. The reason column is emitted only when
	// a result carries one, so successful rows stay uncluttered.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range summary.Results {
		if r.Reason != "" {
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Repo, outcomeLabel(r.Outcome, successLabel), r.Reason); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t\n", r.Repo, outcomeLabel(r.Outcome, successLabel)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// outcomeLabel maps an Outcome to its display label, substituting successLabel
// for OutcomeSucceeded.
func outcomeLabel(o workspace.Outcome, successLabel string) string {
	if o == workspace.OutcomeSucceeded {
		return successLabel
	}
	return string(o)
}
