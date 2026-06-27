package remover

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

// makeRepo creates a managed repo directory (with a .git entry) under root.
func makeRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("setup repo %q: %v", name, err)
	}
	return dir
}

func resultsByRepo(s workspace.Summary) map[string]workspace.Result {
	out := make(map[string]workspace.Result, len(s.Results))
	for _, r := range s.Results {
		out[r.Repo] = r
	}
	return out
}

// markForkPresent registers the fork "<prefix><name>" under the test user.
func markForkPresent(m *githubclient.Mock, name string) {
	m.SetRepo(githubclient.RepoRef{Owner: testUser, Name: testPrefix + name}, githubclient.RepoState{Exists: true})
}

func cleanRunner() *git.MockRunner {
	r := &git.MockRunner{}
	// status --porcelain returns empty => clean working tree.
	r.ResponseFunc = func(_ string, _ []string) (string, error) { return "", nil }
	return r
}

func TestRemove_EmptyListGuard(t *testing.T) {
	gh := githubclient.NewMock()
	_, err := New().Remove(context.Background(), newApp(t.TempDir(), gh, cleanRunner()), nil, Options{})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UsageError for empty list, got %T: %v", err, err)
	}
}

func TestRemove_RefusesUpstreamIdentity(t *testing.T) {
	gh := githubclient.NewMock()
	ap := newApp(t.TempDir(), gh, cleanRunner())
	ap.Config.GitHubUser = upstreamOwner // misconfigured to the org

	_, err := New().Remove(context.Background(), ap, []string{"s3"}, Options{})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UsageError refusing the upstream identity, got %T: %v", err, err)
	}
	// No delete should ever be attempted.
	if gh.CallCount("DeleteRepo") != 0 {
		t.Errorf("must not attempt any delete, got %d", gh.CallCount("DeleteRepo"))
	}
}

func TestRemove_DeletesLocalAndFork(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")

	sum, err := New().Remove(context.Background(), newApp(root, gh, cleanRunner()), []string{"s3"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["s3-controller"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected removed (created bucket), got %s (%q)", res.Outcome, res.Reason)
	}
	// The local clone is gone.
	if dirExists(dir) {
		t.Errorf("expected local clone %q to be deleted", dir)
	}
	// The fork delete targeted the contributor's prefixed fork.
	calls := gh.CallsFor("DeleteRepo")
	if len(calls) != 1 {
		t.Fatalf("expected 1 DeleteRepo call, got %d", len(calls))
	}
	if calls[0].Ref.Owner != testUser || calls[0].Ref.Name != testPrefix+"s3-controller" {
		t.Errorf("DeleteRepo targeted %s, want %s/%s", calls[0].Ref, testUser, testPrefix+"s3-controller")
	}
}

func TestRemove_KeepForkDeletesOnlyLocal(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")

	sum, err := New().Remove(context.Background(), newApp(root, gh, cleanRunner()), []string{"s3"}, Options{KeepFork: true})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	if resultsByRepo(sum)["s3-controller"].Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected removed, got %+v", sum.Results)
	}
	if dirExists(dir) {
		t.Errorf("expected local clone to be deleted")
	}
	// With --keep-fork, neither a RepoExists check nor a delete should occur.
	if gh.CallCount("DeleteRepo") != 0 {
		t.Errorf("keep-fork must not delete the fork, got %d deletes", gh.CallCount("DeleteRepo"))
	}
}

func TestRemove_DirtySkippedUnlessForced(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")

	dirtyRunner := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "status" {
			return " M file.go\n", nil // dirty
		}
		return "", nil
	}}

	sum, err := New().Remove(context.Background(), newApp(root, gh, dirtyRunner), []string{"s3"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	res := resultsByRepo(sum)["s3-controller"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected dirty repo skipped, got %s", res.Outcome)
	}
	// Nothing deleted.
	if !dirExists(dir) {
		t.Errorf("dirty repo's local clone must be preserved")
	}
	if gh.CallCount("DeleteRepo") != 0 {
		t.Errorf("dirty repo's fork must not be deleted, got %d", gh.CallCount("DeleteRepo"))
	}
}

func TestRemove_DirtyForcedRemoves(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")

	dirtyRunner := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "status" {
			return " M file.go\n", nil
		}
		return "", nil
	}}

	sum, err := New().Remove(context.Background(), newApp(root, gh, dirtyRunner), []string{"s3"}, Options{Force: true})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	if resultsByRepo(sum)["s3-controller"].Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected forced removal to succeed, got %+v", sum.Results)
	}
	if dirExists(dir) {
		t.Errorf("expected forced removal to delete the local clone")
	}
	if gh.CallCount("DeleteRepo") != 1 {
		t.Errorf("expected the fork to be deleted under --force")
	}
}

func TestRemove_NothingToRemove(t *testing.T) {
	root := t.TempDir()
	gh := githubclient.NewMock() // fork absent, no local dir
	sum, err := New().Remove(context.Background(), newApp(root, gh, cleanRunner()), []string{"s3"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	res := resultsByRepo(sum)["s3-controller"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped when nothing to remove, got %s", res.Outcome)
	}
}

func TestRemove_ForkDeleteFailurePreservesLocal(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")
	gh.DeleteRepoErr = errors.New("boom: delete forbidden")

	sum, err := New().Remove(context.Background(), newApp(root, gh, cleanRunner()), []string{"s3"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	res := resultsByRepo(sum)["s3-controller"]
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("expected failed on fork-delete error, got %s", res.Outcome)
	}
	// The local clone must be left intact so the user can retry.
	if !dirExists(dir) {
		t.Errorf("local clone must be preserved when the fork delete fails")
	}
}

func TestRemove_AllExpandsToWorkspaceControllers(t *testing.T) {
	root := t.TempDir()
	// Two controllers plus a core repo that must NOT be removed.
	makeRepo(t, root, "s3-controller")
	makeRepo(t, root, "sns-controller")
	makeRepo(t, root, "runtime")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")
	markForkPresent(gh, "sns-controller")

	sum, err := New().Remove(context.Background(), newApp(root, gh, cleanRunner()), []string{"all"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	if len(sum.Results) != 2 {
		t.Fatalf("expected exactly 2 controllers removed, got %d (%+v)", len(sum.Results), sum.Results)
	}
	for _, name := range []string{"s3-controller", "sns-controller"} {
		if byRepo[name].Outcome != workspace.OutcomeCreated {
			t.Errorf("expected %s removed, got %+v", name, byRepo[name])
		}
	}
	// The core repo must be untouched.
	if _, ok := byRepo["runtime"]; ok {
		t.Errorf("remove all must not touch the core repo 'runtime'")
	}
	if dirExists(filepath.Join(root, "runtime")) == false {
		t.Errorf("the runtime directory must still exist")
	}
}

func TestRemove_DryRunDeletesNothing(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "s3-controller")
	gh := githubclient.NewMock()
	markForkPresent(gh, "s3-controller")
	ap := newApp(root, gh, cleanRunner())
	ap.DryRun = true

	sum, err := New().Remove(context.Background(), ap, []string{"s3"}, Options{})
	if err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	if resultsByRepo(sum)["s3-controller"].Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected a would-remove preview, got %+v", sum.Results)
	}
	// Dry-run must not delete the local clone or the fork.
	if !dirExists(dir) {
		t.Errorf("dry-run must not delete the local clone")
	}
	if gh.CallCount("DeleteRepo") != 0 {
		t.Errorf("dry-run must not delete the fork, got %d", gh.CallCount("DeleteRepo"))
	}
}
