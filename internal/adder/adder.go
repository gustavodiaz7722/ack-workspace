// Package adder implements the Controller_Adder, which forks, clones, and
// configures Service_Controller_Repositories named for arbitrary service
// identifiers.
//
// It mirrors the Workspace_Initializer's per-repository fork/clone/configure
// machinery (see internal/initializer) but operates on caller-supplied service
// identifiers instead of the fixed set of Common_Repositories. The small file
// helpers (dirExists, removeRunCreated, repoURL) and the upstreamOwner constant
// are replicated locally rather than imported from the initializer package to
// avoid coupling the two components.
package adder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/engine"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// upstreamOwner is the GitHub organization that hosts the canonical (upstream)
// ACK repositories.
const upstreamOwner = "aws-controllers-k8s"

// controllerSuffix is the conventional suffix of every Service_Controller_Repository
// name. A bare Service_Alias (for example "s3") and its full form
// ("s3-controller") both normalize to the same repository.
const controllerSuffix = "-controller"

// allToken is the special identifier that expands to every Service_Controller_Repository
// available in the Upstream_Organization. It is matched case-insensitively. When
// present anywhere in the identifier list it supersedes the other identifiers.
const allToken = "all"

// UsageError is a typed argument/validation error returned by Add before any
// repository is processed. The cmd layer maps it to a distinct non-zero exit
// code (Requirement 4.2). It is deliberately distinct from per-repository
// failures, which are recorded as failed Results in the Summary instead.
type UsageError struct {
	// Msg is the human-readable validation message.
	Msg string
}

// Error implements error.
func (e *UsageError) Error() string { return e.Msg }

// Adder implements the Controller_Adder. It forks, clones, and configures the
// Service_Controller_Repository for each supplied identifier, reporting an
// exhaustive, mutually-exclusive added/skipped/failed summary.
type Adder struct{}

// New returns a ready-to-use Adder.
func New() *Adder { return &Adder{} }

// Add processes each supplied service identifier independently and returns a
// Summary in which every processed identifier is recorded in exactly one of the
// added (OutcomeCreated), skipped, or failed buckets (Requirement 4).
//
// The returned error is non-nil only for the pre-flight rejection of an empty
// identifier list (Requirement 4.2); in that case no modification is made under
// the Workspace_Root and the Summary is empty. All per-identifier problems are
// captured as failed Results and never surface as the returned error
// (Requirements 4.3, 4.5, 4.8; Property 3).
func (a *Adder) Add(ctx context.Context, ap app.App, identifiers []string) (workspace.Summary, error) {
	// Requirement 4.2: reject an empty identifier list before making any change
	// under the Workspace_Root.
	if len(identifiers) == 0 {
		return workspace.Summary{}, &UsageError{Msg: "at least one service identifier is required"}
	}

	// Expand the special "all" identifier to every Service_Controller_Repository
	// available in the Upstream_Organization. This is done before any change is
	// made under the Workspace_Root; a discovery failure aborts the command
	// without side effects.
	identifiers, err := a.expand(ctx, ap, identifiers)
	if err != nil {
		return workspace.Summary{}, err
	}

	root := ap.Config.WorkspaceRoot

	// Requirement 4.1 / 7: build one task per identifier and run them with the
	// configured bounded concurrency. Each task processes its identifier
	// independently and never aborts the batch on failure.
	tasks := make([]engine.Task, 0, len(identifiers))
	for _, identifier := range identifiers {
		identifier := identifier
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return a.processIdentifier(ctx, ap, identifier, root)
		})
	}

	results := engine.Run(ctx, ap.Config.Concurrency, tasks)
	return workspace.Summary{Results: results}, nil
}

// expand resolves the supplied identifiers into the concrete list to process.
// When the special "all" token (case-insensitive) appears anywhere in the list
// it supersedes the other identifiers: every Service_Controller_Repository in
// the Upstream_Organization is discovered via the GitHub_Client, filtered to the
// "<alias>-controller" naming convention, de-duplicated, and returned sorted.
// Otherwise the identifiers are returned unchanged.
//
// A discovery (listing) failure is returned as the function error so the command
// fails fast without touching the Workspace_Root; finding no controllers is
// reported as a *UsageError so the caller exits with a usage code rather than
// silently doing nothing.
func (a *Adder) expand(ctx context.Context, ap app.App, identifiers []string) ([]string, error) {
	if !containsAll(identifiers) {
		return identifiers, nil
	}

	repos, err := ap.GitHub.ListOrgRepos(ctx, upstreamOwner)
	if err != nil {
		return nil, fmt.Errorf("listing %s controller repositories: %w", upstreamOwner, err)
	}

	seen := make(map[string]bool, len(repos))
	controllers := make([]string, 0, len(repos))
	for _, name := range repos {
		if !strings.HasSuffix(name, controllerSuffix) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		controllers = append(controllers, name)
	}
	if len(controllers) == 0 {
		return nil, &UsageError{Msg: fmt.Sprintf(
			"no controller repositories found in the %s organization", upstreamOwner)}
	}
	sort.Strings(controllers)
	return controllers, nil
}

// containsAll reports whether the identifier list includes the special "all"
// token, ignoring surrounding whitespace and case.
func containsAll(identifiers []string) bool {
	for _, id := range identifiers {
		if strings.EqualFold(strings.TrimSpace(id), allToken) {
			return true
		}
	}
	return false
}

// processIdentifier normalizes a single identifier and runs the
// fork/clone/configure flow for the resolved Service_Controller_Repository.
//
// Requirement 4.1: leading/trailing whitespace is trimmed and both the bare
// Service_Alias and the full "<alias>-controller" form normalize to the same
// repository named "<alias>-controller".
func (a *Adder) processIdentifier(ctx context.Context, ap app.App, identifier, root string) workspace.Result {
	alias := strings.TrimSuffix(strings.TrimSpace(identifier), controllerSuffix)
	if alias == "" {
		err := fmt.Errorf("invalid service identifier %q", identifier)
		return workspace.Result{
			Repo:    strings.TrimSpace(identifier),
			Outcome: workspace.OutcomeFailed,
			Reason:  err.Error(),
			Err:     err,
		}
	}
	return a.process(ctx, ap, a.specFor(ap, alias, root))
}

// specFor builds the RepoSpec for a normalized Service_Alias: the upstream lives
// under the ACK organization as "<alias>-controller", the fork is named
// "<prefix><alias>-controller" under the contributor's account, and the local
// checkout uses the unprefixed "<alias>-controller" name so it matches the
// conventional ACK Go import path.
func (a *Adder) specFor(ap app.App, alias, root string) workspace.RepoSpec {
	name := alias + controllerSuffix
	return workspace.RepoSpec{
		UpstreamOwner: upstreamOwner,
		UpstreamName:  name,
		ForkOwner:     ap.Config.GitHubUser,
		ForkName:      ap.Config.RepoPrefix + name,
		LocalPath:     filepath.Join(root, name),
	}
}

// process runs the resolve/fork/clone/configure flow for a single resolved
// Service_Controller_Repository and returns its terminal Result. It never
// returns an error out-of-band: every failure is captured into a failed Result
// so the engine continues processing the remaining identifiers (Requirements
// 4.3, 4.5, 4.8; Property 3).
func (a *Adder) process(ctx context.Context, ap app.App, spec workspace.RepoSpec) workspace.Result {
	upstreamRef := githubclient.RepoRef{Owner: spec.UpstreamOwner, Name: spec.UpstreamName}
	forkRef := githubclient.RepoRef{Owner: spec.ForkOwner, Name: spec.ForkName}

	// Requirement 4.3: resolve the repository's existence against the
	// organization. A not-found result or an API error both record the
	// identifier as failed and let the batch continue.
	exists, err := ap.GitHub.RepoExists(ctx, upstreamRef)
	if err != nil {
		return failed(spec, fmt.Errorf("resolving %s: %w", upstreamRef, err))
	}
	if !exists {
		return failed(spec, fmt.Errorf("service controller repository %s does not exist", upstreamRef))
	}

	// Requirements 4.4, 4.5: ensure the fork exists, creating it when missing;
	// a fork-create failure records the identifier as failed and continues.
	forkExists, err := ap.GitHub.RepoExists(ctx, forkRef)
	if err != nil {
		return failed(spec, fmt.Errorf("checking fork %s: %w", forkRef, err))
	}

	// Dry-run (Requirements 8.4, 8.5; Property 6): the decision is fully
	// determined from read-only inspection alone (upstream exists, whether the
	// fork exists, and whether the local directory is present). Report the
	// action that would be taken and return without invoking any mutating
	// operation (CreateFork, Clone, SetRemote).
	if ap.DryRun {
		if dirExists(spec.LocalPath) {
			return workspace.Result{
				Repo:    spec.UpstreamName,
				Outcome: workspace.OutcomeSkipped,
				Reason:  "directory already exists",
			}
		}
		reason := "would create fork and clone"
		if forkExists {
			reason = "would clone existing fork"
		}
		return workspace.Result{
			Repo:    spec.UpstreamName,
			Outcome: workspace.OutcomeCreated,
			Reason:  reason,
		}
	}

	if !forkExists {
		if _, err := ap.GitHub.CreateFork(ctx, upstreamRef, spec.ForkName); err != nil {
			return failed(spec, fmt.Errorf("creating fork %s: %w", forkRef, err))
		}
	}

	// Requirement 4.7: if the local directory already exists, skip cloning
	// regardless of its contents and record it as already present. This check
	// runs before the clone so failure cleanup never touches a pre-existing
	// directory (Property 7).
	if dirExists(spec.LocalPath) {
		return workspace.Result{
			Repo:    spec.UpstreamName,
			Outcome: workspace.OutcomeSkipped,
			Reason:  "directory already exists",
		}
	}

	// Requirement 4.6: clone the fork into the local path; on failure remove any
	// partially created directory and record the identifier failed.
	forkURL := repoURL(spec.ForkOwner, spec.ForkName)
	repo, err := git.Clone(ctx, ap.Git, forkURL, spec.LocalPath)
	if err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("cloning fork %s: %w", forkRef, err))
	}

	// Requirements 4.6, 4.8: configure origin -> fork and upstream -> org; on
	// failure remove the cloned directory and record the identifier failed.
	if err := repo.SetRemote(ctx, "origin", forkURL); err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("configuring origin remote: %w", err))
	}
	upstreamURL := repoURL(spec.UpstreamOwner, spec.UpstreamName)
	if err := repo.SetRemote(ctx, "upstream", upstreamURL); err != nil {
		removeRunCreated(spec.LocalPath)
		return failed(spec, fmt.Errorf("configuring upstream remote: %w", err))
	}

	// Requirement 4.6: record the repository as added (OutcomeCreated) on success.
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
// directory is skipped (Requirement 4.7) and never removed by cleanup.
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
