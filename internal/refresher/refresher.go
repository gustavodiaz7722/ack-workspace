package refresher

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/engine"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	// upstreamRemote points at the canonical ACK organization repository.
	upstreamRemote = "upstream"
	// originRemote points at the contributor's fork.
	originRemote = "origin"
	// defaultBranch is the branch the refresher checks out and reconciles. ACK
	// repositories use "main" as their default branch.
	defaultBranch = "main"
	// upstreamRef is the remote-tracking ref the local default branch is reset to
	// match.
	upstreamRef = upstreamRemote + "/" + defaultBranch
)

// Refresher reconciles each managed repository to a clean, up-to-date default
// branch ready for development.
type Refresher struct{}

// New returns a ready-to-use Refresher.
func New() *Refresher { return &Refresher{} }

// Refresh reconciles each managed repository under the workspace root to the
// development baseline: fork synced with upstream, all upstream tags present
// locally, the default branch checked out, and the local default branch reset
// to match upstream.
//
// When only is non-empty, processing is restricted to that subset; any name in
// only that is not a managed repository is recorded as a failed Result, and the
// valid repositories are still processed. The returned error is non-nil only
// for a pre-flight failure to discover the workspace; all per-repository
// problems are captured as failed Results so the batch continues. The returned
// Summary is sorted by repository name for stable output.
func (r *Refresher) Refresh(ctx context.Context, a app.App, only []string) (workspace.Summary, error) {
	root := a.Config.WorkspaceRoot

	discovered, err := workspace.Discover(root)
	if err != nil {
		return workspace.Summary{}, fmt.Errorf("discovering managed repositories under %q: %w", root, err)
	}

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

	tasks := make([]engine.Task, 0, len(toProcess))
	for _, name := range toProcess {
		name := name
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return r.process(ctx, a, filepath.Join(root, name), name)
		})
	}

	procResults := engine.Run(ctx, a.Config.Concurrency, tasks)

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

// process reconciles a single repository to the development baseline and
// returns its terminal Result. It never returns an error out-of-band: every
// failure is captured into a failed Result so the engine continues with the
// other repositories.
//
// The steps below are ordered so the remote, non-destructive work happens
// first: a fork that has diverged from upstream cannot be synced, and has to be
// reported as failed before any local state is discarded.
func (r *Refresher) process(ctx context.Context, a app.App, path, name string) workspace.Result {
	repo := git.NewRepo(path, a.Git)

	// The fork is named "<prefix><upstream-name>" under the contributor's account;
	// the discovered directory name is the upstream name.
	fork := githubclient.RepoRef{Owner: a.Config.GitHubUser, Name: a.Config.RepoPrefix + name}

	// Dry-run: report the action that would be taken using no mutating GitHub or
	// git operations at all.
	if a.DryRun {
		return workspace.Result{
			Repo:    name,
			Outcome: workspace.OutcomeCreated,
			Reason: fmt.Sprintf(
				"would sync fork %s from upstream, fetch tags, discard local changes, reset %s to %s, and fetch %s",
				fork, defaultBranch, upstreamRef, originRemote),
		}
	}

	// 1. Bring the fork's default branch current with upstream server-side. Done
	// first so a diverged fork is reported without destroying any local work.
	if err := a.GitHub.SyncFork(ctx, fork, defaultBranch); err != nil {
		return failed(name, fmt.Errorf("syncing fork from upstream: %w", err))
	}
	// 2. Upstream commits and every upstream tag.
	if err := repo.FetchWithTags(ctx, upstreamRemote); err != nil {
		return failed(name, fmt.Errorf("fetching %s with tags: %w", upstreamRemote, err))
	}
	// 3. Discard tracked modifications and untracked files so the branch switch
	// cannot be blocked by a dirty working tree.
	if err := repo.ResetHard(ctx); err != nil {
		return failed(name, fmt.Errorf("discarding tracked changes: %w", err))
	}
	if err := repo.Clean(ctx); err != nil {
		return failed(name, fmt.Errorf("removing untracked files: %w", err))
	}
	// 4. Safe now that the working tree is pristine.
	if err := repo.Checkout(ctx, defaultBranch); err != nil {
		return failed(name, fmt.Errorf("switching to %s: %w", defaultBranch, err))
	}
	// 5. Force the local default branch to exactly match upstream (== fork).
	if err := repo.ResetHardTo(ctx, upstreamRef); err != nil {
		return failed(name, fmt.Errorf("resetting %s to %s: %w", defaultBranch, upstreamRef, err))
	}
	// 6. Update origin/main to reflect the server-side sync from step 1. Without
	// this the remote-tracking ref stays stale and `git status` misreports the
	// branch as ahead of the fork even though both point at the same commit.
	if err := repo.Fetch(ctx, originRemote); err != nil {
		return failed(name, fmt.Errorf("fetching %s: %w", originRemote, err))
	}

	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeCreated,
		Reason:  fmt.Sprintf("refreshed: %s reset to upstream, fork synced, tags updated", defaultBranch),
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
