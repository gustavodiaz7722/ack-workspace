package syncer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/engine"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// Remote names used by the fork-based contributor workflow: "upstream" points
// at the canonical ACK organization repository (fetched from), and "origin"
// points at the contributor's fork (pushed to).
const (
	upstreamRemote = "upstream"
	originRemote   = "origin"
)

// Upstream default-branch ref scheme.
//
// The Fork_Synchronizer needs the Upstream_Remote Default_Branch to compare
// against and fast-forward to. Rather than implementing full remote-HEAD
// resolution (querying `git symbolic-ref refs/remotes/upstream/HEAD`), this
// implementation uses the documented, simpler scheme permitted by the design:
// it reads the locally checked-out branch via CurrentBranch and treats that as
// the local Default_Branch, then compares and fast-forwards it against the
// remote-tracking ref "upstream/<branch>". For the common ACK case the
// checked-out branch is the default branch (e.g. "main"), so "upstream/main" is
// exactly the Upstream_Remote Default_Branch. This keeps the logic to stable
// git plumbing (symbolic-ref, status, merge-base, merge --ff-only) and is fully
// exercised against the git MockRunner in the tests.
func upstreamRefFor(branch string) string {
	return upstreamRemote + "/" + branch
}

// Syncer implements the Fork_Synchronizer. It updates Managed_Repositories from
// their Upstream_Remote using fast-forward-only semantics, never destroying
// local work, and optionally pushes updated branches to the Origin_Remote.
type Syncer struct{}

// New returns a ready-to-use Syncer.
func New() *Syncer { return &Syncer{} }

// Sync synchronizes Managed_Repositories under the Workspace_Root with their
// Upstream_Remote.
//
// When only is non-empty, synchronization is restricted to that subset
// (Requirement 5.5); any name in only that is not a Managed_Repository is
// recorded as a failed Result identifying the invalid repository, and the valid
// repositories are still processed (Requirement 5.10). When push is true, each
// repository that is updated is pushed to its Origin_Remote (Requirement 5.6).
//
// The returned error is non-nil only for a pre-flight failure to discover the
// workspace; all per-repository problems are captured as failed Results so the
// batch continues (Property 3). The returned Summary is sorted by repository
// name for stable output.
func (s *Syncer) Sync(ctx context.Context, a app.App, only []string, push bool) (workspace.Summary, error) {
	root := a.Config.WorkspaceRoot

	discovered, err := workspace.Discover(root)
	if err != nil {
		return workspace.Summary{}, fmt.Errorf("discovering managed repositories under %q: %w", root, err)
	}

	// Resolve which repositories to process. Names supplied via only that are
	// not managed are recorded as failed up front (Requirement 5.10) and never
	// touch the filesystem.
	toProcess, invalid := selectRepos(discovered, only)

	preResults := make([]workspace.Result, 0, len(invalid))
	for _, name := range invalid {
		err := fmt.Errorf("%q is not a managed repository under %s", name, root)
		preResults = append(preResults, workspace.Result{
			Repo:    name,
			Outcome: workspace.OutcomeFailed,
			Reason:  err.Error(),
			Err:     err,
		})
	}

	// Build one task per valid repository and run them with the configured
	// bounded concurrency (Requirement 7).
	tasks := make([]engine.Task, 0, len(toProcess))
	for _, name := range toProcess {
		name := name
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return s.process(ctx, a, filepath.Join(root, name), name, push)
		})
	}

	procResults := engine.Run(ctx, a.Config.Concurrency, tasks)

	// Combine the invalid-name failures with the per-repository results and
	// sort by repository name so the Summary is deterministic regardless of
	// completion order (Requirement 5.7).
	results := append(preResults, procResults...)
	sort.SliceStable(results, func(i, j int) bool { return results[i].Repo < results[j].Repo })

	return workspace.Summary{Results: results}, nil
}

// selectRepos partitions only into the managed repositories to process and the
// invalid (unmanaged) names. When only is empty, every discovered repository is
// processed and there are no invalid names.
func selectRepos(discovered, only []string) (toProcess, invalid []string) {
	if len(only) == 0 {
		return discovered, nil
	}
	managed := make(map[string]bool, len(discovered))
	for _, name := range discovered {
		managed[name] = true
	}
	for _, name := range only {
		if managed[name] {
			toProcess = append(toProcess, name)
		} else {
			invalid = append(invalid, name)
		}
	}
	return toProcess, invalid
}

// process runs the fetch/compare/fast-forward(/push) flow for a single
// repository and returns its terminal Result. It never returns an error
// out-of-band: every failure is captured into a failed Result so the engine
// continues with the other repositories (Requirements 5.8, 5.9; Property 3).
//
// The decision order upholds the "never lose local work" guarantee
// (Requirement 8): fetch updates only remote-tracking refs; a Dirty_Working_Tree
// is skipped before any branch-modifying command runs (Requirements 5.3, 8.3);
// a non-fast-forwardable branch is skipped before any merge (Requirements 5.4,
// 8.1, 8.2); only a clean, fast-forwardable branch is advanced (Requirement 5.2).
func (s *Syncer) process(ctx context.Context, a app.App, path, name string, push bool) workspace.Result {
	repo := git.NewRepo(path, a.Git)

	// Dry-run (Requirements 8.4, 8.5; Property 6): compute the same decision
	// using only read-only inspection and report the action that would be taken,
	// without invoking any mutating operation. Per Property 6 ("no fetch
	// mutation") this explicitly skips Fetch as well, basing the preview on the
	// currently-available local state (remote-tracking refs as they stand now).
	if a.DryRun {
		return s.preview(ctx, repo, name, push)
	}

	// Requirements 5.1, 5.8: fetch the Upstream_Remote. On failure, leave the
	// branch untouched and record the repository as failed.
	if err := repo.Fetch(ctx, upstreamRemote); err != nil {
		return failed(name, fmt.Errorf("fetching %s: %w", upstreamRemote, err))
	}

	// Determine the local Default_Branch to fast-forward. See upstreamRefFor for
	// the documented ref scheme.
	branch, detached, err := repo.CurrentBranch(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("determining current branch: %w", err))
	}
	if detached {
		// With a detached HEAD there is no local branch to advance. Leave the
		// repository untouched and record it as skipped so a benign,
		// user-controlled state does not fail the batch.
		return skipped(name, "detached HEAD")
	}
	upstreamRef := upstreamRefFor(branch)

	// Requirements 5.3, 8.3: a Dirty_Working_Tree is skipped, touching nothing.
	// This check runs before any branch-modifying command so no merge/checkout
	// or push is issued for a dirty repository.
	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("checking working tree: %w", err))
	}
	if dirty {
		return skipped(name, "uncommitted changes")
	}

	// Requirements 5.4, 8.1, 8.2: if the local branch cannot be fast-forwarded
	// to the upstream ref (diverged history), skip it, leaving the branch
	// unchanged.
	canFF, err := repo.CanFastForward(ctx, branch, upstreamRef)
	if err != nil {
		return failed(name, fmt.Errorf("checking fast-forward against %s: %w", upstreamRef, err))
	}
	if !canFF {
		return skipped(name, "diverged history")
	}

	// Requirement 5.2: advance the local branch to the upstream ref by
	// fast-forward only and record the repository as updated.
	if err := repo.FastForward(ctx, branch, upstreamRef); err != nil {
		return failed(name, fmt.Errorf("fast-forwarding %q to %s: %w", branch, upstreamRef, err))
	}

	// Requirements 5.6, 5.9: when requested, push the updated branch to the
	// Origin_Remote; on push failure record the repository as failed.
	if push {
		if err := repo.Push(ctx, originRemote, branch); err != nil {
			return failed(name, fmt.Errorf("pushing %q to %s: %w", branch, originRemote, err))
		}
	}

	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeCreated,
		Reason:  "updated",
	}
}

// preview computes the sync decision for a single repository using only
// read-only inspection and returns the Result describing the action that would
// be taken, without mutating anything (Requirements 8.4, 8.5; Property 6).
//
// Unlike process it does not Fetch: per Property 6 a fetch mutates remote-
// tracking refs, so dry-run skips it and bases the decision on the currently-
// available local state. The remaining checks (CurrentBranch via symbolic-ref,
// IsDirty via status --porcelain, CanFastForward via merge-base --is-ancestor)
// are all read-only and change nothing. The decision order mirrors process so
// the preview matches what a real sync would do.
func (s *Syncer) preview(ctx context.Context, repo *git.Repo, name string, push bool) workspace.Result {
	branch, detached, err := repo.CurrentBranch(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("determining current branch: %w", err))
	}
	if detached {
		return skipped(name, "would skip (detached HEAD)")
	}
	upstreamRef := upstreamRefFor(branch)

	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("checking working tree: %w", err))
	}
	if dirty {
		return skipped(name, "would skip (uncommitted changes)")
	}

	canFF, err := repo.CanFastForward(ctx, branch, upstreamRef)
	if err != nil {
		return failed(name, fmt.Errorf("checking fast-forward against %s: %w", upstreamRef, err))
	}
	if !canFF {
		return skipped(name, "would skip (diverged history)")
	}

	reason := "would fast-forward"
	if push {
		reason = "would fast-forward and push to origin"
	}
	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeCreated,
		Reason:  reason,
	}
}

// skipped builds a skipped Result carrying the given human-readable reason
// (for example "uncommitted changes" or "diverged history").
func skipped(name, reason string) workspace.Result {
	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeSkipped,
		Reason:  reason,
	}
}

// failed builds a failed Result carrying the underlying error and its text as
// the human-readable reason.
func failed(name string, err error) workspace.Result {
	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeFailed,
		Reason:  err.Error(),
		Err:     err,
	}
}
