// Package remover implements the Controller_Remover, the inverse of the
// Controller_Adder: it tears down a service controller workspace entry by
// deleting its local clone and its GitHub fork.
//
// This is a destructive, irreversible operation (a deleted fork cannot be
// recovered), so the component is deliberately conservative:
//
//   - It only ever deletes a fork owned by the contributor's GitHub identity; it
//     refuses to target the upstream organization (a hard guard against deleting
//     the canonical ACK repositories).
//   - A repository whose working tree has uncommitted changes is skipped unless
//     the caller forces removal, so local work is not silently destroyed.
//   - In dry-run mode it reports what it would delete and touches nothing.
//
// Confirmation of the destructive intent is the responsibility of the CLI layer;
// by the time Remove is called the user has already opted in (or passed
// --dry-run).
package remover

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
// ACK repositories. The remover refuses to delete anything owned by it.
const upstreamOwner = "aws-controllers-k8s"

// controllerSuffix is the conventional suffix of every Service_Controller_Repository
// name. A bare Service_Alias ("s3") and its full form ("s3-controller") both
// normalize to the same repository.
const controllerSuffix = "-controller"

// allToken is the special identifier that expands to every managed controller
// repository found under the workspace root. It is matched case-insensitively.
const allToken = "all"

// UsageError is a typed argument/validation error returned by Remove before any
// repository is touched. The cmd layer maps it to a usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Options controls removal behavior.
type Options struct {
	// KeepFork, when true, deletes only the local clone and leaves the GitHub
	// fork intact.
	KeepFork bool
	// Force, when true, removes a repository even if its working tree has
	// uncommitted changes. Without it, a dirty repository is skipped.
	Force bool
}

// Remover implements the Controller_Remover.
type Remover struct{}

// New returns a ready-to-use Remover.
func New() *Remover { return &Remover{} }

// Remove deletes the local clone and GitHub fork of each identified controller,
// returning a Summary in which every processed target is recorded in exactly one
// of the removed (OutcomeCreated), skipped, or failed buckets.
//
// The special "all" identifier expands to every managed controller repository
// discovered under the workspace root; it supersedes any other identifiers. An
// empty identifier list is rejected with a *UsageError before any change.
//
// The returned error is non-nil only for a pre-flight failure (empty list, an
// invalid identity, or a discovery failure when expanding "all"); all
// per-repository problems are captured as failed Results.
func (r *Remover) Remove(ctx context.Context, ap app.App, identifiers []string, opts Options) (workspace.Summary, error) {
	if len(identifiers) == 0 {
		return workspace.Summary{}, &UsageError{Msg: "at least one service identifier (or 'all') is required"}
	}

	// Hard safety guard: the fork owner must be the contributor, never the
	// upstream organization. This makes it impossible to delete a canonical ACK
	// repository even if misconfigured.
	if ap.Config.GitHubUser == "" {
		return workspace.Summary{}, &UsageError{Msg: "a GitHub identity is required to identify which forks to delete"}
	}
	if strings.EqualFold(ap.Config.GitHubUser, upstreamOwner) {
		return workspace.Summary{}, &UsageError{Msg: fmt.Sprintf(
			"refusing to operate: the configured GitHub identity %q is the upstream organization", ap.Config.GitHubUser)}
	}

	names, err := r.resolve(ap, identifiers)
	if err != nil {
		return workspace.Summary{}, err
	}

	tasks := make([]engine.Task, 0, len(names))
	for _, name := range names {
		name := name
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return r.process(ctx, ap, name, opts)
		})
	}

	results := engine.Run(ctx, ap.Config.Concurrency, tasks)
	return workspace.Summary{Results: results}, nil
}

// resolve turns the supplied identifiers into the concrete set of upstream
// controller names ("<alias>-controller") to remove. When "all" is present it
// supersedes the rest and expands to the managed controllers discovered under
// the workspace root.
func (r *Remover) resolve(ap app.App, identifiers []string) ([]string, error) {
	for _, id := range identifiers {
		if strings.EqualFold(strings.TrimSpace(id), allToken) {
			return r.discoverControllers(ap)
		}
	}

	seen := make(map[string]bool, len(identifiers))
	names := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		alias := strings.TrimSuffix(strings.TrimSpace(id), controllerSuffix)
		if alias == "" {
			// Preserve the invalid token so it is reported as failed during
			// processing rather than silently dropped.
			names = append(names, strings.TrimSpace(id))
			continue
		}
		name := alias + controllerSuffix
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// discoverControllers lists managed controller repositories (directories ending
// in "-controller") directly under the workspace root, sorted by name.
func (r *Remover) discoverControllers(ap app.App) ([]string, error) {
	repos, err := workspace.Discover(ap.Config.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering managed repositories under %q: %w", ap.Config.WorkspaceRoot, err)
	}
	controllers := make([]string, 0, len(repos))
	for _, name := range repos {
		if strings.HasSuffix(name, controllerSuffix) {
			controllers = append(controllers, name)
		}
	}
	sort.Strings(controllers)
	return controllers, nil
}

// process removes a single controller's local clone and fork, returning its
// terminal Result. It never returns an error out-of-band.
func (r *Remover) process(ctx context.Context, ap app.App, name string, opts Options) workspace.Result {
	// Guard against a malformed/empty name slipping through resolution.
	if name == "" || !strings.HasSuffix(name, controllerSuffix) {
		err := fmt.Errorf("invalid controller identifier %q", name)
		return failed(name, err)
	}

	localPath := filepath.Join(ap.Config.WorkspaceRoot, name)
	forkRef := githubclient.RepoRef{Owner: ap.Config.GitHubUser, Name: ap.Config.RepoPrefix + name}

	localExists := dirExists(localPath)

	forkExists := false
	if !opts.KeepFork {
		exists, err := ap.GitHub.RepoExists(ctx, forkRef)
		if err != nil {
			return failed(name, fmt.Errorf("checking fork %s: %w", forkRef, err))
		}
		forkExists = exists
	}

	// Nothing to do.
	if !localExists && !forkExists {
		return skipped(name, "nothing to remove")
	}

	// Safety: do not destroy a repository with uncommitted local changes unless
	// forced. The check is best-effort; if it cannot be determined the repo is
	// treated as clean so removal can proceed.
	if localExists && !opts.Force {
		if dirty, err := git.NewRepo(localPath, ap.Git).IsDirty(ctx); err == nil && dirty {
			return skipped(name, "uncommitted changes (use --force to remove anyway)")
		}
	}

	// Dry-run: report the action without performing it.
	if ap.DryRun {
		return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: previewReason(localExists, forkExists, opts)}
	}

	// Delete the fork first (the irreversible remote action). On failure leave
	// the local clone intact so the user can retry.
	if forkExists {
		if forkRef.Owner == "" || strings.EqualFold(forkRef.Owner, upstreamOwner) {
			// Defensive: should be unreachable given the guards in Remove.
			return failed(name, fmt.Errorf("refusing to delete fork with owner %q", forkRef.Owner))
		}
		if err := ap.GitHub.DeleteRepo(ctx, forkRef); err != nil {
			return failed(name, fmt.Errorf("deleting fork %s: %w", forkRef, err))
		}
	}

	// Delete the local clone.
	if localExists {
		if err := os.RemoveAll(localPath); err != nil {
			return failed(name, fmt.Errorf("removing local clone %s: %w", localPath, err))
		}
	}

	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: removedReason(localExists, forkExists, opts)}
}

// previewReason describes the action a dry-run would take.
func previewReason(localExists, forkExists bool, opts Options) string {
	switch {
	case localExists && forkExists:
		return "would delete local clone and fork"
	case localExists && opts.KeepFork:
		return "would delete local clone (fork kept)"
	case localExists:
		return "would delete local clone (no fork found)"
	case forkExists:
		return "would delete fork (no local clone)"
	default:
		return "nothing to remove"
	}
}

// removedReason describes what was actually removed.
func removedReason(localExists, forkExists bool, opts Options) string {
	switch {
	case localExists && forkExists:
		return "deleted local clone and fork"
	case localExists && opts.KeepFork:
		return "deleted local clone (fork kept)"
	case localExists:
		return "deleted local clone (no fork found)"
	case forkExists:
		return "deleted fork (no local clone)"
	default:
		return "removed"
	}
}

func skipped(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSkipped, Reason: reason}
}

func failed(name string, err error) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeFailed, Reason: err.Error(), Err: err}
}

// dirExists reports whether path exists.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
