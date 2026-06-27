// Package cli renders command output (human-readable summaries) for the
// ack-workspace batch commands and centralizes the labels used when presenting
// repository outcomes.
//
// The status command renders its own table/JSON via internal/inspector; this
// package handles the created/skipped/failed summary produced by the batch
// commands (init, add, sync). The process entrypoint (main.go) maps the same
// Summary to an exit code; see internal/workspace.Summary.HasFailures.
package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// defaultCreatedLabel is the label used for the OutcomeCreated bucket when no
// override is supplied. The init command uses it ("created"); add and sync pass
// their own labels ("added", "updated") to match the requirement wording
// (Requirement 4.9 for add).
const defaultCreatedLabel = "created"

// RenderOptions configures how a Summary is rendered.
type RenderOptions struct {
	// CreatedLabel overrides the label used for the OutcomeCreated bucket in both
	// the count header and the per-repository lines. When empty, "created" is
	// used. The add command passes "added" so OutcomeCreated renders as "added"
	// (Requirement 4.9); the sync command passes "updated".
	CreatedLabel string
}

// RenderSummary writes a human-readable rendering of summary to w. The output is
// a count header (created/added, skipped, and failed totals) followed by one
// line per repository giving its name, outcome, and reason (when present).
//
// The OutcomeCreated bucket is labeled using opts.CreatedLabel (defaulting to
// "created"); skipped and failed always use their literal names. Writing to an
// injectable io.Writer keeps the rendering testable.
func RenderSummary(w io.Writer, summary workspace.Summary, opts RenderOptions) error {
	createdLabel := opts.CreatedLabel
	if createdLabel == "" {
		createdLabel = defaultCreatedLabel
	}

	// Count header. OutcomeCreated is reported under the (possibly overridden)
	// created label so the add summary reads "added: N" (Requirement 4.9).
	if _, err := fmt.Fprintf(w, "%s: %d, skipped: %d, failed: %d\n",
		createdLabel,
		summary.Count(workspace.OutcomeCreated),
		summary.Count(workspace.OutcomeSkipped),
		summary.Count(workspace.OutcomeFailed),
	); err != nil {
		return err
	}

	if len(summary.Results) == 0 {
		return nil
	}

	// Per-repository lines, column-aligned. The reason column is emitted only
	// when a result carries one, so successful created rows stay uncluttered.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range summary.Results {
		if r.Reason != "" {
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Repo, outcomeLabel(r.Outcome, createdLabel), r.Reason); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t\n", r.Repo, outcomeLabel(r.Outcome, createdLabel)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// outcomeLabel maps an Outcome to its display label, substituting createdLabel
// for OutcomeCreated so each command's wording (created/added/updated) is used
// consistently in both the header and the per-repository lines.
func outcomeLabel(o workspace.Outcome, createdLabel string) string {
	if o == workspace.OutcomeCreated {
		return createdLabel
	}
	return string(o)
}
