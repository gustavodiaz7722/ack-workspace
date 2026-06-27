package initializer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/engine"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// upstreamOwner is the GitHub organization that hosts the canonical (upstream)
// ACK repositories.
const upstreamOwner = "aws-controllers-k8s"

// CommonRepositories is the fixed set of core ACK repositories that every
// contributor needs (Requirement 3.3). It is a single constant so the set can
// be adjusted in one place should the canonical setup change.
//
// ack-dev-skills packages the ACK development guidance as an Agent Skill; it is
// bootstrapped the same way as the other core repositories (forked, cloned, and
// configured) so it sits as a peer next to runtime/code-generator/test-infra in
// the workspace root, ready to be installed into the contributor's AI tool.
var CommonRepositories = []string{"runtime", "code-generator", "test-infra", "ack-dev-skills"}

// Initializer implements the Workspace_Initializer. It ensures the
// Workspace_Root exists and then forks, clones, and configures each
// Common_Repository, reporting an exhaustive, mutually-exclusive summary.
type Initializer struct{}

// New returns a ready-to-use Initializer.
func New() *Initializer { return &Initializer{} }

// Init ensures the Workspace_Root exists (aborting the whole command if its
// creation fails) and then processes each Common_Repository concurrently,
// returning a Summary in which every repository is recorded in exactly one of
// the created, skipped, or failed buckets (Requirement 3).
//
// The returned error is non-nil only for the pre-flight failure of creating the
// Workspace_Root (Requirements 3.1, 3.2); in that case no repository is
// processed and the Summary is empty. All per-repository problems are captured
// as failed Results and never surface as the returned error.
func (i *Initializer) Init(ctx context.Context, a app.App) (workspace.Summary, error) {
	root := a.Config.WorkspaceRoot

	// Requirements 3.1, 3.2: create the Workspace_Root first; if that fails,
	// abort without processing any Common_Repository.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return workspace.Summary{}, fmt.Errorf("creating workspace root %q: %w", root, err)
	}

	// Requirement 3.3: build one task per Common_Repository and run them with
	// the configured bounded concurrency (Requirement 7).
	tasks := make([]engine.Task, 0, len(CommonRepositories))
	for _, name := range CommonRepositories {
		spec := i.specFor(a, name, root)
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return i.process(ctx, a, spec)
		})
	}

	results := engine.Run(ctx, a.Config.Concurrency, tasks)

	return workspace.Summary{Results: results}, nil
}

// specFor builds the RepoSpec for a single Common_Repository: the upstream lives
// under the ACK organization, the fork is named "<prefix><name>" under the
// contributor's account, and the local checkout uses the unprefixed name so it
// matches the conventional ACK Go import path.
func (i *Initializer) specFor(a app.App, name, root string) workspace.RepoSpec {
	return workspace.RepoSpec{
		UpstreamOwner: upstreamOwner,
		UpstreamName:  name,
		ForkOwner:     a.Config.GitHubUser,
		ForkName:      a.Config.RepoPrefix + name,
		LocalPath:     filepath.Join(root, name),
	}
}

// process runs the fork/clone/configure flow for a single repository and returns
// its terminal Result. It never returns an error out-of-band: every failure is
// captured into a failed Result so the engine continues with the other
// repositories (Requirement 3.5, 3.9, 3.10; Property 3).
func (i *Initializer) process(ctx context.Context, a app.App, spec workspace.RepoSpec) workspace.Result {
	// Requirement 3.8: if the local directory already exists, skip cloning
	// regardless of its contents. This check runs before any GitHub or clone
	// work, and guarantees cleanup never touches a pre-existing directory
	// (Property 7).
	if dirExists(spec.LocalPath) {
		return workspace.Result{
			Repo:    spec.UpstreamName,
			Outcome: workspace.OutcomeSkipped,
			Reason:  "directory already exists",
		}
	}

	// Requirements 3.4, 3.5: ensure the fork exists, creating it when missing.
	forkRef := githubclient.RepoRef{Owner: spec.ForkOwner, Name: spec.ForkName}
	upstreamRef := githubclient.RepoRef{Owner: spec.UpstreamOwner, Name: spec.UpstreamName}

	exists, err := a.GitHub.RepoExists(ctx, forkRef)
	if err != nil {
		return failed(spec, fmt.Errorf("checking fork %s: %w", forkRef, err))
	}

	// Dry-run (Requirements 8.4, 8.5; Property 6): the decision is now fully
	// determined from read-only inspection alone (the directory does not exist
	// and we know whether the fork exists). Report the action that would be
	// taken and return without invoking any mutating operation (CreateFork,
	// Clone, SetRemote).
	if a.DryRun {
		reason := "would create fork and clone"
		if exists {
			reason = "would clone existing fork"
		}
		return workspace.Result{
			Repo:    spec.UpstreamName,
			Outcome: workspace.OutcomeCreated,
			Reason:  reason,
		}
	}

	if !exists {
		if _, err := a.GitHub.CreateFork(ctx, upstreamRef, spec.ForkName); err != nil {
			return failed(spec, fmt.Errorf("creating fork %s: %w", forkRef, err))
		}
	}

	// Requirements 3.6, 3.9: clone the fork into the local path; on failure
	// remove any partially created directory and record the repository failed.
	forkURL := repoURL(spec.ForkOwner, spec.ForkName)
	repo, err := git.Clone(ctx, a.Git, forkURL, spec.LocalPath)
	if err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("cloning fork %s: %w", forkRef, err))
	}

	// Requirements 3.7, 3.10: configure origin -> fork and upstream -> org; on
	// failure remove the cloned directory and record the repository failed.
	if err := repo.SetRemote(ctx, "origin", forkURL); err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("configuring origin remote: %w", err))
	}
	upstreamURL := repoURL(spec.UpstreamOwner, spec.UpstreamName)
	if err := repo.SetRemote(ctx, "upstream", upstreamURL); err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("configuring upstream remote: %w", err))
	}

	// Requirement 3.7: record the repository as created on success.
	return workspace.Result{
		Repo:    spec.UpstreamName,
		Outcome: workspace.OutcomeCreated,
	}
}

// failed builds a failed Result carrying the underlying error and its text as
// the human-readable reason.
func failed(spec workspace.RepoSpec, err error) workspace.Result {
	return workspace.Result{
		Repo:    spec.UpstreamName,
		Outcome: workspace.OutcomeFailed,
		Reason:  err.Error(),
		Err:     err,
	}
}

// repoURL builds the HTTPS clone URL for a GitHub repository.
func repoURL(owner, name string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

// dirExists reports whether path exists as a directory entry. Any stat result
// without an error is treated as "exists" so that a pre-existing repository
// directory is skipped (Requirement 3.8) and never removed by cleanup.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeRunCreated removes a directory the current run created during failure
// cleanup. It is only ever called for a LocalPath that did not exist when
// processing began (the pre-existing case is handled by the skipped branch
// above), so it never deletes a directory the tool did not create (Property 7).
func removeRunCreated(path string) {
	_ = os.RemoveAll(path)
}
