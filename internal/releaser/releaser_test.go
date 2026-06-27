package releaser

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

// fakeScript is a recording ScriptRunner. onRun, when set, runs after the call
// is recorded so a test can flip state (for example, make the working tree
// appear dirty once the release artifacts have been "generated").
type fakeScript struct {
	called     bool
	gotDir     string
	gotService string
	gotVersion string
	err        error
	onRun      func()
}

func (f *fakeScript) Run(_ context.Context, dir, service, version string) error {
	f.called = true
	f.gotDir = dir
	f.gotService = service
	f.gotVersion = version
	if f.onRun != nil {
		f.onRun()
	}
	return f.err
}

// workspaceWithController builds a temporary workspace root containing a git
// controller clone and a code-generator directory, returning the root.
func workspaceWithController(t *testing.T, controller string) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, controller, ".git"))
	mustMkdir(t, filepath.Join(root, codegenDirName))
	return root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// appWith wires an App around the given root, git runner, and GitHub mock.
func appWith(root string, runner git.Runner, gh githubclient.GitHubClient, dryRun bool) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    "octocat",
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   1,
		},
		GitHub: gh,
		Git:    runner,
		DryRun: dryRun,
	}
}

// only returns the single Result from a one-result Summary.
func only(t *testing.T, s workspace.Summary) workspace.Result {
	t.Helper()
	if len(s.Results) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(s.Results), s.Results)
	}
	return s.Results[0]
}

func TestRelease_HappyPathPushesAndOpensPR(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")

	script := &fakeScript{}
	mr := &git.MockRunner{}
	// The working tree is clean until the release script runs, after which it
	// reports generated changes so the commit step proceeds.
	script.onRun = func() {}
	dirtyAfterScript := false
	mr.ResponseFunc = func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "status" && len(args) > 1 && args[1] == "--porcelain":
			if dirtyAfterScript {
				return " M apis/v1alpha1/generator.go", nil
			}
			return "", nil
		case args[0] == "rev-parse":
			// Release branch does not exist yet (exit status 1).
			return "", &git.ExitError{Code: 1}
		default:
			return "", nil
		}
	}
	script.onRun = func() { dirtyAfterScript = true }

	gh := githubclient.NewMock()
	gh.PullRequestURL = "https://github.com/aws-controllers-k8s/ecr-controller/pull/42"

	r := NewWithScriptRunner(script)
	summary, err := r.Release(context.Background(), appWith(root, mr, gh, false), "ecr", Options{Version: "1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}

	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "v1.0.1") || !strings.Contains(res.Reason, "pull/42") {
		t.Errorf("reason = %q, want it to mention v1.0.1 and the PR URL", res.Reason)
	}

	// Script invoked with the normalized version, bare alias, and code-generator dir.
	if !script.called {
		t.Fatal("release script was not run")
	}
	if script.gotService != "ecr" || script.gotVersion != "v1.0.1" {
		t.Errorf("script run with service=%q version=%q, want ecr / v1.0.1", script.gotService, script.gotVersion)
	}
	if script.gotDir != filepath.Join(root, codegenDirName) {
		t.Errorf("script dir = %q, want %q", script.gotDir, filepath.Join(root, codegenDirName))
	}

	// The expected git commands were issued.
	assertGitCall(t, mr, []string{"fetch", "upstream"})
	assertGitCall(t, mr, []string{"checkout", "-b", "release-v1.0.1"})
	assertGitCall(t, mr, []string{"commit", "-a", "-m", "Release artifacts for release v1.0.1"})
	assertGitCall(t, mr, []string{"push", "origin", "release-v1.0.1"})

	// The PR targets upstream from the contributor's namespaced head branch.
	prCalls := gh.CallsFor("CreatePullRequest")
	if len(prCalls) != 1 {
		t.Fatalf("CreatePullRequest called %d times, want 1", len(prCalls))
	}
	pr := prCalls[0]
	if pr.Ref.Owner != upstreamOwner || pr.Ref.Name != "ecr-controller" {
		t.Errorf("PR upstream = %s, want aws-controllers-k8s/ecr-controller", pr.Ref)
	}
	if pr.PullRequest.Head != "octocat:release-v1.0.1" {
		t.Errorf("PR head = %q, want octocat:release-v1.0.1", pr.PullRequest.Head)
	}
	if pr.PullRequest.Base != "main" {
		t.Errorf("PR base = %q, want main", pr.PullRequest.Base)
	}
	if !strings.Contains(pr.PullRequest.Body, "Opened by `ack-workspace release`") {
		t.Errorf("PR body = %q, want the generated default body", pr.PullRequest.Body)
	}
}

func TestRelease_CustomPRBodyOverridesDefault(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")

	dirtyAfterScript := false
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "status":
			if dirtyAfterScript {
				return " M file", nil
			}
			return "", nil
		case args[0] == "rev-parse":
			return "", &git.ExitError{Code: 1}
		default:
			return "", nil
		}
	}}
	script := &fakeScript{onRun: func() { dirtyAfterScript = true }}
	gh := githubclient.NewMock()

	const body = "## Custom release notes\n\n- handcrafted"
	_, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, gh, false), "ecr",
		Options{Version: "v1.0.1", PRBody: body})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}

	prCalls := gh.CallsFor("CreatePullRequest")
	if len(prCalls) != 1 {
		t.Fatalf("CreatePullRequest called %d times, want 1", len(prCalls))
	}
	if got := prCalls[0].PullRequest.Body; got != body {
		t.Errorf("PR body = %q, want the supplied custom body %q", got, body)
	}
}

func TestRelease_SkipPRPushesButDoesNotOpenPR(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")

	dirtyAfterScript := false
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "status" && len(args) > 1 && args[1] == "--porcelain":
			if dirtyAfterScript {
				return " M file", nil
			}
			return "", nil
		case args[0] == "rev-parse":
			return "", &git.ExitError{Code: 1}
		default:
			return "", nil
		}
	}}
	script := &fakeScript{onRun: func() { dirtyAfterScript = true }}
	gh := githubclient.NewMock()

	r := NewWithScriptRunner(script)
	summary, err := r.Release(context.Background(), appWith(root, mr, gh, false), "ecr", Options{Version: "v2.0.0", SkipPR: true})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "PR skipped") {
		t.Errorf("reason = %q, want it to note the PR was skipped", res.Reason)
	}
	if n := gh.CallCount("CreatePullRequest"); n != 0 {
		t.Errorf("CreatePullRequest called %d times, want 0 with --skip-pr", n)
	}
	assertGitCall(t, mr, []string{"push", "origin", "release-v2.0.0"})
}

func TestRelease_DirtyWorkingTreeSkipped(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if args[0] == "status" {
			return " M dirty.go", nil // dirty from the very first check
		}
		return "", nil
	}}
	script := &fakeScript{}

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", res.Outcome)
	}
	if script.called {
		t.Error("release script must not run when the working tree is dirty")
	}
}

func TestRelease_NoGeneratedChangesSkipped(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	// Working tree stays clean even after the script "runs": no artifacts changed.
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if args[0] == "rev-parse" {
			return "", &git.ExitError{Code: 1}
		}
		return "", nil // status always clean
	}}
	script := &fakeScript{}

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "no changes") {
		t.Errorf("reason = %q, want it to mention no changes", res.Reason)
	}
	for _, c := range mr.Calls {
		if len(c.Args) > 0 && c.Args[0] == "commit" {
			t.Error("must not commit when the script produced no changes")
		}
	}
}

func TestRelease_ExistingBranchSkipped(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if args[0] == "rev-parse" {
			return "abc123\n", nil // branch already exists (exit 0)
		}
		return "", nil
	}}
	script := &fakeScript{}

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped; reason: %s", res.Outcome, res.Reason)
	}
	if script.called {
		t.Error("release script must not run when the release branch already exists")
	}
}

func TestRelease_DivergedBaseFails(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if args[0] == "merge-base" {
			return "", &git.ExitError{Code: 1} // not an ancestor: cannot fast-forward
		}
		return "", nil
	}}
	script := &fakeScript{}

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "diverged") {
		t.Errorf("reason = %q, want it to mention diverged history", res.Reason)
	}
}

func TestRelease_MissingControllerFails(t *testing.T) {
	root := t.TempDir() // no controller, no code-generator
	mr := &git.MockRunner{}
	summary, err := NewWithScriptRunner(&fakeScript{}).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if len(mr.Calls) != 0 {
		t.Errorf("no git commands should run when the controller is absent, got %v", mr.ArgVectors())
	}
}

func TestRelease_DryRunTouchesNothing(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	mr := &git.MockRunner{}
	script := &fakeScript{}

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, githubclient.NewMock(), true), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created (preview)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "would") {
		t.Errorf("reason = %q, want a preview describing what would happen", res.Reason)
	}
	if len(mr.Calls) != 0 {
		t.Errorf("dry-run must not run any git command, got %v", mr.ArgVectors())
	}
	if script.called {
		t.Error("dry-run must not run the release script")
	}
}

func TestRelease_PRFailurePropagatesAsFailed(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	dirtyAfterScript := false
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		switch {
		case args[0] == "status":
			if dirtyAfterScript {
				return " M file", nil
			}
			return "", nil
		case args[0] == "rev-parse":
			return "", &git.ExitError{Code: 1}
		default:
			return "", nil
		}
	}}
	script := &fakeScript{onRun: func() { dirtyAfterScript = true }}
	gh := githubclient.NewMock()
	gh.CreatePullRequestErr = errors.New("boom")

	summary, err := NewWithScriptRunner(script).Release(
		context.Background(), appWith(root, mr, gh, false), "ecr", Options{Version: "v1.0.1"})
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
}

func TestRelease_UsageErrors(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	r := NewWithScriptRunner(&fakeScript{})

	cases := []struct {
		name    string
		service string
		opts    Options
		app     app.App
	}{
		{"empty service", "", Options{Version: "v1.0.1"}, appWith(root, &git.MockRunner{}, githubclient.NewMock(), false)},
		{"missing version", "ecr", Options{}, appWith(root, &git.MockRunner{}, githubclient.NewMock(), false)},
		{"bad version", "ecr", Options{Version: "latest"}, appWith(root, &git.MockRunner{}, githubclient.NewMock(), false)},
		{"missing identity for PR", "ecr", Options{Version: "v1.0.1"}, func() app.App {
			a := appWith(root, &git.MockRunner{}, githubclient.NewMock(), false)
			a.Config.GitHubUser = ""
			return a
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Release(context.Background(), tc.app, tc.service, tc.opts)
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("error = %v (%T), want *UsageError", err, err)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v1.0.1", "v1.0.1", false},
		{"1.0.1", "v1.0.1", false},
		{" 1.2.0 ", "v1.2.0", false},
		{"V3.4.5", "v3.4.5", false},
		{"v1.2.0-rc.1", "v1.2.0-rc.1", false},
		{"", "", true},
		{"latest", "", true},
		{"1.0", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeVersion(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeVersion(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// assertGitCall fails the test if want was not among the recorded git argument
// vectors.
func assertGitCall(t *testing.T, mr *git.MockRunner, want []string) {
	t.Helper()
	for _, got := range mr.ArgVectors() {
		if equalArgs(got, want) {
			return
		}
	}
	t.Errorf("expected git command %v to have been issued; recorded: %v", want, mr.ArgVectors())
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
