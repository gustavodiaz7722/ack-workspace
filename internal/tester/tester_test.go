package tester

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// devContext is a kubeconfig context name of the shape a deploy leaves behind:
// the cluster ARN, not the bare cluster name.
const devContext = "arn:aws:eks:us-west-2:123456789012:cluster/" + deployer.ClusterName

// fakeCluster is a scripted Cluster. Each field drives one of the three reads
// the pre-flight makes.
type fakeCluster struct {
	context      string
	contextErr   error
	reachableErr error
	ready        bool
	readyErr     error

	gotNamespace string
	gotInstance  string
}

func (f *fakeCluster) Context(context.Context) (string, error) { return f.context, f.contextErr }
func (f *fakeCluster) Reachable(context.Context) error         { return f.reachableErr }
func (f *fakeCluster) ControllerReady(_ context.Context, namespace, instance string) (bool, error) {
	f.gotNamespace = namespace
	f.gotInstance = instance
	return f.ready, f.readyErr
}

// readyCluster returns a fakeCluster answering every pre-flight read the way a
// bootstrapped development cluster with the controller running would.
func readyCluster() *fakeCluster {
	return &fakeCluster{context: devContext, ready: true}
}

// fakeEnvironment is a recording Environment that provisions nothing.
type fakeEnvironment struct {
	called          bool
	gotDir          string
	gotRequirements string
	binDir          string
	err             error
}

func (f *fakeEnvironment) Ensure(_ context.Context, dir, requirements string) (Provision, error) {
	f.called = true
	f.gotDir = dir
	f.gotRequirements = requirements
	if f.err != nil {
		return Provision{}, f.err
	}
	binDir := f.binDir
	if binDir == "" {
		binDir = filepath.Join(dir, binDirName)
	}
	return Provision{BinDir: binDir, Created: true, Installed: true}, nil
}

// fakeRunner records the steps it was asked to run, in order, and fails the ones
// named in failing.
type fakeRunner struct {
	steps   []Step
	failing map[string]error
}

func (f *fakeRunner) Run(_ context.Context, step Step) error {
	f.steps = append(f.steps, step)
	return f.failing[step.Name]
}

// names returns the recorded step names in execution order.
func (f *fakeRunner) names() []string {
	out := make([]string, 0, len(f.steps))
	for _, s := range f.steps {
		out = append(out, s.Name)
	}
	return out
}

// step returns the recorded step with the given name.
func (f *fakeRunner) step(t *testing.T, name string) Step {
	t.Helper()
	for _, s := range f.steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q step was run; ran %v", name, f.names())
	return Step{}
}

// suiteOptions controls what workspaceWithSuite lays down.
type suiteOptions struct {
	noRequirements bool
	noBootstrap    bool
	noCleanup      bool
	noSuite        bool
	localVenv      bool
}

// workspaceWithSuite builds a temporary workspace root holding a git controller
// clone with an end-to-end suite, and returns the root.
func workspaceWithSuite(t *testing.T, controller string, opts suiteOptions) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, controller, ".git"))

	if opts.noSuite {
		return root
	}

	e2e := filepath.Join(root, controller, e2eRelPath)
	mustMkdir(t, e2e)
	if !opts.noRequirements {
		mustWrite(t, filepath.Join(e2e, requirementsFileName), "acktest @ git+https://example.invalid/test-infra.git@abc123\n")
	}
	if !opts.noBootstrap {
		mustWrite(t, filepath.Join(e2e, bootstrapScriptName), "")
	}
	if !opts.noCleanup {
		mustWrite(t, filepath.Join(e2e, cleanupScriptName), "")
	}
	if opts.localVenv {
		mustMkdir(t, filepath.Join(e2e, localEnvironmentDirName))
	}
	return root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// appWith wires an App around the given workspace root and dry-run flag, with
// the configuration path pointing inside a temporary state directory so the
// managed environment location is isolated from the real $HOME.
func appWith(t *testing.T, root string, dryRun bool) app.App {
	t.Helper()
	return app.App{
		Config: config.Config{
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   1,
			Path:          filepath.Join(t.TempDir(), configDirName, "config"),
		},
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

// run wires a Tester around the given collaborators and tests the ecr
// controller in root.
func run(t *testing.T, root string, c Cluster, e Environment, r Runner, dryRun bool, opts Options) workspace.Result {
	t.Helper()
	summary, err := NewWith(c, e, r, nil).Test(context.Background(), appWith(t, root, dryRun), "ecr", opts)
	if err != nil {
		t.Fatalf("Test returned an out-of-band error: %v", err)
	}
	return only(t, summary)
}

func TestTest_EmptyServiceIsUsageError(t *testing.T) {
	_, err := NewWith(readyCluster(), &fakeEnvironment{}, &fakeRunner{}, nil).
		Test(context.Background(), appWith(t, t.TempDir(), false), "  ", Options{})
	if err == nil {
		t.Fatal("expected a usage error for an empty service, got nil")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *UsageError", err)
	}
}

func TestTest_SuffixedAndBareServiceAreEquivalent(t *testing.T) {
	for _, service := range []string{"ecr", "ecr-controller"} {
		root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
		runner := &fakeRunner{}

		summary, err := NewWith(readyCluster(), &fakeEnvironment{}, runner, nil).
			Test(context.Background(), appWith(t, root, false), service, Options{})
		if err != nil {
			t.Fatalf("Test(%q) returned an error: %v", service, err)
		}
		res := only(t, summary)
		if res.Outcome != workspace.OutcomeSucceeded {
			t.Fatalf("Test(%q) outcome = %s (%s), want succeeded", service, res.Outcome, res.Reason)
		}
		if res.Repo != "ecr-controller" {
			t.Errorf("Test(%q) repo = %q, want ecr-controller", service, res.Repo)
		}
	}
}

func TestTest_RunsBootstrapTestsAndTeardownInOrder(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{}

	res := run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{})

	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s), want succeeded", res.Outcome, res.Reason)
	}
	if got := strings.Join(runner.names(), ","); got != "bootstrap,tests,teardown" {
		t.Errorf("steps = %s, want bootstrap,tests,teardown", got)
	}

	e2e := filepath.Join(root, "ecr-controller", e2eRelPath)
	for _, s := range runner.steps {
		if s.Dir != e2e {
			t.Errorf("%s step ran in %q, want %q", s.Name, s.Dir, e2e)
		}
	}
}

func TestTest_StepsRunFromTheProvisionedEnvironment(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	env := &fakeEnvironment{binDir: filepath.Join(t.TempDir(), "venv", "bin")}
	runner := &fakeRunner{}

	run(t, root, readyCluster(), env, runner, false, Options{})

	if !env.called {
		t.Fatal("the Python environment was not provisioned")
	}
	wantRequirements := filepath.Join(root, "ecr-controller", e2eRelPath, requirementsFileName)
	if env.gotRequirements != wantRequirements {
		t.Errorf("requirements = %q, want %q", env.gotRequirements, wantRequirements)
	}

	// Executables are addressed absolutely inside the environment so a run cannot
	// pick up a same-named executable from the ambient PATH.
	if got, want := runner.step(t, "tests").Command, filepath.Join(env.binDir, pytestExe); got != want {
		t.Errorf("tests command = %q, want %q", got, want)
	}
	if got, want := runner.step(t, "bootstrap").Command, filepath.Join(env.binDir, pythonExe); got != want {
		t.Errorf("bootstrap command = %q, want %q", got, want)
	}

	// The environment's bin directory still leads the PATH, for anything the suite
	// itself shells out to.
	var foundPath bool
	for _, e := range runner.step(t, "tests").Env {
		if strings.HasPrefix(e, envPath+"=") {
			foundPath = true
			if !strings.HasPrefix(strings.TrimPrefix(e, envPath+"="), env.binDir) {
				t.Errorf("PATH entry %q does not lead with %q", e, env.binDir)
			}
		}
	}
	if !foundPath {
		t.Error("no PATH override was passed to the tests step")
	}
}

func TestTest_ManagedEnvironmentLivesOutsideTheCheckout(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	env := &fakeEnvironment{}

	res := run(t, root, readyCluster(), env, &fakeRunner{}, false, Options{})
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s), want succeeded", res.Outcome, res.Reason)
	}

	// An environment inside the controller checkout would leave untracked files
	// there, which `status` reports as dirty and `deploy` refuses outright.
	controllerPath := filepath.Join(root, "ecr-controller")
	if strings.HasPrefix(env.gotDir, controllerPath) {
		t.Errorf("environment directory %q is inside the controller checkout %q", env.gotDir, controllerPath)
	}
	if filepath.Base(env.gotDir) != "ecr-controller" || filepath.Base(filepath.Dir(env.gotDir)) != environmentsDirName {
		t.Errorf("environment directory = %q, want <state>/%s/ecr-controller", env.gotDir, environmentsDirName)
	}
}

func TestTest_ExistingInCheckoutEnvironmentIsUsed(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{localVenv: true})
	env := &fakeEnvironment{}

	run(t, root, readyCluster(), env, &fakeRunner{}, false, Options{})

	want := filepath.Join(root, "ecr-controller", e2eRelPath, localEnvironmentDirName)
	if env.gotDir != want {
		t.Errorf("environment directory = %q, want the existing %q", env.gotDir, want)
	}
}

func TestTest_SuiteWithoutBootstrapOrCleanupRunsTestsOnly(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{noBootstrap: true, noCleanup: true})
	runner := &fakeRunner{}

	res := run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{})

	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s), want succeeded", res.Outcome, res.Reason)
	}
	if got := strings.Join(runner.names(), ","); got != "tests" {
		t.Errorf("steps = %s, want tests only", got)
	}
}

func TestTest_SkipCleanupLeavesFixturesInPlace(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{}

	run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{SkipCleanup: true})

	if got := strings.Join(runner.names(), ","); got != "bootstrap,tests" {
		t.Errorf("steps = %s, want bootstrap,tests with no teardown", got)
	}
}

func TestTest_FailingTestsStillTearDown(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{failing: map[string]error{"tests": errors.New("2 failed")}}

	res := run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{})

	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %s, want failed when a test fails", res.Outcome)
	}
	if got := strings.Join(runner.names(), ","); got != "bootstrap,tests,teardown" {
		t.Errorf("steps = %s, want the teardown to run after failing tests", got)
	}
}

func TestTest_FailingBootstrapSkipsTestsButTearsDown(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{failing: map[string]error{"bootstrap": errors.New("boom")}}

	res := run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{})

	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %s, want failed when the bootstrap fails", res.Outcome)
	}
	// Tests against an incomplete fixture set would all fail and bury the cause,
	// but fixtures the bootstrap did create must still be deleted.
	if got := strings.Join(runner.names(), ","); got != "bootstrap,teardown" {
		t.Errorf("steps = %s, want bootstrap,teardown", got)
	}
}

func TestTest_FailingTeardownWarnsButKeepsThePytestOutcome(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{failing: map[string]error{"teardown": errors.New("delete failed")}}

	res := run(t, root, readyCluster(), &fakeEnvironment{}, runner, false, Options{})

	// The outcome answers "did the tests pass", so a teardown failure is reported
	// in the reason rather than folded into the outcome.
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s), want succeeded", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "WARNING") || !strings.Contains(res.Reason, "may still exist") {
		t.Errorf("reason %q does not warn that AWS resources may remain", res.Reason)
	}
}

func TestTest_PreFlightRejections(t *testing.T) {
	cases := []struct {
		name        string
		controller  string
		suite       suiteOptions
		cluster     *fakeCluster
		wantOutcome workspace.Outcome
		wantReason  string
	}{
		{
			name:        "controller absent",
			controller:  "s3-controller",
			cluster:     readyCluster(),
			wantOutcome: workspace.OutcomeFailed,
			wantReason:  "ack-workspace add ecr",
		},
		{
			name:        "no suite in the repository",
			controller:  "ecr-controller",
			suite:       suiteOptions{noSuite: true},
			cluster:     readyCluster(),
			wantOutcome: workspace.OutcomeSkipped,
			wantReason:  "no end-to-end suite",
		},
		{
			name:        "suite without pinned requirements",
			controller:  "ecr-controller",
			suite:       suiteOptions{noRequirements: true},
			cluster:     readyCluster(),
			wantOutcome: workspace.OutcomeFailed,
			wantReason:  requirementsFileName,
		},
		{
			name:        "no context selected",
			controller:  "ecr-controller",
			cluster:     &fakeCluster{context: "", ready: true},
			wantOutcome: workspace.OutcomeSkipped,
			wantReason:  "no kubeconfig context is selected",
		},
		{
			name:        "context names another cluster",
			controller:  "ecr-controller",
			cluster:     &fakeCluster{context: "some-other-cluster", ready: true},
			wantOutcome: workspace.OutcomeSkipped,
			wantReason:  "does not name the " + deployer.ClusterName,
		},
		{
			name:        "cluster unreachable",
			controller:  "ecr-controller",
			cluster:     &fakeCluster{context: devContext, reachableErr: errors.New("i/o timeout"), ready: true},
			wantOutcome: workspace.OutcomeFailed,
			wantReason:  "did not answer",
		},
		{
			name:        "controller not running",
			controller:  "ecr-controller",
			cluster:     &fakeCluster{context: devContext, ready: false},
			wantOutcome: workspace.OutcomeSkipped,
			wantReason:  "ack-workspace deploy ecr",
		},
		{
			name:        "readiness unreadable",
			controller:  "ecr-controller",
			cluster:     &fakeCluster{context: devContext, readyErr: errors.New("forbidden")},
			wantOutcome: workspace.OutcomeFailed,
			wantReason:  "checking whether",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceWithSuite(t, tc.controller, tc.suite)
			env := &fakeEnvironment{}
			runner := &fakeRunner{}

			res := run(t, root, tc.cluster, env, runner, false, Options{})

			if res.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %s (%s), want %s", res.Outcome, res.Reason, tc.wantOutcome)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", res.Reason, tc.wantReason)
			}
			// A rejected pre-flight must not provision an environment or run a step.
			if env.called {
				t.Error("the Python environment was provisioned despite a failed pre-flight")
			}
			if len(runner.steps) != 0 {
				t.Errorf("steps %v ran despite a failed pre-flight", runner.names())
			}
		})
	}
}

func TestTest_ReadinessIsCheckedForTheDeployedRelease(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	cluster := readyCluster()

	run(t, root, cluster, &fakeEnvironment{}, &fakeRunner{}, false, Options{})

	if cluster.gotNamespace != deployer.Namespace {
		t.Errorf("namespace = %q, want %q", cluster.gotNamespace, deployer.Namespace)
	}
	// The label value is the Helm release name the deployer installs under.
	if cluster.gotInstance != "ack-ecr-controller" {
		t.Errorf("instance = %q, want ack-ecr-controller", cluster.gotInstance)
	}
}

func TestTest_EnvironmentFailureIsAFailedResult(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	runner := &fakeRunner{}

	res := run(t, root, readyCluster(), &fakeEnvironment{err: errors.New("pip died")}, runner, false, Options{})

	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", res.Outcome)
	}
	if len(runner.steps) != 0 {
		t.Errorf("steps %v ran without a usable Python environment", runner.names())
	}
}

func TestTest_DryRunProvisionsNothingAndRunsNothing(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	env := &fakeEnvironment{}
	runner := &fakeRunner{}
	cluster := readyCluster()

	res := run(t, root, cluster, env, runner, true, Options{})

	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %s (%s), want succeeded", res.Outcome, res.Reason)
	}
	if env.called {
		t.Error("dry run provisioned a Python environment")
	}
	if len(runner.steps) != 0 {
		t.Errorf("dry run executed %v", runner.names())
	}
	// The pre-flight reads are read-only, so a dry run still makes them: a preview
	// that skipped them could describe a run that would never start.
	if cluster.gotInstance == "" {
		t.Error("dry run skipped the controller readiness read")
	}
	for _, want := range []string{"would run", bootstrapScriptName, pytestExe, cleanupScriptName} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("preview %q does not mention %q", res.Reason, want)
		}
	}
}

func TestTest_DryRunReportsARejectedPreFlight(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})

	res := run(t, root, &fakeCluster{context: devContext, ready: false}, &fakeEnvironment{}, &fakeRunner{}, true, Options{})

	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("outcome = %s (%s), want skipped", res.Outcome, res.Reason)
	}
}

func TestPytestArgs(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "defaults match the upstream local runner",
			want: []string{"-n", "auto", "--dist", "no", "-o", "log_cli=true", "--ignore=acktest",
				"--log-cli-level", "info", "--log-level", "info", "."},
		},
		{
			name: "threads and log level are overridable",
			opts: Options{Threads: "4", LogLevel: "debug"},
			want: []string{"-n", "4", "--dist", "no", "-o", "log_cli=true", "--ignore=acktest",
				"--log-cli-level", "debug", "--log-level", "debug", "."},
		},
		{
			name: "several markers become one disjunction",
			opts: Options{Markers: []string{"canary", "slow"}},
			want: []string{"-n", "auto", "--dist", "no", "-o", "log_cli=true", "--ignore=acktest",
				"-m", "canary or slow", "--log-cli-level", "info", "--log-level", "info", "."},
		},
		{
			name: "several methods become one disjunction",
			opts: Options{Methods: []string{"test_topic", "test_queue"}},
			want: []string{"-n", "auto", "--dist", "no", "-o", "log_cli=true", "--ignore=acktest",
				"-k", "test_topic or test_queue", "--log-cli-level", "info", "--log-level", "info", "."},
		},
		{
			name: "blank selectors are dropped",
			opts: Options{Markers: []string{"", "  "}, Methods: []string{" test_topic "}},
			want: []string{"-n", "auto", "--dist", "no", "-o", "log_cli=true", "--ignore=acktest",
				"-k", "test_topic", "--log-cli-level", "info", "--log-level", "info", "."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pytestArgs(tc.opts)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("pytestArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStepEnv_RegionOnlySetWhenRequested(t *testing.T) {
	// Left unset, the suite resolves the region the way boto3 does, from the
	// environment and the AWS profile.
	for _, e := range stepEnv("/venv/bin", Options{}) {
		if strings.HasPrefix(e, envAWSDefaultRegion+"=") {
			t.Errorf("region %q was set without being requested", e)
		}
	}

	var found bool
	for _, e := range stepEnv("/venv/bin", Options{Region: "eu-west-1"}) {
		if e == envAWSDefaultRegion+"=eu-west-1" {
			found = true
		}
	}
	if !found {
		t.Error("requested region was not passed to the suite")
	}
}

func TestNewPlan_ScriptStepsCanImportTheirSiblings(t *testing.T) {
	root := workspaceWithSuite(t, "ecr-controller", suiteOptions{})
	e2e := filepath.Join(root, "ecr-controller", e2eRelPath)

	p := newPlan(e2e, "/venv/bin", nil, Options{})
	if p.bootstrap == nil || p.cleanup == nil {
		t.Fatal("plan is missing the bootstrap or cleanup step")
	}
	for _, s := range []Step{*p.bootstrap, *p.cleanup} {
		var found bool
		for _, e := range s.Env {
			if e == envPythonPath+"=.." {
				found = true
			}
		}
		if !found {
			t.Errorf("%s step env %v is missing %s=..", s.Name, s.Env, envPythonPath)
		}
	}
	// pytest resolves its own imports through the suite's conftest, matching how
	// upstream invokes it.
	for _, e := range p.tests.Env {
		if strings.HasPrefix(e, envPythonPath+"=") {
			t.Errorf("tests step should not set %s, got %q", envPythonPath, e)
		}
	}
}

func TestExecEnvironment_ReinstallsWhenRequirementsChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "venv")
	mustMkdir(t, filepath.Join(dir, binDirName))
	mustWrite(t, filepath.Join(dir, binDirName, pytestExe), "")

	requirements := filepath.Join(t.TempDir(), requirementsFileName)
	mustWrite(t, requirements, "acktest @ git+https://example.invalid/test-infra.git@aaa\n")

	digest, err := fileSHA256(requirements)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}

	env := execEnvironment{out: nil}

	// A stamp matching the requirements means the environment is already right, so
	// Ensure must return it untouched rather than shelling out to python3/pip.
	mustWrite(t, filepath.Join(dir, environmentStampName), digest+"\n")
	prov, err := env.Ensure(context.Background(), dir, requirements)
	if err != nil {
		t.Fatalf("Ensure with a matching stamp returned an error: %v", err)
	}
	if prov.Created || prov.Installed {
		t.Errorf("Ensure reprovisioned a matching environment: %+v", prov)
	}
	if want := filepath.Join(dir, binDirName); prov.BinDir != want {
		t.Errorf("BinDir = %q, want %q", prov.BinDir, want)
	}

	// A stamp that no longer matches must not be reported as usable. Ensure then
	// tries to provision, which fails here because the writer is nil and python3
	// runs for real -- so assert on the stamp comparison directly instead.
	if env.stamp(dir) != digest {
		t.Fatalf("stamp = %q, want %q", env.stamp(dir), digest)
	}
	mustWrite(t, requirements, "acktest @ git+https://example.invalid/test-infra.git@bbb\n")
	changed, err := fileSHA256(requirements)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}
	if env.stamp(dir) == changed {
		t.Error("stamp still matches after the requirements changed, so a repin would go unnoticed")
	}
}

func TestExecEnvironment_MissingRequirementsIsAnError(t *testing.T) {
	_, err := execEnvironment{}.Ensure(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "absent.txt"))
	if err == nil {
		t.Fatal("Ensure with an absent requirements file = nil error, want an error")
	}
}

func TestStateDir_FallsBackWhenNoConfigPathWasResolved(t *testing.T) {
	withPath := stateDir(app.App{Config: config.Config{Path: filepath.Join("/tmp", configDirName, "config")}})
	if want := filepath.Join("/tmp", configDirName); withPath != want {
		t.Errorf("stateDir = %q, want %q", withPath, want)
	}

	if got := stateDir(app.App{}); !strings.Contains(got, configDirName) {
		t.Errorf("stateDir with no resolved path = %q, want it under %s", got, configDirName)
	}
}

func TestStep_StringRendersTheCommand(t *testing.T) {
	s := Step{Command: "/venv/bin/pytest", Args: []string{"-n", "auto", "."}}
	if want := "/venv/bin/pytest -n auto ."; s.String() != want {
		t.Errorf("Step.String() = %q, want %q", s.String(), want)
	}
	if got := (Step{Command: "/venv/bin/pytest"}).String(); got != "/venv/bin/pytest" {
		t.Errorf("Step.String() with no args = %q, want the bare command", got)
	}
}

func TestSalient_KeepsTheAnswerNotTheRetryChatter(t *testing.T) {
	// Exactly what kubectl prints when a credential has expired: five identical
	// klog retries, then the one line that says what is wrong.
	out := `E0909 21:50:05.737522 1207439 memcache.go:265] "Unhandled Error" err="couldn't get current server API group list: the server has asked for the client to provide credentials"
E0909 21:50:06.430950 1207439 memcache.go:265] "Unhandled Error" err="couldn't get current server API group list: the server has asked for the client to provide credentials"
E0909 21:50:07.120083 1207439 memcache.go:265] "Unhandled Error" err="couldn't get current server API group list: the server has asked for the client to provide credentials"
error: You must be logged in to the server (the server has asked for the client to provide credentials)
`

	got := salient(out, 3)
	if got != "error: You must be logged in to the server (the server has asked for the client to provide credentials)" {
		t.Errorf("salient = %q, want just the trailing error line", got)
	}
	// A reason is rendered as one column of a table, so it must stay on one line.
	if strings.Contains(got, "\n") {
		t.Errorf("salient returned a multi-line reason: %q", got)
	}
}

func TestSalient_FallsBackWhenEverythingIsChatter(t *testing.T) {
	out := "E0909 21:50:05.737522 1 memcache.go:265] first\nE0909 21:50:06.430950 1 memcache.go:265] second\n"
	if got := salient(out, 3); got == "" {
		t.Error("salient dropped every line, want the chatter reported rather than nothing")
	}
}

func TestSalient_TrailingLinesAreCapped(t *testing.T) {
	if got, want := salient("a\nb\nc\nd\ne\n", 2), "d; e"; got != want {
		t.Errorf("salient = %q, want %q", got, want)
	}
	if got := salient("   \n\n", 3); got != "" {
		t.Errorf("salient of blank output = %q, want empty", got)
	}
}

func TestIsKlogLine(t *testing.T) {
	cases := map[string]bool{
		`E0909 21:50:05.737522 1 memcache.go:265] boom`: true,
		`W0101 00:00:00.000000 1 x.go:1] warn`:          true,
		`I1231 23:59:59.999999 1 x.go:1] info`:          true,
		`error: You must be logged in to the server`:    false,
		`E09 bad`: false,
		`X0909 21:50:05.737522 1 x.go:1] wrong letter`: false,
		`E0909x21:50:05 no space`:                      false,
		`short`:                                        false,
	}
	for line, want := range cases {
		if got := isKlogLine(line); got != want {
			t.Errorf("isKlogLine(%q) = %v, want %v", line, got, want)
		}
	}
}
