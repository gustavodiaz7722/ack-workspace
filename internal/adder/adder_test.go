package adder

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
// mock, and git runner. Concurrency is 1 so per-identifier ordering is
// deterministic for assertions.
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

// markUpstreamPresent registers the given upstream "<alias>-controller" repos as
// existing in the organization.
func markUpstreamPresent(m *githubclient.Mock, names ...string) {
	for _, name := range names {
		m.SetRepo(githubclient.RepoRef{Owner: upstreamOwner, Name: name}, githubclient.RepoState{Exists: true})
	}
}

// markForkPresent registers the fork "<prefix><name>" as existing under the test
// user so the flow does not create it.
func markForkPresent(m *githubclient.Mock, name string) {
	m.SetRepo(githubclient.RepoRef{Owner: testUser, Name: testPrefix + name}, githubclient.RepoState{Exists: true})
}

// TestAdd_EmptyListGuard verifies Requirement 4.2: an empty identifier list is
// rejected before any change, with no filesystem or client side effects.
func TestAdd_EmptyListGuard(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), nil)
	if err == nil {
		t.Fatalf("expected an error for empty identifier list, got nil")
	}
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("expected a *UsageError, got %T: %v", err, err)
	}
	if len(sum.Results) != 0 {
		t.Fatalf("expected empty summary, got %+v", sum.Results)
	}
	// No GitHub or git work, and no modification under the workspace root.
	if len(gh.Calls) != 0 {
		t.Fatalf("expected no GitHub calls, got %+v", gh.Calls)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no git calls, got %+v", runner.Calls)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no modification under workspace root, found %d entries", len(entries))
	}
}

// TestAdd_NormalizationBareAndFullForm verifies Requirement 4.1: both the bare
// alias and the full "<alias>-controller" form resolve to "<alias>-controller"
// and produce the fork name "<prefix><alias>-controller".
func TestAdd_NormalizationBareAndFullForm(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller", "sns-controller")
	runner := &git.MockRunner{}

	// "s3" (bare) and "sns-controller" (full) plus surrounding whitespace.
	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"  s3 ", "sns-controller"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeCreated); got != 2 {
		t.Fatalf("expected 2 added, got %d (results=%+v)", got, sum.Results)
	}
	byRepo := resultsByRepo(sum)
	for _, name := range []string{"s3-controller", "sns-controller"} {
		if byRepo[name].Outcome != workspace.OutcomeCreated {
			t.Errorf("repo %s: expected created, got %s", name, byRepo[name].Outcome)
		}
	}

	// Resolution must target the org with the normalized "<alias>-controller" name.
	resolved := map[string]bool{}
	for _, c := range gh.CallsFor("RepoExists") {
		if c.Ref.Owner == upstreamOwner {
			resolved[c.Ref.Name] = true
		}
	}
	for _, name := range []string{"s3-controller", "sns-controller"} {
		if !resolved[name] {
			t.Errorf("expected upstream resolution for %q, calls=%+v", name, gh.CallsFor("RepoExists"))
		}
	}

	// Forks were missing, so CreateFork must target the upstream repo with the
	// prefixed "<prefix><alias>-controller" fork name.
	forkNames := map[string]bool{}
	for _, c := range gh.CallsFor("CreateFork") {
		if c.Ref.Owner != upstreamOwner {
			t.Errorf("CreateFork upstream owner = %q, want %q", c.Ref.Owner, upstreamOwner)
		}
		if c.ForkName != testPrefix+c.Ref.Name {
			t.Errorf("CreateFork fork name = %q, want %q", c.ForkName, testPrefix+c.Ref.Name)
		}
		forkNames[c.ForkName] = true
	}
	for _, name := range []string{testPrefix + "s3-controller", testPrefix + "sns-controller"} {
		if !forkNames[name] {
			t.Errorf("expected CreateFork with fork name %q, got %+v", name, gh.CallsFor("CreateFork"))
		}
	}
}

// TestAdd_ResolutionFailureContinues verifies Requirement 4.3: a not-found repo
// and an API error both record the identifier as failed while the batch
// continues processing the remaining identifiers.
func TestAdd_ResolutionFailureContinues(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	// "s3-controller" exists; "bad-controller" is not found; "err-controller"
	// returns an API error.
	markUpstreamPresent(gh, "s3-controller")
	gh.SetRepo(githubclient.RepoRef{Owner: upstreamOwner, Name: "err-controller"}, githubclient.RepoState{Err: errors.New("boom: api error")})
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3", "bad", "err"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := len(sum.Results); got != 3 {
		t.Fatalf("expected 3 results, got %d (results=%+v)", got, sum.Results)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 1 {
		t.Fatalf("expected 1 added, got %d (results=%+v)", got, sum.Results)
	}
	if got := sum.Count(workspace.OutcomeFailed); got != 2 {
		t.Fatalf("expected 2 failed, got %d (results=%+v)", got, sum.Results)
	}

	byRepo := resultsByRepo(sum)
	if byRepo["s3-controller"].Outcome != workspace.OutcomeCreated {
		t.Errorf("s3-controller: expected created, got %s", byRepo["s3-controller"].Outcome)
	}
	if byRepo["bad-controller"].Outcome != workspace.OutcomeFailed {
		t.Errorf("bad-controller: expected failed, got %s", byRepo["bad-controller"].Outcome)
	}
	if byRepo["err-controller"].Outcome != workspace.OutcomeFailed {
		t.Errorf("err-controller: expected failed, got %s", byRepo["err-controller"].Outcome)
	}
	// No clone should have been attempted for the failed resolutions.
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "clone" {
			if filepath.Base(c.Args[len(c.Args)-1]) != "s3-controller" {
				t.Errorf("unexpected clone for failed identifier: %+v", c)
			}
		}
	}
}

// TestAdd_ForkCreateFailure verifies Requirement 4.5: a fork-create failure
// records the identifier as failed and the batch continues; no clone is
// attempted for it.
func TestAdd_ForkCreateFailure(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	gh.CreateForkErr = errors.New("boom: fork quota exceeded")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeFailed); got != 1 {
		t.Fatalf("expected 1 failed, got %d (results=%+v)", got, sum.Results)
	}
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "clone" {
			t.Fatalf("unexpected clone after fork-create failure: %+v", c)
		}
	}
}

// TestAdd_SkipExistingDirectory verifies Requirement 4.7: an existing local
// directory is skipped (recorded as already present), the clone is not
// attempted, and the pre-existing directory is never removed.
func TestAdd_SkipExistingDirectory(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "s3-controller")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if byRepo["s3-controller"].Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected s3-controller skipped, got %s", byRepo["s3-controller"].Outcome)
	}
	if !dirExists(existing) {
		t.Fatalf("pre-existing directory %q was removed", existing)
	}
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "clone" {
			t.Fatalf("unexpected clone for skipped repo: %+v", c)
		}
	}
}

// TestAdd_RemoteConfigFailureCleansUp verifies Requirement 4.8: when configuring
// a remote fails after a clone, the run-created directory is removed and the
// identifier is recorded as failed.
func TestAdd_RemoteConfigFailureCleansUp(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")

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

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeFailed); got != 1 {
		t.Fatalf("expected 1 failed, got %d (results=%+v)", got, sum.Results)
	}
	dir := filepath.Join(root, "s3-controller")
	if dirExists(dir) {
		t.Fatalf("expected cloned dir %q to be cleaned up after remote-config failure", dir)
	}
}

// TestAdd_SuccessConfiguresRemotes verifies Requirement 4.6: a successful add
// clones the fork and configures both origin and upstream remotes.
func TestAdd_SuccessConfiguresRemotes(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 1 {
		t.Fatalf("expected 1 added, got %d (results=%+v)", got, sum.Results)
	}
	if gh.CallCount("CreateFork") != 0 {
		t.Fatalf("expected no CreateFork when fork present, got %d", gh.CallCount("CreateFork"))
	}
	assertGitCommandIssued(t, runner, "clone")
	assertRemoteConfigured(t, runner, "origin")
	assertRemoteConfigured(t, runner, "upstream")
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

// TestAdd_DryRunForkPresentPreview verifies Requirements 8.4/8.5 and Property 6:
// with the fork present, dry-run previews "would clone existing fork" using only
// read-only operations and invokes no CreateFork and no mutating git command.
func TestAdd_DryRunForkPresentPreview(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), dryRunApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeCreated); got != 1 {
		t.Fatalf("expected 1 would-create preview, got %d (%+v)", got, sum.Results)
	}
	if r := resultsByRepo(sum)["s3-controller"]; r.Reason != "would clone existing fork" {
		t.Errorf("expected reason %q, got %q", "would clone existing fork", r.Reason)
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestAdd_DryRunForkMissingPreview verifies that when the fork is missing,
// dry-run previews "would create fork and clone" without calling CreateFork or
// any mutating git command.
func TestAdd_DryRunForkMissingPreview(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller") // fork absent
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), dryRunApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeCreated); got != 1 {
		t.Fatalf("expected 1 would-create preview, got %d (%+v)", got, sum.Results)
	}
	if r := resultsByRepo(sum)["s3-controller"]; r.Reason != "would create fork and clone" {
		t.Errorf("expected reason %q, got %q", "would create fork and clone", r.Reason)
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestAdd_DryRunSkipExistingDirectory verifies a pre-existing directory is
// previewed as skipped and never removed in dry-run.
func TestAdd_DryRunSkipExistingDirectory(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "s3-controller")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	gh := githubclient.NewMock()
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), dryRunApp(root, gh, runner), []string{"s3"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if r := resultsByRepo(sum)["s3-controller"]; r.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected s3-controller skipped, got %s", r.Outcome)
	}
	if !dirExists(existing) {
		t.Fatalf("pre-existing directory %q was removed in dry-run", existing)
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestAdd_DryRunResolutionFailureStillReported verifies that read-only
// resolution failures are still reported (as failed) in dry-run while no
// mutation occurs.
func TestAdd_DryRunResolutionFailureStillReported(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock() // "bad-controller" not found
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), dryRunApp(root, gh, runner), []string{"bad"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	if got := sum.Count(workspace.OutcomeFailed); got != 1 {
		t.Fatalf("expected 1 failed, got %d (%+v)", got, sum.Results)
	}
	if n := gh.CallCount("CreateFork"); n != 0 {
		t.Fatalf("expected 0 CreateFork calls in dry-run, got %d", n)
	}
	assertNoMutatingGitCalls(t, runner)
}

// TestAdd_AllExpandsToOrgControllers verifies that the special "all" identifier
// expands to every "<alias>-controller" repository discovered in the
// organization, ignoring non-controller repositories, and that each discovered
// controller is set up.
func TestAdd_AllExpandsToOrgControllers(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	// The org contains two controllers plus several non-controller repos that
	// must be filtered out by the "<alias>-controller" naming convention.
	gh.SetOrgRepos(upstreamOwner,
		"runtime", "code-generator", "test-infra", "community",
		"s3-controller", "sns-controller")
	// The discovered controllers must resolve as existing upstream repos.
	markUpstreamPresent(gh, "s3-controller", "sns-controller")
	markForkPresent(gh, "s3-controller")
	markForkPresent(gh, "sns-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"all"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	// ListOrgRepos must have been consulted exactly once.
	if got := gh.CallCount("ListOrgRepos"); got != 1 {
		t.Errorf("expected ListOrgRepos to be called once, got %d", got)
	}

	// Only the two controllers should have been processed (created).
	if got := len(sum.Results); got != 2 {
		t.Fatalf("expected 2 results, got %d (results=%+v)", got, sum.Results)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 2 {
		t.Fatalf("expected 2 added, got %d (results=%+v)", got, sum.Results)
	}
	byRepo := resultsByRepo(sum)
	for _, name := range []string{"s3-controller", "sns-controller"} {
		if byRepo[name].Outcome != workspace.OutcomeCreated {
			t.Errorf("repo %s: expected created, got %s", name, byRepo[name].Outcome)
		}
	}
	// Non-controller repositories must not have been processed.
	for _, name := range []string{"runtime", "code-generator", "test-infra", "community"} {
		if _, ok := byRepo[name]; ok {
			t.Errorf("non-controller repo %q should not have been processed", name)
		}
	}
}

// TestAdd_AllSupersedesOtherIdentifiers verifies that when "all" appears
// alongside other identifiers it supersedes them: only the discovered
// controllers are processed.
func TestAdd_AllSupersedesOtherIdentifiers(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	gh.SetOrgRepos(upstreamOwner, "sns-controller")
	markUpstreamPresent(gh, "sns-controller")
	markForkPresent(gh, "sns-controller")
	runner := &git.MockRunner{}

	// "s3" is supplied alongside "all"; it must be ignored in favor of the
	// discovered controller set.
	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"s3", "all"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if _, ok := byRepo["s3-controller"]; ok {
		t.Errorf("s3-controller should not have been processed when 'all' is given")
	}
	if byRepo["sns-controller"].Outcome != workspace.OutcomeCreated {
		t.Errorf("expected sns-controller created, got %+v", byRepo["sns-controller"])
	}
	if len(sum.Results) != 1 {
		t.Errorf("expected exactly 1 result, got %d (%+v)", len(sum.Results), sum.Results)
	}
}

// TestAdd_AllListFailureAborts verifies that a failure to list the organization
// repositories aborts the command with the error surfaced and makes no change
// under the workspace root.
func TestAdd_AllListFailureAborts(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	gh.ListOrgReposErr = errors.New("boom: api error")
	runner := &git.MockRunner{}

	_, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"all"})
	if err == nil {
		t.Fatal("expected an error when listing org repos fails, got nil")
	}
	// A listing failure is a runtime error, not a *UsageError.
	var ue *UsageError
	if errors.As(err, &ue) {
		t.Errorf("listing failure should not be a *UsageError, got %v", err)
	}
	// No git work and nothing created under the root.
	if len(runner.Calls) != 0 {
		t.Errorf("expected no git calls on listing failure, got %+v", runner.Calls)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("expected no modification under workspace root, found %d entries", len(entries))
	}
}

// TestAdd_AllNoControllersFound verifies that when the organization contains no
// controller repositories, "all" yields a *UsageError and no work is done.
func TestAdd_AllNoControllersFound(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	// Only non-controller repos are present.
	gh.SetOrgRepos(upstreamOwner, "runtime", "code-generator", "community")
	runner := &git.MockRunner{}

	_, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"all"})
	if err == nil {
		t.Fatal("expected a usage error when no controllers are found, got nil")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UsageError, got %T: %v", err, err)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("expected no git calls, got %+v", runner.Calls)
	}
}

// TestAdd_AllCaseInsensitive verifies the "all" token is matched ignoring case
// and surrounding whitespace.
func TestAdd_AllCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	gh.SetOrgRepos(upstreamOwner, "s3-controller")
	markUpstreamPresent(gh, "s3-controller")
	markForkPresent(gh, "s3-controller")
	runner := &git.MockRunner{}

	sum, err := New().Add(context.Background(), newApp(root, gh, runner), []string{"  ALL "})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}
	if gh.CallCount("ListOrgRepos") != 1 {
		t.Errorf("expected 'ALL' to trigger org listing")
	}
	if sum.Count(workspace.OutcomeCreated) != 1 {
		t.Errorf("expected 1 added, got %+v", sum.Results)
	}
}

// TestAdd_AllDryRunMakesNoChange verifies that "all" combined with dry-run
// discovers controllers (read-only) and previews them without creating forks or
// invoking git.
func TestAdd_AllDryRun(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock()
	gh.SetOrgRepos(upstreamOwner, "s3-controller", "sns-controller")
	markUpstreamPresent(gh, "s3-controller", "sns-controller")
	runner := &git.MockRunner{}

	ap := newApp(root, gh, runner)
	ap.DryRun = true

	sum, err := New().Add(context.Background(), ap, []string{"all"})
	if err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 2 {
		t.Fatalf("expected 2 previewed, got %d (%+v)", got, sum.Results)
	}
	// Dry-run must not create forks or run git.
	if gh.CallCount("CreateFork") != 0 {
		t.Errorf("dry-run must not create forks, got %d", gh.CallCount("CreateFork"))
	}
	if len(runner.Calls) != 0 {
		t.Errorf("dry-run must not invoke git, got %+v", runner.Calls)
	}
}
