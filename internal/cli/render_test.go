package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// sampleSummary returns a summary with one created, one skipped, and one failed
// result so the count header and the per-repository lines can be asserted.
func sampleSummary() workspace.Summary {
	return workspace.Summary{Results: []workspace.Result{
		{Repo: "runtime", Outcome: workspace.OutcomeCreated},
		{Repo: "code-generator", Outcome: workspace.OutcomeSkipped, Reason: "directory already exists"},
		{Repo: "test-infra", Outcome: workspace.OutcomeFailed, Reason: "boom"},
	}}
}

func TestRenderSummary_DefaultCreatedLabel(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummary(&buf, sampleSummary(), RenderOptions{}); err != nil {
		t.Fatalf("RenderSummary returned error: %v", err)
	}
	out := buf.String()

	// Count header uses the default "created" label and the correct totals.
	if !strings.Contains(out, "created: 1, skipped: 1, failed: 1") {
		t.Errorf("output missing default count header.\n%s", out)
	}
	// Per-repository lines appear with their repo, outcome, and reason.
	for _, want := range []string{"runtime", "code-generator", "directory already exists", "test-infra", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q.\n%s", want, out)
		}
	}
}

func TestRenderSummary_AddedLabel(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummary(&buf, sampleSummary(), RenderOptions{CreatedLabel: "added"}); err != nil {
		t.Fatalf("RenderSummary returned error: %v", err)
	}
	out := buf.String()

	// The created bucket is relabeled "added" in the header (Requirement 4.9).
	if !strings.Contains(out, "added: 1, skipped: 1, failed: 1") {
		t.Errorf("output missing relabeled count header.\n%s", out)
	}
	// The default "created:" header must not appear when relabeled.
	if strings.Contains(out, "created:") {
		t.Errorf("output should not contain default created header when relabeled.\n%s", out)
	}
	// The OutcomeCreated per-repository line is also relabeled "added".
	if !strings.Contains(out, "runtime") || !strings.Contains(out, "added") {
		t.Errorf("created row not rendered with the added label.\n%s", out)
	}
}

func TestRenderSummary_UpdatedLabel(t *testing.T) {
	var buf bytes.Buffer
	summary := workspace.Summary{Results: []workspace.Result{
		{Repo: "s3-controller", Outcome: workspace.OutcomeCreated, Reason: "updated"},
	}}
	if err := RenderSummary(&buf, summary, RenderOptions{CreatedLabel: "updated"}); err != nil {
		t.Fatalf("RenderSummary returned error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "updated: 1, skipped: 0, failed: 0") {
		t.Errorf("output missing updated count header.\n%s", out)
	}
}

func TestRenderSummary_EmptyPrintsHeaderOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderSummary(&buf, workspace.Summary{}, RenderOptions{}); err != nil {
		t.Fatalf("RenderSummary returned error: %v", err)
	}
	out := strings.TrimSpace(buf.String())

	// With no results only the zeroed header is printed; no per-repo lines.
	if out != "created: 0, skipped: 0, failed: 0" {
		t.Errorf("empty summary output = %q, want only the zeroed header", out)
	}
}
