//go:build integration
// +build integration

// Package syncer_test contains end-to-end integration tests that drive the real
// `git` binary against local repositories created in a temp dir. They exercise
// the Fork_Synchronizer (and the Workspace_Inspector) against genuine git state
// rather than the MockRunner used by the unit tests, validating the
// clone/fetch/fast-forward/dirty/diverged behaviour end-to-end.
//
// These tests are guarded by the `integration` build tag so the default
// `go test ./...` run stays fast and hermetic; run them with
// `go test -tags integration ./...`. GitHub interactions are mocked
// (githubclient.NewMock) because the integration here is for LOCAL git only —
// the syncer never calls GitHub, but the App still carries a client.
//
// All repositories are bare/working clones under t.TempDir(), and every setup
// git command runs with an isolated, deterministic environment (fixed identity,
// global/system config disabled, and an explicit `main` default branch) so the
// tests behave identically regardless of the host's git configuration.
package syncer_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/inspector"
	"github.com/aws-controllers-k8s/ack-workspace/internal/syncer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// defaultBranch is the branch every test repository checks out. It is set
// explicitly on init so the tests do not depend on the host git's configured
// default branch name; the syncer compares and fast-forwards the checked-out
// branch against "upstream/<branch>".
const defaultBranch = "main"

// requireGit skips the test gracefully when no `git` executable is resolvable
// on PATH, so the integration suite is a no-op on machines without git rather
// than a failure.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping local-git integration test")
	}
}

// gitEnv builds a deterministic environment for setup git invocations: a fixed
// author/committer identity (so commits succeed without relying on the host's
// configured user) and disabled global/system config (so host settings such as
// commit signing or a custom default branch cannot influence the test).
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=ack-workspace test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=ack-workspace test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// gitRun runs `git <args...>` in dir with the deterministic test environment
// and returns its trimmed combined output, failing the test on any error. It is
// used only for test SETUP and assertions; the code under test drives git
// through git.NewExecRunner().
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// headSHA returns the commit SHA that HEAD resolves to in the repository at dir.
func headSHA(t *testing.T, dir string) string {
	t.Helper()
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// isDirty reports whether the working tree at dir has uncommitted changes,
// mirroring `git status --porcelain`.
func isDirty(t *testing.T, dir string) bool {
	t.Helper()
	return gitRun(t, dir, "status", "--porcelain") != ""
}

// setupUpstream creates a bare "upstream" repository seeded with one commit on
// the `main` branch and returns the path to the bare repo plus the path to the
// seed working clone (retained so tests can add further commits and push them
// to the bare upstream to simulate upstream advancing).
func setupUpstream(t *testing.T, base string) (upstream, seed string) {
	t.Helper()

	seed = filepath.Join(base, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	// Initialise with an explicit default branch so the test is independent of
	// the host git's init.defaultBranch setting.
	gitRun(t, seed, "init", "-b", defaultBranch)
	writeFile(t, filepath.Join(seed, "README.md"), "initial\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "initial commit")

	upstream = filepath.Join(base, "upstream.git")
	// A bare clone of the seed becomes the canonical upstream; its HEAD points
	// at the seed's checked-out `main` branch.
	gitRun(t, base, "clone", "--bare", seed, upstream)
	return upstream, seed
}

// setupManagedRepo clones the bare upstream into <root>/<name> (a real clone
// that contains a .git directory, so workspace.Discover finds it and the
// checked-out branch is `main`), then adds an `upstream` remote pointing at the
// bare upstream — matching the fork-based contributor layout the syncer expects
// (it fetches and compares against the `upstream` remote). Returns the managed
// repository path.
func setupManagedRepo(t *testing.T, root, name, upstream string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace root: %v", err)
	}
	gitRun(t, root, "clone", upstream, name)
	managed := filepath.Join(root, name)
	gitRun(t, managed, "remote", "add", "upstream", upstream)
	// Populate the upstream/<branch> remote-tracking ref so the read-only
	// inspector has something to compare against; the syncer re-fetches during
	// Sync, so this does not weaken the sync scenarios.
	gitRun(t, managed, "fetch", "upstream")
	return managed
}

// advanceUpstream adds a new commit to the seed clone and pushes it to the bare
// upstream's `main`, simulating upstream moving ahead of the managed clone.
func advanceUpstream(t *testing.T, seed, upstream, filename, content string) {
	t.Helper()
	writeFile(t, filepath.Join(seed, filename), content)
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "upstream: "+filename)
	gitRun(t, seed, "push", upstream, defaultBranch)
}

// newApp builds an App context for the integration tests: real exec-based git
// Runner, a mocked GitHub client (the syncer never calls GitHub, but App
// carries one), and the given workspace root.
func newApp(root string) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    "octocat",
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   4,
		},
		GitHub: githubclient.NewMock(),
		Git:    git.NewExecRunner(),
	}
}

// findResult returns the Result for repo name, failing if absent.
func findResult(t *testing.T, sum workspace.Summary, name string) workspace.Result {
	t.Helper()
	for _, r := range sum.Results {
		if r.Repo == name {
			return r
		}
	}
	t.Fatalf("no result for repo %q in summary %+v", name, sum.Results)
	return workspace.Result{}
}

// TestSyncFastForward verifies the happy path end-to-end: upstream advances, the
// managed clone is clean and fast-forwardable, so sync fetches, fast-forwards
// the local `main`, and records the repository as updated; the local HEAD then
// matches the upstream HEAD (Requirements 5.1, 5.2, 8.1).
func TestSyncFastForward(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "runtime"
	managed := setupManagedRepo(t, root, name, upstream)

	// Upstream moves one commit ahead of the managed clone.
	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n")
	wantHead := gitRun(t, seed, "rev-parse", defaultBranch)

	sum, err := syncer.New().Sync(context.Background(), newApp(root), nil, false)
	if err != nil {
		t.Fatalf("Sync returned pre-flight error: %v", err)
	}

	res := findResult(t, sum, name)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q (reason %q); want %q (updated)", res.Outcome, res.Reason, workspace.OutcomeCreated)
	}
	if res.Reason != "updated" {
		t.Errorf("reason = %q; want %q", res.Reason, "updated")
	}
	if sum.HasFailures() {
		t.Errorf("summary reports failures: %+v", sum.Results)
	}

	if got := headSHA(t, managed); got != wantHead {
		t.Errorf("managed HEAD = %s; want fast-forwarded to upstream %s", got, wantHead)
	}
	if isDirty(t, managed) {
		t.Errorf("managed working tree unexpectedly dirty after fast-forward")
	}
}

// TestSyncDirtySkips verifies that a Dirty_Working_Tree is skipped with the
// "uncommitted changes" reason and that nothing in the managed repository
// changes — neither the commit history nor the dirty working-tree content —
// even though upstream has advanced and a fast-forward would otherwise apply
// (Requirements 5.3, 8.3).
func TestSyncDirtySkips(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "code-generator"
	managed := setupManagedRepo(t, root, name, upstream)

	// Upstream advances so a fast-forward would be possible were the tree clean.
	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n")

	// Make the managed working tree dirty by modifying a tracked file without
	// committing.
	const dirtyContent = "initial\nlocal uncommitted edit\n"
	writeFile(t, filepath.Join(managed, "README.md"), dirtyContent)

	headBefore := headSHA(t, managed)
	if !isDirty(t, managed) {
		t.Fatalf("precondition: managed repo should be dirty")
	}

	sum, err := syncer.New().Sync(context.Background(), newApp(root), nil, false)
	if err != nil {
		t.Fatalf("Sync returned pre-flight error: %v", err)
	}

	res := findResult(t, sum, name)
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %q (reason %q); want %q", res.Outcome, res.Reason, workspace.OutcomeSkipped)
	}
	if res.Reason != "uncommitted changes" {
		t.Errorf("reason = %q; want %q", res.Reason, "uncommitted changes")
	}

	// The commit history must be untouched...
	if got := headSHA(t, managed); got != headBefore {
		t.Errorf("managed HEAD changed to %s; want unchanged %s", got, headBefore)
	}
	// ...and the dirty working-tree content must be preserved verbatim.
	got, err := os.ReadFile(filepath.Join(managed, "README.md"))
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}
	if string(got) != dirtyContent {
		t.Errorf("README content = %q; want preserved %q", string(got), dirtyContent)
	}
	if !isDirty(t, managed) {
		t.Errorf("managed working tree should still be dirty after skip")
	}
}

// TestSyncDivergedSkips verifies that when the local branch has diverged from
// upstream (each has a commit the other lacks) so a fast-forward is impossible,
// sync skips the repository with the "diverged history" reason and leaves the
// local branch byte-for-byte unchanged (Requirements 5.4, 8.1, 8.2).
func TestSyncDivergedSkips(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "test-infra"
	managed := setupManagedRepo(t, root, name, upstream)

	// Upstream gains a commit the managed clone does not have.
	advanceUpstream(t, seed, upstream, "upstream.txt", "from upstream\n")

	// The managed clone gains a *different* local commit, so the histories
	// diverge and no fast-forward is possible.
	writeFile(t, filepath.Join(managed, "local.txt"), "local divergent work\n")
	gitRun(t, managed, "add", ".")
	gitRun(t, managed, "commit", "-m", "local divergent commit")
	headBefore := headSHA(t, managed)

	sum, err := syncer.New().Sync(context.Background(), newApp(root), nil, false)
	if err != nil {
		t.Fatalf("Sync returned pre-flight error: %v", err)
	}

	res := findResult(t, sum, name)
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %q (reason %q); want %q", res.Outcome, res.Reason, workspace.OutcomeSkipped)
	}
	if res.Reason != "diverged history" {
		t.Errorf("reason = %q; want %q", res.Reason, "diverged history")
	}

	if got := headSHA(t, managed); got != headBefore {
		t.Errorf("managed HEAD changed to %s; want unchanged %s (local commits preserved)", got, headBefore)
	}
	if isDirty(t, managed) {
		t.Errorf("managed working tree unexpectedly dirty after diverged skip")
	}
}

// TestSyncMultipleReposIsolation verifies that a batch sync processes every
// repository and that one repository's skip condition does not affect another:
// a clean+behind repo is updated while a dirty repo in the same workspace is
// skipped, each ending in exactly one outcome (Requirements 5.1, 5.2, 5.3, 8.3).
func TestSyncMultipleReposIsolation(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)
	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n")
	wantHead := gitRun(t, seed, "rev-parse", defaultBranch)

	root := filepath.Join(base, "workspace")
	clean := setupManagedRepo(t, root, "runtime", upstream)
	dirty := setupManagedRepo(t, root, "code-generator", upstream)

	// Dirty one repo only.
	writeFile(t, filepath.Join(dirty, "README.md"), "initial\nlocal edit\n")
	dirtyHeadBefore := headSHA(t, dirty)

	sum, err := syncer.New().Sync(context.Background(), newApp(root), nil, false)
	if err != nil {
		t.Fatalf("Sync returned pre-flight error: %v", err)
	}

	if len(sum.Results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(sum.Results), sum.Results)
	}

	cleanRes := findResult(t, sum, "runtime")
	if cleanRes.Outcome != workspace.OutcomeCreated {
		t.Errorf("clean repo outcome = %q (reason %q); want updated", cleanRes.Outcome, cleanRes.Reason)
	}
	if got := headSHA(t, clean); got != wantHead {
		t.Errorf("clean repo HEAD = %s; want fast-forwarded %s", got, wantHead)
	}

	dirtyRes := findResult(t, sum, "code-generator")
	if dirtyRes.Outcome != workspace.OutcomeSkipped || dirtyRes.Reason != "uncommitted changes" {
		t.Errorf("dirty repo result = %q/%q; want skipped/uncommitted changes", dirtyRes.Outcome, dirtyRes.Reason)
	}
	if got := headSHA(t, dirty); got != dirtyHeadBefore {
		t.Errorf("dirty repo HEAD changed to %s; want unchanged %s", got, dirtyHeadBefore)
	}
}

// TestInspectorReportsState exercises the Workspace_Inspector against the same
// kind of real local repositories, asserting the JSON status reflects the
// branch, ahead/behind comparison, and dirty flag. The inspector is read-only
// and does not fetch, so the test fetches `upstream` into the managed clone
// first to make the comparison observable (Requirements 6.2, 6.4, 6.5).
func TestInspectorReportsState(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "runtime"
	managed := setupManagedRepo(t, root, name, upstream)

	// Up-to-date, clean baseline.
	var buf strings.Builder
	if _, err := inspector.NewWithWriter(&buf).Status(context.Background(), newApp(root), true); err != nil {
		t.Fatalf("Status (baseline) error: %v", err)
	}
	entries := decodeEntries(t, buf.String())
	e := entryFor(t, entries, name)
	if e.Branch != defaultBranch {
		t.Errorf("baseline branch = %q; want %q", e.Branch, defaultBranch)
	}
	if e.Comparison != "up_to_date" {
		t.Errorf("baseline comparison = %q; want up_to_date", e.Comparison)
	}
	if e.Dirty {
		t.Errorf("baseline should be clean")
	}

	// Upstream advances by one commit; fetch it into the managed clone (the
	// inspector does not fetch) and dirty the working tree.
	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n")
	gitRun(t, managed, "fetch", "upstream")
	writeFile(t, filepath.Join(managed, "README.md"), "initial\nlocal edit\n")

	buf.Reset()
	if _, err := inspector.NewWithWriter(&buf).Status(context.Background(), newApp(root), true); err != nil {
		t.Fatalf("Status (behind+dirty) error: %v", err)
	}
	entries = decodeEntries(t, buf.String())
	e = entryFor(t, entries, name)
	if e.Comparison != "behind" {
		t.Errorf("comparison = %q; want behind", e.Comparison)
	}
	if e.Behind != 1 {
		t.Errorf("behind = %d; want 1", e.Behind)
	}
	if e.Ahead != 0 {
		t.Errorf("ahead = %d; want 0", e.Ahead)
	}
	if !e.Dirty {
		t.Errorf("expected dirty working tree to be reported")
	}
}

// decodeEntries parses the inspector's JSON document into StatusEntry values.
func decodeEntries(t *testing.T, jsonDoc string) []workspace.StatusEntry {
	t.Helper()
	var entries []workspace.StatusEntry
	if err := json.Unmarshal([]byte(jsonDoc), &entries); err != nil {
		t.Fatalf("unmarshalling status JSON %q: %v", jsonDoc, err)
	}
	return entries
}

// entryFor returns the StatusEntry for repo name, failing if absent.
func entryFor(t *testing.T, entries []workspace.StatusEntry, name string) workspace.StatusEntry {
	t.Helper()
	for _, e := range entries {
		if e.Repo == name {
			return e
		}
	}
	t.Fatalf("no status entry for repo %q in %+v", name, entries)
	return workspace.StatusEntry{}
}
