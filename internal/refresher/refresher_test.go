package refresher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// newApp builds an App context for tests backed by a default GitHub mock whose
// SyncFork succeeds. Concurrency is 1 so the shared MockRunner is driven
// serially and its recorded Calls slice is safe to assert against.
func newApp(root string, runner git.Runner) app.App {
	return newAppWithGitHub(root, runner, githubclient.NewMock())
}

// newAppWithGitHub is like newApp but uses the supplied GitHub mock so a test
// can script the server-side fork sync (for example a *ForkDivergedError).
func newAppWithGitHub(root string, runner git.Runner, gh githubclient.GitHubClient) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    "octocat",
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   1,
		},
		GitHub: gh,
		Git:    runner,
	}
}

func makeRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("setup repo %q: %v", name, err)
	}
	return dir
}

// scriptOptions configures the scripted git behavior for a repository keyed by
// its working directory.
type scriptOptions struct {
	fetchErr    error
	resetErr    error // first `reset` (reset --hard, no ref) error
	cleanErr    error
	checkoutErr error
	resetToErr  error // second `reset` (reset --hard <ref>) error
}

// scriptedRunner returns a MockRunner whose ResponseFunc scripts git output per
// repository directory. The two `reset` invocations are distinguished by arg
// count: `reset --hard` (2 args) vs `reset --hard <ref>` (3 args).
func scriptedRunner(opts map[string]scriptOptions) *git.MockRunner {
	r := &git.MockRunner{}
	r.ResponseFunc = func(dir string, args []string) (string, error) {
		o := opts[dir]
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "fetch":
			return "", o.fetchErr
		case "reset":
			if len(args) >= 3 { // reset --hard <ref>
				return "", o.resetToErr
			}
			return "", o.resetErr
		case "clean":
			return "", o.cleanErr
		case "checkout":
			return "", o.checkoutErr
		default:
			return "", nil
		}
	}
	return r
}

func resultsByRepo(s workspace.Summary) map[string]workspace.Result {
	out := make(map[string]workspace.Result, len(s.Results))
	for _, r := range s.Results {
		out[r.Repo] = r
	}
	return out
}

func callsInDir(runner *git.MockRunner, dir string) [][]string {
	var out [][]string
	for _, c := range runner.Calls {
		if c.Dir == dir {
			out = append(out, c.Args)
		}
	}
	return out
}

func assertNoSubcommand(t *testing.T, runner *git.MockRunner, dir, sub string) {
	t.Helper()
	for _, args := range callsInDir(runner, dir) {
		if len(args) > 0 && args[0] == sub {
			t.Fatalf("expected no git %q in %s, but it was issued: %v", sub, dir, args)
		}
	}
}

func findCall(runner *git.MockRunner, dir string, prefix ...string) bool {
	for _, args := range callsInDir(runner, dir) {
		if len(args) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if args[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestRefresh_SuccessfulReconcile(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "autoscaling-controller")
	gh := githubclient.NewMock()
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	sum, err := New().Refresh(context.Background(), newAppWithGitHub(root, runner, gh), nil)
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["autoscaling-controller"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected created, got %s (%+v)", res.Outcome, res)
	}

	// The fork was synced server-side against the prefixed fork name.
	syncCalls := gh.CallsFor("SyncFork")
	if len(syncCalls) != 1 {
		t.Fatalf("expected 1 SyncFork call, got %d", len(syncCalls))
	}
	if syncCalls[0].Ref.Name != "ack-autoscaling-controller" || syncCalls[0].Branch != "main" {
		t.Errorf("SyncFork ref/branch = %s/%s, want ack-autoscaling-controller/main", syncCalls[0].Ref, syncCalls[0].Branch)
	}
	// Tags are fetched, the tree is reset and cleaned, main checked out, and the
	// local branch reset to the upstream ref.
	if !findCall(runner, dir, "fetch", "upstream", "--tags") {
		t.Errorf("expected fetch upstream --tags, calls=%v", callsInDir(runner, dir))
	}
	if !findCall(runner, dir, "reset", "--hard", "upstream/main") {
		t.Errorf("expected reset --hard upstream/main, calls=%v", callsInDir(runner, dir))
	}
	if !findCall(runner, dir, "clean", "-fd") {
		t.Errorf("expected clean -fd, calls=%v", callsInDir(runner, dir))
	}
	if !findCall(runner, dir, "checkout", "main") {
		t.Errorf("expected checkout main, calls=%v", callsInDir(runner, dir))
	}
}

func TestRefresh_ForkDivergedFailsBeforeLocalChanges(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	gh := githubclient.NewMock()
	gh.SyncForkErr = &githubclient.ForkDivergedError{
		Fork:   githubclient.RepoRef{Owner: "octocat", Name: "ack-alpha"},
		Branch: "main",
	}
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	sum, err := New().Refresh(context.Background(), newAppWithGitHub(root, runner, gh), nil)
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("expected failed, got %s (%+v)", res.Outcome, res)
	}
	// No local mutation must occur when the fork cannot be synced.
	for _, sub := range []string{"fetch", "reset", "clean", "checkout"} {
		assertNoSubcommand(t, runner, dir, sub)
	}
}

func TestRefresh_FetchFailure(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {fetchErr: errors.New("network unreachable")},
	})

	sum, err := New().Refresh(context.Background(), newApp(root, runner), nil)
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("expected failed, got %s (%+v)", res.Outcome, res)
	}
	if !strings.Contains(res.Reason, "fetch") {
		t.Errorf("expected reason to mention fetch, got %q", res.Reason)
	}
	// A fetch failure must abort before any destructive local command.
	assertNoSubcommand(t, runner, dir, "reset")
	assertNoSubcommand(t, runner, dir, "clean")
	assertNoSubcommand(t, runner, dir, "checkout")
}

func TestRefresh_SubsetSelection(t *testing.T) {
	root := t.TempDir()
	dirA := makeRepo(t, root, "alpha")
	dirB := makeRepo(t, root, "beta")
	runner := scriptedRunner(map[string]scriptOptions{dirA: {}, dirB: {}})

	sum, err := New().Refresh(context.Background(), newApp(root, runner), []string{"alpha"})
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	if len(sum.Results) != 1 || sum.Results[0].Repo != "alpha" {
		t.Fatalf("expected only alpha processed, got %+v", sum.Results)
	}
	if len(callsInDir(runner, dirB)) != 0 {
		t.Errorf("expected no git calls in beta, got %v", callsInDir(runner, dirB))
	}
}

func TestRefresh_InvalidNameInOnly(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	sum, err := New().Refresh(context.Background(), newApp(root, runner), []string{"alpha", "bogus"})
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if byRepo["bogus"].Outcome != workspace.OutcomeFailed {
		t.Errorf("expected bogus failed, got %s", byRepo["bogus"].Outcome)
	}
	if byRepo["alpha"].Outcome != workspace.OutcomeCreated {
		t.Errorf("expected alpha refreshed, got %s", byRepo["alpha"].Outcome)
	}
}

func TestRefresh_DryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	gh := githubclient.NewMock()
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	a := newAppWithGitHub(root, runner, gh)
	a.DryRun = true

	sum, err := New().Refresh(context.Background(), a, nil)
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected would-refresh (created), got %s (%+v)", res.Outcome, res)
	}
	if !strings.Contains(res.Reason, "would") {
		t.Errorf("expected a preview reason, got %q", res.Reason)
	}
	// Dry-run must issue no git commands and no GitHub mutation.
	if len(runner.Calls) != 0 {
		t.Errorf("expected no git calls in dry-run, got %v", runner.ArgVectors())
	}
	if gh.CallCount("SyncFork") != 0 {
		t.Errorf("expected no SyncFork call in dry-run, got %d", gh.CallCount("SyncFork"))
	}
}

func TestRefresh_EmptyWorkspace(t *testing.T) {
	root := t.TempDir()
	runner := scriptedRunner(map[string]scriptOptions{})

	sum, err := New().Refresh(context.Background(), newApp(root, runner), nil)
	if err != nil {
		t.Fatalf("Refresh returned unexpected error: %v", err)
	}
	if len(sum.Results) != 0 {
		t.Fatalf("expected empty summary, got %+v", sum.Results)
	}
}
