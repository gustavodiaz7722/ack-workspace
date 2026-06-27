// Package releaser implements the Controller_Releaser, which cuts a release for
// a single service controller by mechanizing the manual ACK release workflow:
//
//  1. update the controller's base branch from upstream,
//  2. create a release branch named "release-<version>",
//  3. regenerate the controller's release artifacts via the code-generator's
//     build-controller-release.sh script with RELEASE_VERSION set,
//  4. commit the generated artifacts,
//  5. push the release branch to the contributor's fork, and
//  6. open a pull request against the upstream repository.
//
// Unlike the initializer and adder, the releaser operates on a controller that
// is already present in the workspace; it never forks or clones. It is
// deliberately conservative: a repository with uncommitted local changes is
// skipped so in-flight work is never mixed into a release, a base branch that
// has diverged from upstream is reported as a failure rather than force-updated,
// and in dry-run mode it reports the steps it would take and touches nothing.
package releaser

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	// upstreamOwner is the GitHub organization that hosts the canonical
	// (upstream) ACK repositories. Releases target it as the PR base.
	upstreamOwner = "aws-controllers-k8s"
	// controllerSuffix is the conventional suffix of every service controller
	// repository name. A bare alias ("ecr") and its full form ("ecr-controller")
	// both normalize to the same repository.
	controllerSuffix = "-controller"
	// codegenDirName is the directory under the workspace root that holds the
	// ACK code-generator (and its release build script).
	codegenDirName = "code-generator"
	// releaseScript is the code-generator script that regenerates a controller's
	// release artifacts. It is invoked from the code-generator directory.
	releaseScript = "./scripts/build-controller-release.sh"
	// defaultBaseBranch is the branch a release is cut from when the caller does
	// not override it.
	defaultBaseBranch = "main"
	// releaseVersionEnv is the environment variable the release build script
	// reads to learn which version it is generating.
	releaseVersionEnv = "RELEASE_VERSION"

	upstreamRemote = "upstream"
	originRemote   = "origin"
)

// versionPattern validates a normalized release version: a leading "v" followed
// by a semantic "MAJOR.MINOR.PATCH" core and an optional pre-release/build
// suffix (for example v1.0.1 or v1.2.0-rc.1).
var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.\-]+)?$`)

// UsageError is a typed argument/validation error returned by Release before any
// repository is touched (a missing service identifier or an invalid version).
// The cmd layer maps it to a distinct usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// ScriptRunner executes the controller release build script. It is the seam
// through which the real os/exec invocation is replaced by a scripted mock in
// tests, so the release flow can be exercised without running the actual
// code-generator.
type ScriptRunner interface {
	// Run executes the release build script for service in codegenDir (the
	// code-generator directory), with RELEASE_VERSION set to version.
	Run(ctx context.Context, codegenDir, service, version string) error
}

// Options controls release behavior.
type Options struct {
	// Version is the release version (for example "v1.0.1"). It is required and
	// is normalized to carry a leading "v".
	Version string
	// BaseBranch is the branch the release is cut from. It defaults to "main"
	// when empty.
	BaseBranch string
	// SkipPR, when true, pushes the release branch to the fork but does not open
	// a pull request against upstream.
	SkipPR bool
	// PRBody, when non-empty, overrides the default pull request body. It is
	// ignored when SkipPR is true.
	PRBody string
}

// Releaser implements the Controller_Releaser.
type Releaser struct {
	script ScriptRunner
}

// New returns a Releaser that runs the real code-generator release script.
func New() *Releaser {
	return &Releaser{script: execScriptRunner{}}
}

// NewWithScriptRunner returns a Releaser backed by the supplied ScriptRunner. It
// is intended for tests that need to script or record release-script execution
// without invoking the real code-generator.
func NewWithScriptRunner(s ScriptRunner) *Releaser {
	return &Releaser{script: s}
}

// Release cuts a release for the controller named by service at opts.Version and
// returns a single-result Summary recording the outcome (released, skipped, or
// failed).
//
// The returned error is non-nil only for a pre-flight validation failure (an
// empty service identifier, an invalid version, or a missing identity when a PR
// is requested); all execution problems are captured as a failed Result so the
// caller renders a uniform summary.
func (r *Releaser) Release(ctx context.Context, ap app.App, service string, opts Options) (workspace.Summary, error) {
	alias := strings.TrimSuffix(strings.TrimSpace(service), controllerSuffix)
	if alias == "" {
		return workspace.Summary{}, &UsageError{Msg: "a service identifier is required (for example: ecr or ecr-controller)"}
	}

	version, err := normalizeVersion(opts.Version)
	if err != nil {
		return workspace.Summary{}, err
	}

	base := strings.TrimSpace(opts.BaseBranch)
	if base == "" {
		base = defaultBaseBranch
	}

	// A pull request is opened from "<identity>:<branch>", so the contributor's
	// identity is required unless the caller only wants to push.
	if !opts.SkipPR && strings.TrimSpace(ap.Config.GitHubUser) == "" {
		return workspace.Summary{}, &UsageError{Msg: "a GitHub identity is required to open a pull request; pass --skip-pr to push only"}
	}

	result := r.process(ctx, ap, alias, version, base, opts)
	return workspace.Summary{Results: []workspace.Result{result}}, nil
}

// process runs the full release flow for one controller and returns its terminal
// Result. It never returns an error out-of-band: every failure is captured into
// a failed Result.
func (r *Releaser) process(ctx context.Context, ap app.App, alias, version, base string, opts Options) workspace.Result {
	name := alias + controllerSuffix
	root := ap.Config.WorkspaceRoot
	controllerPath := filepath.Join(root, name)
	codegenPath := filepath.Join(root, codegenDirName)
	branch := "release-" + version

	// Pre-flight: the controller and the code-generator must already be present
	// in the workspace. Releasing neither forks nor clones.
	if !dirExists(controllerPath) {
		return failed(name, fmt.Errorf("controller %s not found at %s; add it first with `ack-workspace add %s`", name, controllerPath, alias))
	}
	if !isGitRepo(controllerPath) {
		return failed(name, fmt.Errorf("%s is not a git repository", controllerPath))
	}
	if !dirExists(codegenPath) {
		return failed(name, fmt.Errorf("code-generator not found at %s; run `ack-workspace init` first", codegenPath))
	}

	repo := git.NewRepo(controllerPath, ap.Git)

	// Dry-run: report the steps that would be taken without mutating anything.
	if ap.DryRun {
		return r.preview(name, branch, base, version, opts)
	}

	// Safety: never run a release on top of uncommitted local work, which could
	// be swept into the release commit by `git commit -a`.
	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("checking working tree: %w", err))
	}
	if dirty {
		return skipped(name, "uncommitted changes")
	}

	// 1. Update the base branch from upstream (fast-forward only).
	if err := repo.Fetch(ctx, upstreamRemote); err != nil {
		return failed(name, fmt.Errorf("fetching %s: %w", upstreamRemote, err))
	}
	if err := repo.Checkout(ctx, base); err != nil {
		return failed(name, fmt.Errorf("checking out base branch %q: %w", base, err))
	}
	upstreamRef := upstreamRemote + "/" + base
	canFF, err := repo.CanFastForward(ctx, base, upstreamRef)
	if err != nil {
		return failed(name, fmt.Errorf("checking fast-forward against %s: %w", upstreamRef, err))
	}
	if !canFF {
		return failed(name, fmt.Errorf("base branch %q has diverged from %s; reconcile it before releasing", base, upstreamRef))
	}
	if err := repo.FastForward(ctx, base, upstreamRef); err != nil {
		return failed(name, fmt.Errorf("updating %q from %s: %w", base, upstreamRef, err))
	}

	// 2. Create the release branch. If it already exists, skip rather than
	// clobber a release that may already be in progress.
	exists, err := repo.LocalBranchExists(ctx, branch)
	if err != nil {
		return failed(name, fmt.Errorf("checking for release branch %q: %w", branch, err))
	}
	if exists {
		return skipped(name, fmt.Sprintf("release branch %q already exists", branch))
	}
	if err := repo.CheckoutNewBranch(ctx, branch); err != nil {
		return failed(name, fmt.Errorf("creating release branch %q: %w", branch, err))
	}

	// 3. Regenerate the release artifacts via the code-generator script.
	if err := r.script.Run(ctx, codegenPath, alias, version); err != nil {
		return failed(name, fmt.Errorf("generating release artifacts: %w", err))
	}

	// 4. Commit the generated artifacts. If the script produced no changes the
	// release is a no-op; report it as skipped instead of creating an empty
	// commit.
	changed, err := repo.IsDirty(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("checking generated changes: %w", err))
	}
	if !changed {
		return skipped(name, "release script produced no changes")
	}
	if err := repo.CommitAll(ctx, commitMessage(version)); err != nil {
		return failed(name, fmt.Errorf("committing release artifacts: %w", err))
	}

	// 5. Push the release branch to the contributor's fork.
	if err := repo.Push(ctx, originRemote, branch); err != nil {
		return failed(name, fmt.Errorf("pushing %q to %s: %w", branch, originRemote, err))
	}

	// 6. Open a pull request against upstream, unless the caller only wanted to
	// push.
	if opts.SkipPR {
		return released(name, fmt.Sprintf("released %s; pushed %q to %s (PR skipped)", version, branch, originRemote))
	}
	prURL, err := r.openPullRequest(ctx, ap, name, branch, base, version, opts.PRBody)
	if err != nil {
		return failed(name, fmt.Errorf("opening pull request: %w", err))
	}
	return released(name, fmt.Sprintf("released %s (%s)", version, prURL))
}

// openPullRequest opens the upstream PR from the contributor's release branch
// and returns its URL. When body is non-empty it overrides the default,
// generated PR body.
func (r *Releaser) openPullRequest(ctx context.Context, ap app.App, name, branch, base, version, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		body = defaultPRBody(name, version)
	}
	upstreamRef := githubclient.RepoRef{Owner: upstreamOwner, Name: name}
	return ap.GitHub.CreatePullRequest(ctx, upstreamRef, githubclient.NewPullRequest{
		Title: commitMessage(version),
		Body:  body,
		Head:  ap.Config.GitHubUser + ":" + branch,
		Base:  base,
	})
}

// defaultPRBody builds the generated pull request body used when the caller does
// not supply one.
func defaultPRBody(name, version string) string {
	return fmt.Sprintf(
		"Release artifacts for `%s` version `%s`.\n\nOpened by `ack-workspace release`.",
		name, version)
}

// preview computes the release steps for a dry-run without mutating anything.
func (r *Releaser) preview(name, branch, base, version string, opts Options) workspace.Result {
	reason := fmt.Sprintf(
		"would update %q from upstream, create branch %q, run %s, commit %q, and push to %s",
		base, branch, releaseScript, commitMessage(version), originRemote)
	if !opts.SkipPR {
		reason += fmt.Sprintf("; then open a PR to %s/%s", upstreamOwner, name)
	}
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: reason}
}

// commitMessage builds the conventional release commit/PR title for version.
func commitMessage(version string) string {
	return fmt.Sprintf("Release artifacts for release %s", version)
}

// normalizeVersion trims surrounding whitespace, ensures a single leading "v",
// and validates the result looks like a semantic version. An empty or malformed
// version is reported as a *UsageError.
func normalizeVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", &UsageError{Msg: "a release version is required (for example: --version v1.0.1)"}
	}
	v = "v" + strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if !versionPattern.MatchString(v) {
		return "", &UsageError{Msg: fmt.Sprintf("invalid release version %q: expected a semantic version such as v1.0.1", v)}
	}
	return v, nil
}

// execScriptRunner is the production ScriptRunner. It invokes the code-generator
// release build script with the working directory set to the code-generator
// directory and RELEASE_VERSION exported in the environment.
type execScriptRunner struct{}

// Run executes `./scripts/build-controller-release.sh <service>` in codegenDir
// with RELEASE_VERSION=<version> added to the inherited environment. On failure
// it surfaces any script output to aid debugging.
func (execScriptRunner) Run(ctx context.Context, codegenDir, service, version string) error {
	cmd := exec.CommandContext(ctx, releaseScript, service)
	cmd.Dir = codegenDir
	cmd.Env = append(os.Environ(), releaseVersionEnv+"="+version)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(combined.String())
		if out != "" {
			return fmt.Errorf("%s %s: %w\n%s", releaseScript, service, err, out)
		}
		return fmt.Errorf("%s %s: %w", releaseScript, service, err)
	}
	return nil
}

// released builds a successful (OutcomeCreated) Result with the given reason.
func released(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: reason}
}

// skipped builds a skipped Result carrying a human-readable reason.
func skipped(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSkipped, Reason: reason}
}

// failed builds a failed Result carrying the underlying error and its text.
func failed(name string, err error) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeFailed, Reason: err.Error(), Err: err}
}

// dirExists reports whether path exists.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isGitRepo reports whether dir contains a ".git" entry (a clone or worktree
// gitfile).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
