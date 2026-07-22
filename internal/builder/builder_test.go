package builder

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
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// fakeMaker is a recording MakeRunner. It captures the arguments the builder
// passes and returns a scripted error.
type fakeMaker struct {
	called     bool
	gotDir     string
	gotService string
	gotEnv     []string
	err        error
}

func (f *fakeMaker) Run(_ context.Context, dir, service string, env []string) error {
	f.called = true
	f.gotDir = dir
	f.gotService = service
	f.gotEnv = env
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

// appWith wires an App around the given root, git runner, and dry-run flag.
func appWith(root string, runner git.Runner, dryRun bool) app.App {
	return app.App{
		Config: config.Config{
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   1,
		},
		Git:    runner,
		DryRun: dryRun,
	}
}

// branchRunner returns a MockRunner that answers `symbolic-ref` with the given
// branch name so CurrentBranch reports it.
func branchRunner(branch string) *git.MockRunner {
	return &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return branch + "\n", nil
		}
		return "", nil
	}}
}

// only returns the single Result from a one-result Summary.
func only(t *testing.T, s workspace.Summary) workspace.Result {
	t.Helper()
	if len(s.Results) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(s.Results), s.Results)
	}
	return s.Results[0]
}

func TestBuild_HappyPathRunsMakeWithEnvOverrides(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	maker := &fakeMaker{}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("feature-x"), false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "feature-x") || !strings.Contains(res.Reason, "ecr-controller") {
		t.Errorf("reason = %q, want it to mention the branch and controller", res.Reason)
	}

	if !maker.called {
		t.Fatal("make was not run")
	}
	// The bare alias (not the full -controller name) is passed to the code-generator.
	if maker.gotService != "ecr" {
		t.Errorf("service = %q, want ecr", maker.gotService)
	}
	if maker.gotDir != filepath.Join(root, codegenDirName) {
		t.Errorf("dir = %q, want %q", maker.gotDir, filepath.Join(root, codegenDirName))
	}

	// The three workspace-path overrides must be wired to the real workspace root.
	assertEnv(t, maker.gotEnv, envRuntimeCRDDir+"="+filepath.Join(root, "runtime", "config"))
	assertEnv(t, maker.gotEnv, envAckGenerateBinPath+"="+filepath.Join(root, codegenDirName, "bin", "ack-generate"))
	assertEnv(t, maker.gotEnv, envTemplatesDir+"="+filepath.Join(root, codegenDirName, "templates"))
	// Without --sdk-version, AWS_SDK_GO_VERSION is left for the scripts to resolve.
	for _, e := range maker.gotEnv {
		if strings.HasPrefix(e, envAWSSDKGoVersion+"=") {
			t.Errorf("unexpected %s override without --sdk-version: %q", envAWSSDKGoVersion, e)
		}
	}
}

func TestBuild_FullControllerNameNormalizesToAlias(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	maker := &fakeMaker{}

	if _, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("main"), false), "ecr-controller", Options{}); err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if maker.gotService != "ecr" {
		t.Errorf("service = %q, want ecr (suffix stripped)", maker.gotService)
	}
}

func TestBuild_SDKVersionPinnedIsPassed(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	maker := &fakeMaker{}

	if _, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("main"), false), "ecr", Options{SDKVersion: "v1.41.0"}); err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	assertEnv(t, maker.gotEnv, envAWSSDKGoVersion+"=v1.41.0")
}

func TestBuild_MissingControllerFails(t *testing.T) {
	root := t.TempDir() // no controller, no code-generator
	maker := &fakeMaker{}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, &git.MockRunner{}, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if maker.called {
		t.Error("make must not run when the controller is absent")
	}
}

func TestBuild_MissingCodeGeneratorFails(t *testing.T) {
	root := t.TempDir()
	// Controller present but code-generator absent.
	mustMkdir(t, filepath.Join(root, "ecr-controller", ".git"))
	maker := &fakeMaker{}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("main"), false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "code-generator") {
		t.Errorf("reason = %q, want it to mention the missing code-generator", res.Reason)
	}
	if maker.called {
		t.Error("make must not run when the code-generator is absent")
	}
}

func TestBuild_MakeFailurePropagatesAsFailed(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	maker := &fakeMaker{err: errors.New("boom")}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("main"), false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "boom") {
		t.Errorf("reason = %q, want it to carry the make failure", res.Reason)
	}
}

func TestBuild_DryRunTouchesNothing(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	maker := &fakeMaker{}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, branchRunner("main"), true), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created (preview)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "would") {
		t.Errorf("reason = %q, want a preview describing what would happen", res.Reason)
	}
	// The preview must still surface the env overrides that make it work.
	if !strings.Contains(res.Reason, envRuntimeCRDDir) {
		t.Errorf("preview = %q, want it to mention the env overrides", res.Reason)
	}
	if maker.called {
		t.Error("dry-run must not run make")
	}
}

func TestBuild_DetachedHeadReported(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	// symbolic-ref -q on a detached HEAD exits 1 with no output.
	mr := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return "", &git.ExitError{Code: 1}
		}
		return "", nil
	}}
	maker := &fakeMaker{}

	summary, err := NewWithMakeRunner(maker).Build(
		context.Background(), appWith(root, mr, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created", res.Outcome)
	}
	if !strings.Contains(res.Reason, "detached HEAD") {
		t.Errorf("reason = %q, want it to note the detached HEAD", res.Reason)
	}
	if !maker.called {
		t.Error("a detached HEAD must not prevent the build")
	}
}

func TestBuild_EmptyServiceIsUsageError(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	_, err := NewWithMakeRunner(&fakeMaker{}).Build(
		context.Background(), appWith(root, &git.MockRunner{}, false), "", Options{})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *UsageError", err, err)
	}
}

// assertEnv fails the test if want is not among the env entries.
func assertEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("expected env entry %q; got %v", want, env)
}
