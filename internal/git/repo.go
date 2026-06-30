package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Repo composes a Runner into the high-level git operations the feature
// components (initializer, adder, refresher, inspector) need. It binds a single
// on-disk repository (Path) to the Runner that executes git within it, so
// callers can build a Repo around an existing checkout with an injected Runner
// (real or mock) and drive clone/remote/fetch/compare/fast-forward/push flows.
//
// Every method maps to a stable git plumbing command (see the design's "Git
// plumbing mapping"); the parsing here is deliberately tolerant of trailing
// whitespace so it behaves identically against real git output and scripted
// mock output.
type Repo struct {
	// Path is the absolute filesystem path of the repository working tree. All
	// of this Repo's git commands run with this directory as their working dir.
	Path string
	// runner executes the underlying git commands. It is injected so tests can
	// substitute a recording mock.
	runner Runner
}

// NewRepo binds an existing repository path to a Runner. Callers that already
// know a repository's location (for example after discovery, or after a fresh
// clone) use this to obtain a Repo without re-cloning.
func NewRepo(path string, runner Runner) *Repo {
	return &Repo{Path: path, runner: runner}
}

// Runner returns the Runner backing this Repo. It is exposed so callers can
// issue additional ad-hoc commands through the same seam when needed.
func (r *Repo) Runner() Runner { return r.runner }

// ExitError reports that a git command terminated with a non-zero exit status
// and carries the numeric exit Code. It lets callers distinguish a meaningful
// non-zero exit (for example `git merge-base --is-ancestor` returning 1 to mean
// "not an ancestor") from a genuine execution failure (a missing repository,
// bad revision, etc., which use higher exit codes).
//
// The production ExecRunner wraps the standard library's *exec.ExitError with
// %w, so its exit code is already reachable via errors.As; tests using
// MockRunner can return an *ExitError directly to simulate a specific exit
// status. exitCodeOf understands both representations.
type ExitError struct {
	// Code is the process exit status reported by git.
	Code int
	// Err is the underlying error, when one is available.
	Err error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("git exited with status %d: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("git exited with status %d", e.Code)
}

// Unwrap exposes the wrapped error for errors.Is/As traversal.
func (e *ExitError) Unwrap() error { return e.Err }

// exitCodeOf extracts a git process exit code from an error returned by a
// Runner. It looks through the wrapped chain for either this package's
// *ExitError (returned by tests) or the standard library's *exec.ExitError
// (wrapped by ExecRunner), returning the code and true when one is found.
//
// This is the single place that interprets "what exit code did git return",
// so the same logic applies whether the command ran for real or through a mock.
func exitCodeOf(err error) (int, bool) {
	var ge *ExitError
	if errors.As(err, &ge) {
		return ge.Code, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// Clone runs `git clone <url> <dest>` and returns a Repo bound to dest. The
// clone itself runs in the process working directory because dest does not yet
// exist; subsequent operations on the returned Repo run inside dest.
func Clone(ctx context.Context, runner Runner, url, dest string) (*Repo, error) {
	if _, err := runner.Run(ctx, "", "clone", url, dest); err != nil {
		return nil, fmt.Errorf("clone %s into %s: %w", url, dest, err)
	}
	return &Repo{Path: dest, runner: runner}, nil
}

// SetRemote points the named remote (origin or upstream) at url. It is
// idempotent: it first attempts `git remote set-url <name> <url>` to update an
// existing remote, and falls back to `git remote add <name> <url>` when the
// remote does not yet exist. This covers both the freshly-cloned case (where
// `origin` already exists and must be repointed at the fork) and adding a brand
// new `upstream` remote.
func (r *Repo) SetRemote(ctx context.Context, name, url string) error {
	if _, err := r.runner.Run(ctx, r.Path, "remote", "set-url", name, url); err == nil {
		return nil
	}
	// set-url failed, most commonly because the remote does not exist yet; add it.
	if _, err := r.runner.Run(ctx, r.Path, "remote", "add", name, url); err != nil {
		return fmt.Errorf("set remote %q to %s: %w", name, url, err)
	}
	return nil
}

// Fetch runs `git fetch <remote>` to update remote-tracking refs without
// touching the working tree or local branches.
func (r *Repo) Fetch(ctx context.Context, remote string) error {
	if _, err := r.runner.Run(ctx, r.Path, "fetch", remote); err != nil {
		return fmt.Errorf("fetch %q: %w", remote, err)
	}
	return nil
}

// CurrentBranch reports the checked-out branch name. It runs
// `git symbolic-ref --short -q HEAD`; an empty result means HEAD does not point
// at a branch, i.e. a detached HEAD state, in which case it returns
// ("", true, nil). The -q flag makes git exit with status 1 (and no output) on
// a detached HEAD, which is treated as the detached case rather than an error;
// any other non-zero exit is surfaced as a real error.
func (r *Repo) CurrentBranch(ctx context.Context) (name string, detached bool, err error) {
	out, runErr := r.runner.Run(ctx, r.Path, "symbolic-ref", "--short", "-q", "HEAD")
	// Handle the error first: git may write diagnostic text to the combined
	// output, so the output is only a trustworthy branch name when the command
	// succeeded. A detached HEAD makes `symbolic-ref -q` exit with status 1 and
	// no branch name; any other non-zero exit is a genuine failure.
	if runErr != nil {
		if code, ok := exitCodeOf(runErr); ok && code == 1 {
			return "", true, nil
		}
		return "", false, fmt.Errorf("current branch: %w", runErr)
	}
	name = strings.TrimSpace(out)
	if name == "" {
		// Empty output with no error also denotes a detached HEAD.
		return "", true, nil
	}
	return name, false, nil
}

// IsDirty reports whether the working tree has uncommitted changes (modified,
// staged, or untracked files). It runs `git status --porcelain`; any non-empty
// output means the tree is dirty (a Dirty_Working_Tree).
func (r *Repo) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.runner.Run(ctx, r.Path, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("status: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// AheadBehind returns how many commits localRef is ahead of and behind
// upstreamRef. It runs `git rev-list --left-right --count <local>...<upstream>`,
// whose output is two integers: the left count (commits reachable from localRef
// but not upstreamRef, i.e. ahead) and the right count (commits reachable from
// upstreamRef but not localRef, i.e. behind).
func (r *Repo) AheadBehind(ctx context.Context, localRef, upstreamRef string) (ahead, behind int, err error) {
	spec := localRef + "..." + upstreamRef
	out, runErr := r.runner.Run(ctx, r.Path, "rev-list", "--left-right", "--count", spec)
	if runErr != nil {
		return 0, 0, fmt.Errorf("ahead/behind %s: %w", spec, runErr)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("ahead/behind %s: unexpected rev-list output %q", spec, out)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("ahead/behind %s: parse ahead count %q: %w", spec, fields[0], err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("ahead/behind %s: parse behind count %q: %w", spec, fields[1], err)
	}
	return ahead, behind, nil
}

// CanFastForward reports whether localRef can be fast-forwarded to upstreamRef,
// i.e. whether localRef is an ancestor of upstreamRef. It runs
// `git merge-base --is-ancestor <local> <upstream>`, which exits 0 when localRef
// is an ancestor (fast-forward possible) and exits 1 when it is not.
//
// The exit-1 "not an ancestor" case is a normal, expected answer and is
// reported as (false, nil); it must not be conflated with a real git failure
// (such as an unknown revision), which produces a higher exit code or a
// non-exit error and is surfaced as (false, err). exitCodeOf is what makes that
// distinction reliable for both the real ExecRunner and the test MockRunner.
func (r *Repo) CanFastForward(ctx context.Context, localRef, upstreamRef string) (bool, error) {
	_, err := r.runner.Run(ctx, r.Path, "merge-base", "--is-ancestor", localRef, upstreamRef)
	if err == nil {
		return true, nil
	}
	if code, ok := exitCodeOf(err); ok && code == 1 {
		// Exit status 1 specifically means "not an ancestor": a valid, non-error
		// answer of "no, a fast-forward is not possible".
		return false, nil
	}
	return false, fmt.Errorf("can fast-forward %s..%s: %w", localRef, upstreamRef, err)
}

// FastForward advances branch to upstreamRef using a fast-forward-only merge,
// never creating a merge commit or rewriting history. It first checks out
// branch so the fast-forward is applied to the intended local branch, then runs
// `git merge --ff-only <upstreamRef>`; the --ff-only flag guarantees git
// refuses (and changes nothing) if the update would not be a fast-forward,
// upholding the "never lose local work" guarantee.
func (r *Repo) FastForward(ctx context.Context, branch, upstreamRef string) error {
	if _, err := r.runner.Run(ctx, r.Path, "checkout", branch); err != nil {
		return fmt.Errorf("checkout %q: %w", branch, err)
	}
	if _, err := r.runner.Run(ctx, r.Path, "merge", "--ff-only", upstreamRef); err != nil {
		return fmt.Errorf("fast-forward %q to %s: %w", branch, upstreamRef, err)
	}
	return nil
}

// FetchWithTags runs `git fetch <remote> --tags` to update remote-tracking refs
// and download all tags from the remote, without touching the working tree or
// local branches. It is used to ensure every upstream tag is present on the
// local copy.
func (r *Repo) FetchWithTags(ctx context.Context, remote string) error {
	if _, err := r.runner.Run(ctx, r.Path, "fetch", remote, "--tags"); err != nil {
		return fmt.Errorf("fetch %q --tags: %w", remote, err)
	}
	return nil
}

// ResetHard discards all uncommitted changes to tracked files by running
// `git reset --hard`, returning the working tree and index to the current
// branch's HEAD. It is a destructive operation: staged and unstaged
// modifications to tracked files are lost. Untracked files are not touched by
// reset; use Clean to remove those as well.
func (r *Repo) ResetHard(ctx context.Context) error {
	if _, err := r.runner.Run(ctx, r.Path, "reset", "--hard"); err != nil {
		return fmt.Errorf("reset --hard: %w", err)
	}
	return nil
}

// ResetHardTo resets the current branch, the index, and the working tree to ref
// by running `git reset --hard <ref>`. It is destructive: any commits on the
// current branch not reachable from ref are dropped from the branch, and local
// modifications are discarded. It is used to force the checked-out default
// branch to match the upstream ref exactly.
func (r *Repo) ResetHardTo(ctx context.Context, ref string) error {
	if _, err := r.runner.Run(ctx, r.Path, "reset", "--hard", ref); err != nil {
		return fmt.Errorf("reset --hard %s: %w", ref, err)
	}
	return nil
}

// Clean removes untracked files and directories by running `git clean -fd`
// (force, including directories). It is destructive: untracked content that is
// not ignored is deleted. Together with ResetHard it returns the working tree to
// a pristine state so a branch switch cannot be blocked by local changes.
func (r *Repo) Clean(ctx context.Context) error {
	if _, err := r.runner.Run(ctx, r.Path, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean -fd: %w", err)
	}
	return nil
}

// Push runs `git push <remote> <branch>` to publish the local branch to the
// given remote (for example pushing an updated default branch to origin).
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	if _, err := r.runner.Run(ctx, r.Path, "push", remote, branch); err != nil {
		return fmt.Errorf("push %s to %q: %w", branch, remote, err)
	}
	return nil
}

// Checkout switches the working tree to an existing branch by running
// `git checkout <branch>`. It is used to move onto the base branch before a
// release run.
func (r *Repo) Checkout(ctx context.Context, branch string) error {
	if _, err := r.runner.Run(ctx, r.Path, "checkout", branch); err != nil {
		return fmt.Errorf("checkout %q: %w", branch, err)
	}
	return nil
}

// CheckoutNewBranch creates branch and switches to it by running
// `git checkout -b <branch>`. It fails if the branch already exists, so callers
// that want to skip an existing branch should consult LocalBranchExists first.
func (r *Repo) CheckoutNewBranch(ctx context.Context, branch string) error {
	if _, err := r.runner.Run(ctx, r.Path, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("create branch %q: %w", branch, err)
	}
	return nil
}

// LocalBranchExists reports whether a local branch named branch exists. It runs
// `git rev-parse --verify --quiet refs/heads/<branch>`, which exits 0 when the
// ref resolves and 1 when it does not; the exit-1 case is reported as
// (false, nil) rather than an error, mirroring CanFastForward's handling of the
// "expected non-zero exit" answer. Any other non-zero exit is surfaced as an
// error.
func (r *Repo) LocalBranchExists(ctx context.Context, branch string) (bool, error) {
	_, err := r.runner.Run(ctx, r.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if code, ok := exitCodeOf(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("checking local branch %q: %w", branch, err)
}

// CommitAll stages every tracked, modified file and records a commit in one step
// by running `git commit -a -m <message>`. It is used to capture the generated
// release artifacts in a single commit; it fails (like git itself) when there is
// nothing staged to commit, so callers should confirm the tree is dirty first.
func (r *Repo) CommitAll(ctx context.Context, message string) error {
	if _, err := r.runner.Run(ctx, r.Path, "commit", "-a", "-m", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
