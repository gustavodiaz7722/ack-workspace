// Package tester runs a service controller's end-to-end test suite against the
// controller already running on the development cluster:
//
//  1. locate the controller and its test/e2e suite in the workspace,
//  2. confirm the kubeconfig selects the development cluster, that the cluster
//     answers, and that the controller under test is actually running on it,
//  3. ensure a Python environment holding the suite's pinned requirements,
//  4. create the suite's AWS fixtures (service_bootstrap.py),
//  5. run pytest over the suite, and
//  6. tear those fixtures down again (service_cleanup.py).
//
// Like the builder and the deployer, the tester never touches git history: it
// tests whatever the controller repository currently has checked out. Every
// execution problem is captured as a failed Result rather than returned
// out-of-band, and a dry run reports the commands it would run while changing
// nothing.
//
// # Why this does not call test-infra's run-e2e-tests.sh
//
// test-infra's scripts/run-e2e-tests.sh is the canonical ACK e2e entrypoint, and
// delegating to it would normally be the right call — it is what `build` and
// `deploy` do with their code-generator scripts. It is not used here because it
// sources kind.sh, controller-setup.sh and pytest-image-runner.sh
// unconditionally at load time, and each of those runs its own binary check on
// being sourced. Running it therefore requires kind, kustomize, docker, jq,
// uuidgen and yq, plus a test_config.yaml carrying an assumed-role ARN, before
// it will start — every one of which exists to serve the path that creates a KIND
// cluster and installs a controller into it. `ack-workspace deploy` already owns
// getting a controller onto a cluster, so that path is deliberately never taken,
// and the work left over is the three commands scripts/pytest-local-runner.sh
// runs: bootstrap, pytest, cleanup.
//
// Those three invocations are reproduced here rather than shelled out to, which
// trades a small amount of duplication for a prerequisite set of kubectl and
// python3. The pytest invocation is kept identical to the upstream one so a run
// here behaves the same as a run there; it is the one thing in this package that
// must be checked against upstream if that script changes.
//
// # Why the cluster is not selectable
//
// The suite runs against whatever cluster the kubeconfig selects, which is the
// one thing about a test run that cannot be inferred from its output: a suite
// pointed at the wrong cluster fails in ways that look like product bugs. So the
// selected context must name the development cluster (deployer.ClusterName) that
// `deploy` repoints the kubeconfig at, and a context naming anything else is
// skipped rather than run. This is the same stance `deploy` takes for the same
// reason, and it is why there is no flag to override it.
//
// # Why the Python environment lives outside the controller checkout
//
// The documented setup creates the virtual environment at
// <controller>/test/e2e/.venv, but a controller's .gitignore does not cover it,
// so creating one leaves the checkout with untracked files — which makes `status`
// report the repository dirty and makes `deploy` refuse it outright. The tester
// therefore creates its environments under $HOME/.ack-workspace/venvs/<name>,
// leaving the checkout pristine. An existing test/e2e/.venv is still used when
// one is found, because a developer who already set one up should not pay for a
// second copy of the same dependencies; it is used, never created.
//
// # Why the outcome is pytest's alone
//
// The Result reflects the pytest exit code and nothing else, matching upstream,
// so "the command failed" always means "a test failed". Teardown still runs
// after a failed run (and after a failed bootstrap, which may have created part
// of its fixtures), and a teardown that itself fails is reported in the Result's
// reason — naming the fixtures that may remain — rather than folded into the
// outcome.
package tester

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	// controllerSuffix is the conventional suffix of every service controller
	// repository name. A bare alias ("ecr") and its full form ("ecr-controller")
	// both normalize to the same repository.
	controllerSuffix = "-controller"
	// requirementsFileName is the suite's pinned dependency list, which also
	// identifies the Python environment's contents (see environmentStampName).
	requirementsFileName = "requirements.txt"
	// bootstrapScriptName creates the AWS fixtures the suite expects to exist
	// before any test runs. Not every controller has one.
	bootstrapScriptName = "service_bootstrap.py"
	// cleanupScriptName deletes the fixtures bootstrapScriptName created.
	cleanupScriptName = "service_cleanup.py"

	// releasePrefix is prepended to the controller name to form the Helm release
	// name the deployer installs under. It must match the deployer's own prefix:
	// the readiness check selects the controller Deployment by the
	// app.kubernetes.io/instance label, whose value is that release name.
	releasePrefix = "ack-"

	// configDirName is the directory under $HOME that holds ack-workspace state.
	// It matches internal/config's own constant, and is only used as a fallback
	// when the resolved configuration carries no path.
	configDirName = ".ack-workspace"
	// environmentsDirName is the directory under the ack-workspace state directory
	// that holds the managed Python environments, one per controller.
	environmentsDirName = "venvs"
	// localEnvironmentDirName is the in-checkout virtual environment the ACK
	// contributor docs tell developers to create. It is used when present but
	// never created (see the package doc).
	localEnvironmentDirName = ".venv"
	// environmentStampName records the SHA-256 of the requirements.txt an
	// environment was installed from, so a controller that repins acktest gets a
	// reinstall instead of a stale environment.
	environmentStampName = ".ack-requirements-sha256"
	// binDirName is the subdirectory of a virtual environment holding its
	// executables.
	binDirName = "bin"

	// pytestExe, pythonExe and pipExe are the executables invoked out of the
	// environment's bin directory. They are addressed absolutely so a run cannot
	// pick up a same-named executable from the ambient PATH.
	pytestExe = "pytest"
	pythonExe = "python"
	pipExe    = "pip"
	// venvPython is the interpreter used to create an environment. It is resolved
	// from the PATH because it necessarily predates the environment.
	venvPython = "python3"

	// defaultThreads is the pytest-xdist worker count, matching upstream's
	// PYTEST_NUM_THREADS default.
	defaultThreads = "auto"
	// defaultLogLevel is the pytest log level, matching upstream's
	// PYTEST_LOG_LEVEL default.
	defaultLogLevel = "info"

	// Environment variables set for the suite's own processes.

	// envPath is overridden so the environment's executables come first, for the
	// benefit of anything the suite shells out to.
	envPath = "PATH"
	// envPythonPath lets the bootstrap and cleanup scripts import the suite's
	// sibling modules, matching how upstream invokes them.
	envPythonPath = "PYTHONPATH"
	// envAWSDefaultRegion pins the region the suite creates AWS resources in. It
	// is set only when a region is requested explicitly; otherwise boto3's own
	// resolution (environment, then profile, then us-west-2) is left alone.
	envAWSDefaultRegion = "AWS_DEFAULT_REGION"

	// clusterRequestTimeout bounds every read against the cluster so an
	// unreachable endpoint fails promptly instead of hanging the pre-flight.
	clusterRequestTimeout = "30s"
)

// e2eRelPath is the suite directory relative to a controller's root.
var e2eRelPath = filepath.Join("test", "e2e")

// UsageError is a typed argument/validation error returned by Test before any
// test is attempted (for example a missing service identifier). The cmd layer
// maps it to a distinct usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Cluster reports what the local kubeconfig selects and what is running there.
// It is the seam through which the real kubectl invocations are replaced in
// tests.
type Cluster interface {
	// Context returns the name of the currently selected kubeconfig context.
	Context(ctx context.Context) (string, error)
	// Reachable reports whether the selected cluster answers a read.
	Reachable(ctx context.Context) error
	// ControllerReady reports whether a Deployment labelled with the given Helm
	// release instance has at least one ready replica in namespace.
	ControllerReady(ctx context.Context, namespace, instance string) (bool, error)
}

// Environment provisions the Python environment the suite runs in. It is the
// seam through which the real venv/pip invocations are replaced in tests.
type Environment interface {
	// Ensure returns the provisioned environment rooted at dir, creating it and
	// installing requirements when it is absent or was installed from a different
	// requirements file.
	Ensure(ctx context.Context, dir, requirements string) (Provision, error)
}

// Provision describes the state of a Python environment after Ensure.
type Provision struct {
	// BinDir holds the environment's executables.
	BinDir string
	// Created reports whether the environment itself was created by this call.
	Created bool
	// Installed reports whether requirements were installed by this call, which
	// happens both on creation and when the pinned requirements changed.
	Installed bool
}

// Runner executes one step of the suite. It is the seam through which the real
// os/exec invocations are replaced in tests.
type Runner interface {
	// Run executes step, streaming its output as it is produced. An end-to-end
	// suite runs for tens of minutes, so its output is of no use buffered.
	Run(ctx context.Context, step Step) error
}

// Step is one command in the suite's run: the bootstrap, the tests, or the
// teardown.
type Step struct {
	// Name labels the step in progress output and in error messages.
	Name string
	// Dir is the working directory the command runs in.
	Dir string
	// Command is the executable, given as an absolute path.
	Command string
	// Args are the command's arguments.
	Args []string
	// Env holds "KEY=VALUE" entries appended to the inherited process
	// environment, where a repeated key overrides the inherited value.
	Env []string
}

// String renders the step as the shell command it stands for, for previews and
// progress output.
func (s Step) String() string {
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}

// Options controls test behavior. All fields are optional.
type Options struct {
	// Markers selects tests by pytest marker. Several are combined into one
	// disjunction ("canary or slow").
	Markers []string
	// Methods selects tests by name, using pytest's -k expression syntax. Several
	// are combined into one disjunction.
	Methods []string
	// Region pins the AWS region the suite creates its resources in. When empty,
	// boto3's own resolution is left in place.
	Region string
	// Threads is the pytest-xdist worker count. Empty means defaultThreads.
	Threads string
	// LogLevel is the pytest log level. Empty means defaultLogLevel.
	LogLevel string
	// SkipCleanup leaves the suite's AWS fixtures in place after the run, for
	// inspecting the state a failure left behind. Those resources then cost money
	// until they are deleted by hand.
	SkipCleanup bool
}

// plan is the ordered set of commands one run consists of.
type plan struct {
	// bootstrap creates the suite's fixtures. It is nil when the suite has no
	// bootstrap script.
	bootstrap *Step
	// tests is the pytest invocation.
	tests Step
	// cleanup deletes the fixtures. It is nil when the suite has no cleanup script
	// or teardown was skipped.
	cleanup *Step
}

// steps returns the plan's commands in execution order, for previewing.
func (p plan) steps() []Step {
	steps := make([]Step, 0, 3)
	if p.bootstrap != nil {
		steps = append(steps, *p.bootstrap)
	}
	steps = append(steps, p.tests)
	if p.cleanup != nil {
		steps = append(steps, *p.cleanup)
	}
	return steps
}

// Tester runs a controller's end-to-end suite.
type Tester struct {
	cluster Cluster
	env     Environment
	runner  Runner
	out     io.Writer
}

// New returns a Tester wired to the production toolchain (kubectl for the
// cluster reads, python3/pip for the environment, and the environment's own
// python and pytest for the suite), reporting progress on os.Stdout.
// Constructing it performs no external work; that happens only when Test runs.
func New() *Tester {
	return NewWithWriter(os.Stdout)
}

// NewWithWriter returns a production Tester that streams suite output and
// progress to out. A nil out discards them.
func NewWithWriter(out io.Writer) *Tester {
	if out == nil {
		out = io.Discard
	}
	return &Tester{
		cluster: execCluster{},
		env:     execEnvironment{out: out},
		runner:  execRunner{out: out},
		out:     out,
	}
}

// NewWith returns a Tester backed by the supplied collaborators. It is intended
// for tests that need to script cluster, environment, and step behavior without
// invoking the real toolchain.
func NewWith(c Cluster, e Environment, r Runner, out io.Writer) *Tester {
	if out == nil {
		out = io.Discard
	}
	return &Tester{cluster: c, env: e, runner: r, out: out}
}

// Test runs the end-to-end suite of the controller named by service and returns
// a single-result Summary recording the outcome (tested, skipped, or failed).
//
// The returned error is non-nil only for a pre-flight validation failure (an
// empty service identifier); all execution problems, including a failing test,
// are captured as a Result so the caller renders a uniform summary.
func (t *Tester) Test(ctx context.Context, ap app.App, service string, opts Options) (workspace.Summary, error) {
	alias := strings.TrimSuffix(strings.TrimSpace(service), controllerSuffix)
	if alias == "" {
		return workspace.Summary{}, &UsageError{Msg: "a service identifier is required (for example: ecr or ecr-controller)"}
	}

	result := t.process(ctx, ap, alias, opts)
	return workspace.Summary{Results: []workspace.Result{result}}, nil
}

// process runs the full flow for one controller and returns its terminal
// Result. It never returns an error out-of-band: every failure is captured into
// a failed Result.
func (t *Tester) process(ctx context.Context, ap app.App, alias string, opts Options) workspace.Result {
	name := alias + controllerSuffix
	controllerPath := filepath.Join(ap.Config.WorkspaceRoot, name)
	e2ePath := filepath.Join(controllerPath, e2eRelPath)
	requirements := filepath.Join(e2ePath, requirementsFileName)

	// Pre-flight: the controller must already be in the workspace with a suite to
	// run. Testing neither forks nor clones.
	if !dirExists(controllerPath) {
		return failed(name, fmt.Errorf("controller %s not found at %s; add it first with `ack-workspace add %s`", name, controllerPath, alias))
	}
	if !isGitRepo(controllerPath) {
		return failed(name, fmt.Errorf("%s is not a git repository", controllerPath))
	}
	// Around a tenth of the controllers carry no suite at all. That is a property
	// of the repository rather than a mistake by the caller, so it is skipped
	// rather than failed.
	if !dirExists(e2ePath) {
		return skipped(name, fmt.Sprintf("no end-to-end suite: %s does not exist", e2ePath))
	}
	if !fileExists(requirements) {
		return failed(name, fmt.Errorf("suite at %s has no %s, so its Python dependencies cannot be resolved", e2ePath, requirementsFileName))
	}

	// Where the suite runs is the one thing its output cannot tell you, so it is
	// established before anything is provisioned or created. These are read-only,
	// which is why a dry run makes them too: a preview that skipped them could
	// describe a run that would not actually start.
	kubeContext, err := t.checkCluster(ctx, name, alias)
	if err != nil {
		return *err.result
	}

	// An existing in-checkout environment is used as-is; otherwise the managed one
	// outside the checkout is (see the package doc).
	envDir, managed := t.environmentDir(ap, name, e2ePath)

	if ap.DryRun {
		return t.preview(name, kubeContext, envDir, managed, e2ePath, opts)
	}

	prov, err2 := t.env.Ensure(ctx, envDir, requirements)
	if err2 != nil {
		return failed(name, fmt.Errorf("preparing the Python environment at %s: %w", envDir, err2))
	}

	p := newPlan(e2ePath, prov.BinDir, stepEnv(prov.BinDir, opts), opts)

	fmt.Fprintf(t.out, "Running the %s end-to-end suite on %s (Python environment %s)\n", name, kubeContext, envDir)

	if p.bootstrap != nil {
		if err := t.runner.Run(ctx, *p.bootstrap); err != nil {
			// A bootstrap that failed part way through may already have created some
			// fixtures, so teardown still runs. The tests do not: every one of them
			// would fail against an incomplete fixture set, burying the real cause.
			return failed(name, fmt.Errorf("creating the suite's fixtures: %w%s", err, t.teardown(ctx, p.cleanup)))
		}
	}

	testErr := t.runner.Run(ctx, p.tests)
	note := t.teardown(ctx, p.cleanup)

	if testErr != nil {
		return failed(name, fmt.Errorf("%s%s", testErr, note))
	}
	return succeeded(name, fmt.Sprintf("%s end-to-end suite passed on %s%s", name, kubeContext, note))
}

// resultError carries a terminal Result out of a helper that can end the run,
// keeping the pre-flight readable without giving each check its own return
// signature.
type resultError struct{ result *workspace.Result }

func (e *resultError) Error() string { return e.result.Reason }

// checkCluster establishes that the kubeconfig selects the development cluster,
// that the cluster answers, and that the controller under test is running on
// it. It returns the selected context name, or a resultError carrying the
// terminal Result.
//
// An unreachable cluster is a failure; a context pointing somewhere else, or a
// controller that is not running, is a skip — in both cases nothing was
// attempted and the fix is a command the caller runs, not an error in the suite.
func (t *Tester) checkCluster(ctx context.Context, name, alias string) (string, *resultError) {
	kubeContext, err := t.cluster.Context(ctx)
	if err != nil {
		r := failed(name, fmt.Errorf("reading the current kubeconfig context: %w", err))
		return "", &resultError{result: &r}
	}
	kubeContext = strings.TrimSpace(kubeContext)
	if kubeContext == "" {
		r := skipped(name, fmt.Sprintf("no kubeconfig context is selected; run `ack-workspace deploy %s` to point it at the %s cluster", alias, deployer.ClusterName))
		return "", &resultError{result: &r}
	}
	// The context name a deploy leaves behind is the cluster ARN, so the cluster
	// name is matched as a substring rather than compared outright.
	if !strings.Contains(kubeContext, deployer.ClusterName) {
		r := skipped(name, fmt.Sprintf(
			"kubeconfig context %s does not name the %s development cluster; run `ack-workspace deploy %s` to build the controller and point the kubeconfig at it",
			kubeContext, deployer.ClusterName, alias))
		return "", &resultError{result: &r}
	}

	if err := t.cluster.Reachable(ctx); err != nil {
		r := failed(name, fmt.Errorf("cluster selected by context %s did not answer: %w", kubeContext, err))
		return "", &resultError{result: &r}
	}

	// Without the controller running, every test fails on a timeout and the report
	// says nothing about why. The state is readable right now, so it is read
	// rather than discovered half an hour later.
	instance := releasePrefix + name
	ready, err := t.cluster.ControllerReady(ctx, deployer.Namespace, instance)
	if err != nil {
		r := failed(name, fmt.Errorf("checking whether %s is running in namespace %s: %w", instance, deployer.Namespace, err))
		return "", &resultError{result: &r}
	}
	if !ready {
		r := skipped(name, fmt.Sprintf(
			"%s has no ready replica in namespace %s on %s; deploy it first with `ack-workspace deploy %s`",
			instance, deployer.Namespace, kubeContext, alias))
		return "", &resultError{result: &r}
	}

	return kubeContext, nil
}

// preview computes the run for a dry-run without provisioning an environment or
// executing anything.
func (t *Tester) preview(name, kubeContext, envDir string, managed bool, e2ePath string, opts Options) workspace.Result {
	p := newPlan(e2ePath, filepath.Join(envDir, binDirName), nil, opts)

	commands := make([]string, 0, 3)
	for _, s := range p.steps() {
		commands = append(commands, s.String())
	}

	envState := "reusing the existing environment"
	if !dirExists(envDir) {
		envState = "creating the environment"
		if managed {
			envState += " (outside the controller checkout, so the checkout stays clean for `deploy`)"
		}
	}

	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSucceeded, Reason: fmt.Sprintf(
		"would run the %s end-to-end suite on %s in %s, %s at %s: %s",
		name, kubeContext, e2ePath, envState, envDir, strings.Join(commands, "; "))}
}

// teardown runs the cleanup step, if there is one, and returns a note to append
// to the Result's reason: empty when it succeeded or was not run, and otherwise
// a warning naming what may be left behind. A teardown failure never changes the
// outcome (see the package doc).
func (t *Tester) teardown(ctx context.Context, cleanup *Step) string {
	if cleanup == nil {
		return ""
	}
	if err := t.runner.Run(ctx, *cleanup); err != nil {
		return fmt.Sprintf("; WARNING: deleting the suite's fixtures failed (%v), so AWS resources it created may still exist and still cost money", err)
	}
	return ""
}

// environmentDir returns the directory the suite's Python environment lives in,
// and whether it is one the tester manages. An in-checkout .venv is preferred
// when it already exists; otherwise the managed location under the
// ack-workspace state directory is used (see the package doc).
func (t *Tester) environmentDir(ap app.App, name, e2ePath string) (dir string, managed bool) {
	if local := filepath.Join(e2ePath, localEnvironmentDirName); dirExists(local) {
		return local, false
	}
	return filepath.Join(stateDir(ap), environmentsDirName, name), true
}

// stateDir returns the ack-workspace state directory, derived from the resolved
// configuration file's location so it tracks whatever $HOME resolution produced
// it. It falls back to $HOME/.ack-workspace when no path was resolved.
func stateDir(ap app.App) string {
	if p := strings.TrimSpace(ap.Config.Path); p != "" {
		return filepath.Dir(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return configDirName
	}
	return filepath.Join(home, configDirName)
}

// newPlan assembles the ordered commands for one run. The bootstrap and cleanup
// steps are included only when the suite actually has those scripts, and cleanup
// only when it was not skipped.
func newPlan(e2ePath, binDir string, env []string, opts Options) plan {
	p := plan{
		tests: Step{
			Name:    "tests",
			Dir:     e2ePath,
			Command: filepath.Join(binDir, pytestExe),
			Args:    pytestArgs(opts),
			Env:     env,
		},
	}

	// The bootstrap and cleanup scripts import their siblings by module name, so
	// they need the suite's parent directory importable, exactly as upstream's
	// runner arranges.
	scriptEnv := append([]string{envPythonPath + "=.."}, env...)

	if fileExists(filepath.Join(e2ePath, bootstrapScriptName)) {
		p.bootstrap = &Step{
			Name:    "bootstrap",
			Dir:     e2ePath,
			Command: filepath.Join(binDir, pythonExe),
			Args:    []string{bootstrapScriptName},
			Env:     scriptEnv,
		}
	}
	if !opts.SkipCleanup && fileExists(filepath.Join(e2ePath, cleanupScriptName)) {
		p.cleanup = &Step{
			Name:    "teardown",
			Dir:     e2ePath,
			Command: filepath.Join(binDir, pythonExe),
			Args:    []string{cleanupScriptName},
			Env:     scriptEnv,
		}
	}
	return p
}

// pytestArgs builds the pytest invocation. It mirrors the one in test-infra's
// scripts/pytest-local-runner.sh so a run here behaves the same as a run there,
// with one deliberate difference: several markers or methods are combined into a
// single disjunction rather than passed as repeated -m/-k flags, which pytest
// resolves by keeping only the last.
func pytestArgs(opts Options) []string {
	threads := defaultThreads
	if v := strings.TrimSpace(opts.Threads); v != "" {
		threads = v
	}
	level := defaultLogLevel
	if v := strings.TrimSpace(opts.LogLevel); v != "" {
		level = v
	}

	// --ignore=acktest excludes the copy of test-infra that upstream's container
	// image lands in the suite directory, so acktest's own unit tests are never
	// collected. It is inert outside that image and kept for parity.
	args := []string{"-n", threads, "--dist", "no", "-o", "log_cli=true", "--ignore=acktest"}
	if expr := disjunction(opts.Markers); expr != "" {
		args = append(args, "-m", expr)
	}
	if expr := disjunction(opts.Methods); expr != "" {
		args = append(args, "-k", expr)
	}
	return append(args, "--log-cli-level", level, "--log-level", level, ".")
}

// disjunction joins the non-empty values into a single pytest selector
// expression, returning "" when there is nothing to select on.
func disjunction(values []string) string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			kept = append(kept, v)
		}
	}
	return strings.Join(kept, " or ")
}

// stepEnv builds the environment overrides shared by every step: the Python
// environment's executables ahead of the inherited PATH, and the region when one
// was requested.
func stepEnv(binDir string, opts Options) []string {
	env := []string{envPath + "=" + binDir + string(os.PathListSeparator) + os.Getenv(envPath)}
	if region := strings.TrimSpace(opts.Region); region != "" {
		env = append(env, envAWSDefaultRegion+"="+region)
	}
	return env
}

// execCluster is the production Cluster, reading cluster state with kubectl.
// Every call is bounded by clusterRequestTimeout so an unreachable endpoint
// fails promptly instead of hanging.
type execCluster struct{}

// Context returns the selected kubeconfig context. It reads local configuration
// only, so it carries no request timeout.
func (execCluster) Context(ctx context.Context) (string, error) {
	out, err := runCombined(exec.CommandContext(ctx, "kubectl", "config", "current-context"))
	if err != nil {
		return "", annotate("kubectl config current-context", out, err)
	}
	return strings.TrimSpace(out), nil
}

// Reachable confirms the selected cluster answers a read.
func (execCluster) Reachable(ctx context.Context) error {
	out, err := runCombined(exec.CommandContext(ctx, "kubectl", "get", "nodes",
		"-o", "name", "--request-timeout="+clusterRequestTimeout))
	if err != nil {
		return annotate("kubectl get nodes", out, err)
	}
	return nil
}

// ControllerReady reports whether a Deployment labelled with the given Helm
// release instance has a ready replica.
//
// The Deployment is selected by label rather than by name because the chart
// names it after both the release and the chart ("ack-ecr-controller-ecr-chart"),
// which the caller would otherwise have to reconstruct. The jsonpath ranges over
// items so an empty list yields empty output rather than an index error, letting
// "no such Deployment" and "not ready yet" travel the same path.
func (execCluster) ControllerReady(ctx context.Context, namespace, instance string) (bool, error) {
	out, err := runCombined(exec.CommandContext(ctx, "kubectl", "get", "deployment",
		"-n", namespace,
		"-l", "app.kubernetes.io/instance="+instance,
		"-o", `jsonpath={range .items[*]}{.status.readyReplicas}{"\n"}{end}`,
		"--request-timeout="+clusterRequestTimeout))
	if err != nil {
		return false, annotate("kubectl get deployment -l app.kubernetes.io/instance="+instance, out, err)
	}
	for _, field := range strings.Fields(out) {
		if n, err := strconv.Atoi(field); err == nil && n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// execEnvironment is the production Environment, provisioning a virtual
// environment with python3 and installing the suite's requirements with pip.
type execEnvironment struct {
	out io.Writer
}

// Ensure creates the environment at dir and installs requirements when it does
// not exist, is unusable, or was installed from a different requirements file.
// An environment already matching the requirements is returned untouched, which
// is what makes a repeat run start in seconds.
func (e execEnvironment) Ensure(ctx context.Context, dir, requirements string) (Provision, error) {
	binDir := filepath.Join(dir, binDirName)

	want, err := fileSHA256(requirements)
	if err != nil {
		return Provision{}, fmt.Errorf("reading %s: %w", requirements, err)
	}

	usable := fileExists(filepath.Join(binDir, pytestExe))
	if usable && e.stamp(dir) == want {
		return Provision{BinDir: binDir}, nil
	}

	created := !dirExists(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Provision{}, fmt.Errorf("creating %s: %w", filepath.Dir(dir), err)
	}

	// `python3 -m venv` is idempotent, so it also repairs an environment that
	// exists but lost its executables.
	fmt.Fprintf(e.out, "==> preparing the Python environment at %s\n", dir)
	if out, err := runCombined(exec.CommandContext(ctx, venvPython, "-m", "venv", dir)); err != nil {
		return Provision{}, annotate(venvPython+" -m venv "+dir, out, err)
	}

	// setuptools is installed alongside the pinned requirements because the ACK
	// suites import pkg_resources, which Python 3.12 and later no longer ship.
	install := exec.CommandContext(ctx, filepath.Join(binDir, pipExe), "install", "-r", requirements, "setuptools")
	install.Stdout = e.out
	install.Stderr = e.out
	if err := install.Run(); err != nil {
		return Provision{}, fmt.Errorf("pip install -r %s: %w", requirements, err)
	}

	if err := os.WriteFile(filepath.Join(dir, environmentStampName), []byte(want+"\n"), 0o644); err != nil {
		return Provision{}, fmt.Errorf("recording the installed requirements in %s: %w", dir, err)
	}

	return Provision{BinDir: binDir, Created: created, Installed: true}, nil
}

// stamp returns the requirements digest recorded in the environment at dir, or
// "" when there is none. An unreadable stamp is indistinguishable from a missing
// one on purpose: both mean "reinstall", which is always safe.
func (execEnvironment) stamp(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, environmentStampName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// execRunner is the production Runner. It streams each step's output as it
// arrives, which for a suite that runs for tens of minutes is the only useful
// way to present it.
type execRunner struct {
	out io.Writer
}

// Run executes step with its output streamed to the runner's writer.
func (r execRunner) Run(ctx context.Context, step Step) error {
	fmt.Fprintf(r.out, "==> %s: %s\n", step.Name, step)

	cmd := exec.CommandContext(ctx, step.Command, step.Args...)
	cmd.Dir = step.Dir
	cmd.Env = append(os.Environ(), step.Env...)
	cmd.Stdout = r.out
	cmd.Stderr = r.out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s step failed (%s): %w", step.Name, step, err)
	}
	return nil
}

// runCombined runs cmd and returns its combined output. It is used for the short
// cluster and environment reads whose output is only wanted on failure.
func runCombined(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// annotate wraps err with the command that produced it and, when there was any,
// the salient part of its output (see salient). The result stays on one line
// because it becomes a Result's reason, which is rendered as one column of a
// table.
func annotate(label, out string, err error) error {
	if s := salient(out, 3); s != "" {
		return fmt.Errorf("%s: %w: %s", label, err, s)
	}
	return fmt.Errorf("%s: %w", label, err)
}

// salient reduces command output to at most n lines worth reporting, joined with
// "; ".
//
// It takes the LAST lines rather than the first, and drops klog-formatted
// progress lines, because that is where kubectl puts the answer: an expired
// credential prints five identical "couldn't get current server API group list"
// retries and then the one line that matters, "error: You must be logged in to
// the server". Reporting the head would report only the noise.
func salient(out string, n int) string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}

	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !isKlogLine(line) {
			kept = append(kept, line)
		}
	}
	// Output that is nothing but klog lines still beats reporting nothing at all.
	if len(kept) == 0 {
		kept = lines
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "; ")
}

// isKlogLine reports whether line carries a klog severity/timestamp header
// ("E0909 21:50:18.415171 ..."), which kubectl uses for the retry chatter that
// precedes its actual error.
func isKlogLine(line string) bool {
	if len(line) < 6 {
		return false
	}
	if !strings.ContainsRune("IWEF", rune(line[0])) {
		return false
	}
	for _, c := range line[1:5] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return line[5] == ' '
}

// fileSHA256 returns the hex-encoded SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// succeeded builds a successful (OutcomeSucceeded) Result with the given reason.
func succeeded(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSucceeded, Reason: reason}
}

// skipped builds a skipped Result with the given reason.
func skipped(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSkipped, Reason: reason}
}

// failed builds a failed Result carrying the underlying error and its text.
func failed(name string, err error) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeFailed, Reason: err.Error(), Err: err}
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// fileExists reports whether path exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isGitRepo reports whether dir contains a ".git" entry (a clone or worktree
// gitfile).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
