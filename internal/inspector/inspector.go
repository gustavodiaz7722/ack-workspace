package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// Comparison values reported in a StatusEntry. They mirror the constrained set
// of strings the data model documents ("up_to_date" | "ahead" | "behind" |
// "unavailable") so the human and JSON renderings stay in sync.
const (
	comparisonUpToDate    = "up_to_date"
	comparisonAhead       = "ahead"
	comparisonBehind      = "behind"
	comparisonUnavailable = "unavailable"
)

// upstreamRemote is the name of the git remote that points at the upstream
// (canonical) repository. The status comparison measures the local branch
// against this remote's tracking ref for the same branch (see comparisonFor).
const upstreamRemote = "upstream"

// Inspector implements the Workspace_Inspector. It reports the state of every
// Managed_Repository found under the Workspace_Root.
//
// Output is written to an injectable io.Writer (out) so tests can capture and
// assert it; New defaults out to os.Stdout while NewWithWriter lets tests
// substitute a buffer.
//
// Status is READ-ONLY: it only inspects repositories and never mutates them.
// Consequently it never records per-repository failures and always returns a
// neutral (empty) workspace.Summary, so Summary.HasFailures() is false and the
// command's exit code is success unless a pre-flight error occurs (the exit-code
// mapping itself lives in the CLI layer). The returned error is non-nil only for
// a pre-flight failure such as being unable to read the Workspace_Root.
type Inspector struct {
	// out is where rendered status (table or JSON) is written. It defaults to
	// os.Stdout and is injectable for testing.
	out io.Writer
}

// New returns an Inspector that writes to os.Stdout.
func New() *Inspector { return &Inspector{out: os.Stdout} }

// NewWithWriter returns an Inspector that writes to out. It is intended for
// tests that capture and assert the rendered output.
func NewWithWriter(out io.Writer) *Inspector { return &Inspector{out: out} }

// Status lists each Managed_Repository found directly under the Workspace_Root,
// ordered alphabetically by directory name, and reports each repository's
// branch (or detached-HEAD state), its ahead/behind/up-to-date comparison
// against the upstream default branch, and whether its working tree is dirty
// (Requirement 6).
//
// When no managed repository is found it emits a friendly message (or an empty
// JSON array when jsonOut is true) and returns without error (Requirement 6.7).
// When jsonOut is true the entire status set is emitted as a single JSON
// document — an array of StatusEntry values (Requirement 6.8); otherwise a
// human-readable table is printed.
//
// Status is read-only: it returns a neutral, empty Summary (see the type doc).
func (in *Inspector) Status(ctx context.Context, a app.App, jsonOut bool) (workspace.Summary, error) {
	root := a.Config.WorkspaceRoot

	// Requirement 6.1: list repositories directly under the Workspace_Root,
	// already returned sorted alphabetically by directory name. A missing root
	// is treated by Discover as an empty workspace (nil error).
	repos, err := workspace.Discover(root)
	if err != nil {
		// Pre-flight failure (e.g. the root is unreadable). Surface it so the CLI
		// layer can exit non-zero; this is the only error Status returns.
		return workspace.Summary{}, fmt.Errorf("discovering repositories under %q: %w", root, err)
	}

	// Requirement 6.7: no managed repositories. Emit a friendly message (or an
	// empty JSON array in machine-readable mode) and exit without error.
	if len(repos) == 0 {
		if jsonOut {
			in.renderJSON([]workspace.StatusEntry{})
		} else {
			fmt.Fprintf(in.out, "No managed repositories found under %s\n", root)
		}
		return workspace.Summary{}, nil
	}

	// Build one StatusEntry per repository. Iteration is sequential because the
	// work is read-only and the output must be ordered by repository name; the
	// repos slice is already sorted, so the entries preserve that order.
	entries := make([]workspace.StatusEntry, 0, len(repos))
	for _, name := range repos {
		repo := git.NewRepo(filepath.Join(root, name), a.Git)
		entries = append(entries, in.inspect(ctx, repo, name))
	}

	if jsonOut {
		in.renderJSON(entries)
	} else {
		in.renderTable(entries)
	}

	// Status records no Results: it is read-only and never contributes to the
	// exit code (see the type doc).
	return workspace.Summary{}, nil
}

// inspect builds the StatusEntry for a single repository. It is resilient: any
// inspection problem degrades that field gracefully (an undeterminable
// comparison becomes "unavailable", Requirement 6.6) rather than aborting the
// listing, so the remaining repositories are always reported.
func (in *Inspector) inspect(ctx context.Context, repo *git.Repo, name string) workspace.StatusEntry {
	entry := workspace.StatusEntry{Repo: name}

	// Requirements 6.2, 6.3: report the checked-out branch, or that the
	// repository is in a detached HEAD state.
	branch, detached, err := repo.CurrentBranch(ctx)
	switch {
	case err != nil:
		// Branch could not be determined; leave Branch empty and mark the
		// comparison unavailable since there is no local branch to compare.
		entry.Comparison = comparisonUnavailable
	case detached:
		// Requirement 6.3: detached HEAD — no branch name, and with no current
		// branch the upstream comparison is unavailable (Requirement 6.6).
		entry.Detached = true
		entry.Comparison = comparisonUnavailable
	default:
		entry.Branch = branch
		// Requirements 6.4, 6.6: compare the local branch to its upstream
		// tracking ref and classify the relationship.
		in.fillComparison(ctx, repo, branch, &entry)
	}

	// Requirement 6.5: report whether the working tree is dirty. A read error
	// here leaves Dirty at its zero value (false) rather than failing the listing.
	if dirty, derr := repo.IsDirty(ctx); derr == nil {
		entry.Dirty = dirty
	}

	return entry
}

// fillComparison sets the Comparison, Ahead, and Behind fields of entry by
// comparing the local branch to its upstream tracking ref.
//
// Ref scheme: the local branch <branch> is compared to "upstream/<branch>" —
// the same branch on the "upstream" remote. AheadBehind reports how many
// commits the local branch is ahead of and behind that ref.
//
// Requirement 6.6: if the comparison cannot be determined (for example the
// upstream remote or its branch ref is not present locally, which makes
// AheadBehind fail), the comparison is recorded as "unavailable" and listing
// continues.
//
// When both ahead and behind counts are non-zero (a diverged branch), the
// comparison is classified as "behind" so the table flags that upstream has
// commits the local branch lacks; both counts remain available in the entry
// (and in the JSON output) regardless of the classification.
func (in *Inspector) fillComparison(ctx context.Context, repo *git.Repo, branch string, entry *workspace.StatusEntry) {
	upstreamRef := upstreamRemote + "/" + branch
	ahead, behind, err := repo.AheadBehind(ctx, branch, upstreamRef)
	if err != nil {
		entry.Comparison = comparisonUnavailable
		return
	}
	entry.Ahead = ahead
	entry.Behind = behind
	switch {
	case behind > 0:
		entry.Comparison = comparisonBehind
	case ahead > 0:
		entry.Comparison = comparisonAhead
	default:
		entry.Comparison = comparisonUpToDate
	}
}

// renderJSON writes the entries as a single, indented JSON document (an array)
// to the Inspector's writer (Requirement 6.8). A nil or empty slice is rendered
// as "[]" rather than "null" because entries is always a non-nil slice.
func (in *Inspector) renderJSON(entries []workspace.StatusEntry) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		// StatusEntry contains only JSON-encodable scalar fields, so marshalling
		// cannot realistically fail; guard defensively all the same.
		fmt.Fprintf(in.out, "error rendering JSON: %v\n", err)
		return
	}
	fmt.Fprintf(in.out, "%s\n", data)
}

// renderTable writes a human-readable, column-aligned table of the entries to
// the Inspector's writer using a tabwriter.
func (in *Inspector) renderTable(entries []workspace.StatusEntry) {
	w := tabwriter.NewWriter(in.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tBRANCH\tSTATUS\tWORKING TREE")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Repo, branchText(e), statusText(e), workingTreeText(e.Dirty))
	}
	_ = w.Flush()
}

// branchText renders the branch column: the branch name, or "(detached HEAD)"
// when the repository is in a detached HEAD state (Requirement 6.3).
func branchText(e workspace.StatusEntry) string {
	if e.Detached {
		return "(detached HEAD)"
	}
	return e.Branch
}

// statusText renders the comparison column from the entry's Comparison and
// counts (Requirement 6.4), or "unavailable" when the comparison could not be
// determined (Requirement 6.6).
func statusText(e workspace.StatusEntry) string {
	switch e.Comparison {
	case comparisonUpToDate:
		return "up to date"
	case comparisonAhead:
		return fmt.Sprintf("ahead %d", e.Ahead)
	case comparisonBehind:
		return fmt.Sprintf("behind %d", e.Behind)
	default:
		return "unavailable"
	}
}

// workingTreeText renders the dirty column (Requirement 6.5).
func workingTreeText(dirty bool) string {
	if dirty {
		return "dirty"
	}
	return "clean"
}
