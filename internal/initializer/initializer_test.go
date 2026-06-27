package initializer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	testUser   = "octocat"
	testPrefix = "ack-"
)

// newApp builds an App context for tests with the given workspace root, GitHub
// mock, and git runner.
func newApp(root string, gh githubclient.GitHubClient, runner git.Runner) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    testUser,
			WorkspaceRoot: root,
			RepoPrefix:    testPrefix,
			Concurrency:   1,
		},
		GitHub: gh,
		Git:    runner,
	}
}

// resultsByRepo indexes a Summary's Results by upstream repo name.
func resultsByRepo(s workspace.Summary) map[string]workspace.Result {
	out := make(map[string]workspace.Result, len(s.Results))
	for _, r := range s.Results {
		out[r.Repo] = r
	}
	return out
}

// markForksPresent registers all three Common_Repository forks as already
// existing under the test user, so the flow does not need to create them.
func markForksPresent(m *githubclient.Mock) {
	for _, name := range CommonRepositories {
		m.SetRepo(githubclient.RepoRef{Owner: testUser, Name: testPrefix + name}, githubclient.RepoState{Exists: true})
	}
}

func TestInit_AllCreated(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markForksPresent(gh)
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	if got := len(sum.Results); got != len(CommonRepositories) {
		t.Fatalf("expected %d results, got %d", len(CommonRepositories), got)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 3 {
		t.Fatalf("expected 3 created, got %d (results=%+v)", got, sum.Results)
	}
	if sum.HasFailures() {
		t.Fatalf("did not expect failures: %+v", sum.Results)
	}
	// Forks already existed, so no fork should have been created.
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls, got %d", n)
	}

	// Each repo should have been cloned and had origin+upstream configured.
	byRepo := resultsByRepo(sum)
	for _, name := range CommonRepositories {
		if byRepo[name].Outcome != workspace.OutcomeCreated {
			t.Errorf("repo %s: expected created, got %s", name, byRepo[name].Outcome)
		}
	}
	assertGitCommandIssued(t, runner, "clone")
	assertRemoteConfigured(t, runner, "origin")
	assertRemoteConfigured(t, runner, "upstream")
}

func TestInit_SkipExistingDirectory(t *testing.T) {
	root := t.TempDir()
	// Pre-create the runtime directory: it must be skipped regardless of
	// contents, and never deleted.
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	gh := githubclient.NewMock()
	markForksPresent(gh)
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if byRepo["runtime"].Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected runtime skipped, got %s", byRepo["runtime"].Outcome)
	}
	// The pre-existing directory must still be present (never deleted).
	if !dirExists(runtimeDir) {
		t.Fatalf("pre-existing directory %q was removed", runtimeDir)
	}
	// No GitHub work should have been done for the skipped repo.
	for _, c := range gh.Calls {
		if c.Ref.Name == "runtime" || c.Ref.Name == testPrefix+"runtime" {
			t.Fatalf("unexpected GitHub call for skipped repo: %+v", c)
		}
	}
	// The other two repos should have been created.
	if got := sum.Count(workspace.OutcomeCreated); got != 2 {
		t.Fatalf("expected 2 created, got %d", got)
	}
	if got := sum.Count(workspace.OutcomeSkipped); got != 1 {
		t.Fatalf("expected 1 skipped, got %d", got)
	}
}

func TestInit_ForkMissingThenCreated(t *testing.T) {
	root := t.TempDir()
	// Forks do not exist -> they must be created. ForkAppears defaults to true.
	gh := githubclient.NewMock()
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	if n := gh.CallCount("CreateFork"); n != len(CommonRepositories) {
		t.Fatalf("expected %d CreateFork calls, got %d", len(CommonRepositories), n)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 3 {
		t.Fatalf("expected 3 created, got %d (results=%+v)", got, sum.Results)
	}
	// CreateFork should target the upstream repos with the prefixed fork name.
	for _, c := range gh.CallsFor("CreateFork") {
		if c.Ref.Owner != upstreamOwner {
			t.Errorf("CreateFork upstream owner = %q, want %q", c.Ref.Owner, upstreamOwner)
		}
		if c.ForkName != testPrefix+c.Ref.Name {
			t.Errorf("CreateFork fork name = %q, want %q", c.ForkName, testPrefix+c.Ref.Name)
		}
	}
}

func TestInit_ForkCreateFailure(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	gh.CreateForkErr = errors.New("boom: fork quota exceeded")
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	// All three repos fail (fork creation fails) but every repo is still
	// processed (failure isolation).
	if got := len(sum.Results); got != 3 {
		t.Fatalf("expected 3 results, got %d", got)
	}
	if got := sum.Count(workspace.OutcomeFailed); got != 3 {
		t.Fatalf("expected 3 failed, got %d (results=%+v)", got, sum.Results)
	}
	// No clone should have been attempted when the fork could not be created.
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "clone" {
			t.Fatalf("unexpected clone after fork-create failure: %+v", c)
		}
	}
}

func TestInit_CloneFailureCleansUp(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markForksPresent(gh)

	// Simulate a partial clone: the clone command creates the destination
	// directory and then fails. Cleanup must remove that run-created directory.
	runner := &git.MockRunner{}
	runner.ResponseFunc = func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			_ = os.MkdirAll(dest, 0o755)
			return "fatal: clone exploded", errors.New("clone failed")
		}
		return "", nil
	}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeFailed); got != 3 {
		t.Fatalf("expected 3 failed, got %d (results=%+v)", got, sum.Results)
	}
	// Every run-created directory must have been cleaned up.
	for _, name := range CommonRepositories {
		dir := filepath.Join(root, name)
		if dirExists(dir) {
			t.Errorf("expected %q to be cleaned up after clone failure", dir)
		}
	}
}

func TestInit_RemoteConfigFailureCleansUp(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markForksPresent(gh)

	// Clone succeeds (creating the directory) but configuring a remote fails.
	// Both `remote set-url` and the `remote add` fallback fail, so SetRemote
	// returns an error and the cloned directory must be removed.
	runner := &git.MockRunner{}
	runner.ResponseFunc = func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "clone":
			dest := args[len(args)-1]
			_ = os.MkdirAll(dest, 0o755)
			return "", nil
		case "remote":
			return "fatal: remote exploded", errors.New("remote config failed")
		}
		return "", nil
	}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeFailed); got != 3 {
		t.Fatalf("expected 3 failed, got %d (results=%+v)", got, sum.Results)
	}
	for _, name := range CommonRepositories {
		dir := filepath.Join(root, name)
		if dirExists(dir) {
			t.Errorf("expected cloned dir %q to be cleaned up after remote-config failure", dir)
		}
	}
}

func TestInit_WorkspaceRootCreationFailureAborts(t *testing.T) {
	// Use a regular file as the parent so MkdirAll of a child path fails.
	tmp := t.TempDir()
	fileAsParent := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(fileAsParent, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	root := filepath.Join(fileAsParent, "workspace")

	gh := githubclient.NewMock()
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), newApp(root, gh, runner))
	if err == nil {
		t.Fatalf("expected a pre-flight error creating workspace root, got nil")
	}
	if len(sum.Results) != 0 {
		t.Fatalf("expected empty summary on pre-flight abort, got %+v", sum.Results)
	}
	// No repository should have been processed.
	if len(gh.Calls) != 0 {
		t.Fatalf("expected no GitHub calls on pre-flight abort, got %+v", gh.Calls)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no git calls on pre-flight abort, got %+v", runner.Calls)
	}
}

// assertGitCommandIssued fails the test if no recorded git call begins with the
// given subcommand.
func assertGitCommandIssued(t *testing.T, runner *git.MockRunner, sub string) {
	t.Helper()
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == sub {
			return
		}
	}
	t.Errorf("expected a git %q command, but none was issued", sub)
}

// assertRemoteConfigured fails the test if no recorded git call configures the
// named remote via `remote set-url <name>` or `remote add <name>`.
func assertRemoteConfigured(t *testing.T, runner *git.MockRunner, name string) {
	t.Helper()
	for _, c := range runner.Calls {
		if len(c.Args) >= 3 && c.Args[0] == "remote" &&
			(c.Args[1] == "set-url" || c.Args[1] == "add") && c.Args[2] == name {
			return
		}
	}
	t.Errorf("expected remote %q to be configured, but it was not", name)
}

// dryRunApp builds an App context with DryRun enabled.
func dryRunApp(root string, gh githubclient.GitHubClient, runner git.Runner) app.App {
	a := newApp(root, gh, runner)
	a.DryRun = true
	return a
}

// assertNoMutatingGitCalls fails the test if the runner recorded any mutating
// git subcommand (clone, remote, fetch, checkout, merge, push).
func assertNoMutatingGitCalls(t *testing.T, runner *git.MockRunner) {
	t.Helper()
	mutating := map[string]bool{
		"clone": true, "remote": true, "fetch": true,
		"checkout": true, "merge": true, "push": true,
	}
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && mutating[c.Args[0]] {
			t.Fatalf("dry-run must not issue mutating git command, but got: %v", c.Args)
		}
	}
}

// TestInit_DryRunNoMutationsForkPresent verifies Requirements 8.4/8.5 and
// Property 6: with forks already present, dry-run computes a per-repo preview
// (OutcomeCreated with a "would ..." reason) using only read-only operations,
// invoking no CreateFork and no mutating git command.
func TestInit_DryRunNoMutationsForkPresent(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markForksPresent(gh)
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), dryRunApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	// Preview output is still produced: a result per Common_Repository.
	if got := len(sum.Results); got != len(CommonRepositories) {
		t.Fatalf("expected %d results, got %d", len(CommonRepositories), got)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != len(CommonRepositories) {
		t.Fatalf("expected %d would-create previews, got %d (%+v)", len(CommonRepositories), got, sum.Results)
	}
	if sum.HasFailures() {
		t.Fatalf("did not expect failures in dry-run: %+v", sum.Results)
	}
	for _, r := range sum.Results {
		if r.Reason != "would clone existing fork" {
			t.Errorf("repo %s: expected preview reason %q, got %q", r.Repo, "would clone existing fork", r.Reason)
		}
	}

	// No mutating operations whatsoever.
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestInit_DryRunForkMissingPreview verifies that when a fork is missing,
// dry-run reports "would create fork and clone" without calling CreateFork or
// any mutating git command.
func TestInit_DryRunForkMissingPreview(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock() // forks absent by default
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), dryRunApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeCreated); got != len(CommonRepositories) {
		t.Fatalf("expected %d would-create previews, got %d (%+v)", len(CommonRepositories), got, sum.Results)
	}
	for _, r := range sum.Results {
		if r.Reason != "would create fork and clone" {
			t.Errorf("repo %s: expected reason %q, got %q", r.Repo, "would create fork and clone", r.Reason)
		}
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestInit_DryRunSkipsExistingDirectory verifies the pre-existing directory is
// previewed as skipped and never removed in dry-run.
func TestInit_DryRunSkipsExistingDirectory(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	gh := githubclient.NewMock()
	markForksPresent(gh)
	runner := &git.MockRunner{}

	sum, err := New().Init(context.Background(), dryRunApp(root, gh, runner))
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if byRepo["runtime"].Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected runtime skipped, got %s", byRepo["runtime"].Outcome)
	}
	if !dirExists(runtimeDir) {
		t.Fatalf("pre-existing directory %q was removed in dry-run", runtimeDir)
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}
