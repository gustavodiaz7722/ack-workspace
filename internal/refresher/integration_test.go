//go:build integration
// +build integration

// Package refresher_test contains end-to-end integration tests that drive the
// real `git` binary against local repositories created in a temp dir. They
// exercise the Workspace_Refresher (and the Workspace_Inspector) against genuine
// git state rather than the MockRunner used by the unit tests.
//
// These tests are guarded by the `integration` build tag so the default
// `go test ./...` run stays fast and hermetic; run them with
// `go test -tags integration ./...`. The GitHub merge-upstream call is mocked
// (githubclient.NewMock) because there is no real GitHub here — the integration
// validates the LOCAL git reconcile: tags fetched, working tree reset, main
// checked out, and local main reset to exactly match upstream.
package refresher_test

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
	"github.com/aws-controllers-k8s/ack-workspace/internal/refresher"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const defaultBranch = "main"

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping local-git integration test")
	}
}

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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	return gitRun(t, dir, "rev-parse", "HEAD")
}

func isDirty(t *testing.T, dir string) bool {
	t.Helper()
	return gitRun(t, dir, "status", "--porcelain") != ""
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return gitRun(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// setupUpstream creates a bare "upstream" repo seeded with one commit and a tag
// on main, returning the bare repo path and the seed working clone.
func setupUpstream(t *testing.T, base string) (upstream, seed string) {
	t.Helper()
	seed = filepath.Join(base, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	gitRun(t, seed, "init", "-b", defaultBranch)
	writeFile(t, filepath.Join(seed, "README.md"), "initial\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "initial commit")
	gitRun(t, seed, "tag", "v0.0.1")

	upstream = filepath.Join(base, "upstream.git")
	gitRun(t, base, "clone", "--bare", seed, upstream)
	return upstream, seed
}

// setupManagedRepo clones the bare upstream into <root>/<name> and adds the
// upstream remote, matching the fork-based contributor layout the refresher
// expects.
func setupManagedRepo(t *testing.T, root, name, upstream string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace root: %v", err)
	}
	gitRun(t, root, "clone", upstream, name)
	managed := filepath.Join(root, name)
	gitRun(t, managed, "remote", "add", "upstream", upstream)
	gitRun(t, managed, "fetch", "upstream")
	return managed
}

// advanceUpstream adds a commit and a tag to the seed and pushes both to the
// bare upstream, simulating upstream moving ahead with a new release tag.
func advanceUpstream(t *testing.T, seed, upstream, filename, content, tag string) {
	t.Helper()
	writeFile(t, filepath.Join(seed, filename), content)
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "upstream: "+filename)
	if tag != "" {
		gitRun(t, seed, "tag", tag)
	}
	gitRun(t, seed, "push", upstream, defaultBranch)
	gitRun(t, seed, "push", upstream, "--tags")
}

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

// TestRefreshReconcilesToBaseline verifies the full desired end state on a repo
// that is on a feature branch with uncommitted changes while upstream has
// advanced and published a new tag: after refresh, main is checked out, the
// working tree is clean, local main matches the advanced upstream HEAD, and the
// new upstream tag is present locally.
func TestRefreshReconcilesToBaseline(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "autoscaling-controller"
	managed := setupManagedRepo(t, root, name, upstream)

	// Upstream advances by one commit and gains a new tag.
	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n", "v0.0.2")
	wantHead := gitRun(t, seed, "rev-parse", defaultBranch)

	// Developer is on a feature branch with uncommitted changes and an untracked
	// file — exactly the "blocking changes" the refresh must discard.
	gitRun(t, managed, "checkout", "-b", "featureA")
	writeFile(t, filepath.Join(managed, "README.md"), "initial\nlocal uncommitted edit\n")
	writeFile(t, filepath.Join(managed, "scratch.txt"), "untracked\n")
	if !isDirty(t, managed) {
		t.Fatalf("precondition: managed repo should be dirty")
	}

	sum, err := refresher.New().Refresh(context.Background(), newApp(root), nil)
	if err != nil {
		t.Fatalf("Refresh returned pre-flight error: %v", err)
	}

	res := findResult(t, sum, name)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q (reason %q); want created", res.Outcome, res.Reason)
	}

	// 1. main is checked out.
	if got := currentBranch(t, managed); got != defaultBranch {
		t.Errorf("current branch = %q; want %q", got, defaultBranch)
	}
	// 3. local main matches the advanced upstream HEAD.
	if got := headSHA(t, managed); got != wantHead {
		t.Errorf("managed HEAD = %s; want upstream %s", got, wantHead)
	}
	// Working tree is clean (blocking changes discarded).
	if isDirty(t, managed) {
		t.Errorf("managed working tree should be clean after refresh")
	}
	// 4. all upstream tags present locally, including the newly published one.
	tags := gitRun(t, managed, "tag", "--list")
	for _, want := range []string{"v0.0.1", "v0.0.2"} {
		if !strings.Contains(tags, want) {
			t.Errorf("expected tag %q present locally; tags=%q", want, tags)
		}
	}
}

// TestRefreshResetsDivergedMain verifies that a local main which has diverged
// from upstream (a local-only commit) is force-reset to match upstream.
func TestRefreshResetsDivergedMain(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "runtime"
	managed := setupManagedRepo(t, root, name, upstream)

	advanceUpstream(t, seed, upstream, "upstream.txt", "from upstream\n", "")
	wantHead := gitRun(t, seed, "rev-parse", defaultBranch)

	// Local main gains a different commit, diverging from upstream.
	writeFile(t, filepath.Join(managed, "local.txt"), "local divergent work\n")
	gitRun(t, managed, "add", ".")
	gitRun(t, managed, "commit", "-m", "local divergent commit")

	if _, err := refresher.New().Refresh(context.Background(), newApp(root), nil); err != nil {
		t.Fatalf("Refresh returned pre-flight error: %v", err)
	}

	if got := headSHA(t, managed); got != wantHead {
		t.Errorf("managed HEAD = %s; want reset to upstream %s", got, wantHead)
	}
}

// TestInspectorReportsState exercises the Workspace_Inspector against the same
// kind of real local repositories.
func TestInspectorReportsState(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	upstream, seed := setupUpstream(t, base)

	root := filepath.Join(base, "workspace")
	const name = "runtime"
	managed := setupManagedRepo(t, root, name, upstream)

	var buf strings.Builder
	if _, err := inspector.NewWithWriter(&buf).Status(context.Background(), newApp(root), true); err != nil {
		t.Fatalf("Status (baseline) error: %v", err)
	}
	e := entryFor(t, decodeEntries(t, buf.String()), name)
	if e.Branch != defaultBranch {
		t.Errorf("baseline branch = %q; want %q", e.Branch, defaultBranch)
	}
	if e.Comparison != "up_to_date" {
		t.Errorf("baseline comparison = %q; want up_to_date", e.Comparison)
	}
	if e.Dirty {
		t.Errorf("baseline should be clean")
	}

	advanceUpstream(t, seed, upstream, "feature.txt", "feature\n", "")
	gitRun(t, managed, "fetch", "upstream")
	writeFile(t, filepath.Join(managed, "README.md"), "initial\nlocal edit\n")

	buf.Reset()
	if _, err := inspector.NewWithWriter(&buf).Status(context.Background(), newApp(root), true); err != nil {
		t.Fatalf("Status (behind+dirty) error: %v", err)
	}
	e = entryFor(t, decodeEntries(t, buf.String()), name)
	if e.Comparison != "behind" {
		t.Errorf("comparison = %q; want behind", e.Comparison)
	}
	if e.Behind != 1 {
		t.Errorf("behind = %d; want 1", e.Behind)
	}
	if !e.Dirty {
		t.Errorf("expected dirty working tree to be reported")
	}
}

func decodeEntries(t *testing.T, jsonDoc string) []workspace.StatusEntry {
	t.Helper()
	var entries []workspace.StatusEntry
	if err := json.Unmarshal([]byte(jsonDoc), &entries); err != nil {
		t.Fatalf("unmarshalling status JSON %q: %v", jsonDoc, err)
	}
	return entries
}

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
