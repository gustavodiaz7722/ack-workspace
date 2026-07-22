// Package builder implements the Controller_Builder, which regenerates a service
// controller's code from its local (checked-out) source by running the
// code-generator's `make build-controller` target:
//
//  1. locate the controller and the code-generator in the workspace,
//  2. read the controller's currently checked-out branch (for reporting only),
//  3. run `make build-controller SERVICE=<alias>` in the code-generator
//     directory, wiring the environment overrides the code-generator scripts
//     need when the workspace root is not literally ".../aws-controllers-k8s".
//
// Like the deployer, the builder never touches git history: it regenerates
// against whatever the controller repository currently has checked out so a
// developer can iterate on local generator.yaml or hook-template changes. It is
// deliberately conservative about reporting: every execution problem is captured
// as a failed Result rather than returned out-of-band, and in dry-run mode it
// reports the command it would run and touches nothing.
//
// # Why the environment overrides are required
//
// `make build-controller` runs two scripts that locate their sibling repos
// inconsistently. build-controller.sh (core generation) defaults RUNTIME_CRD_DIR
// to "<code-generator>/../../aws-controllers-k8s/runtime/config", and
// build-controller-release.sh (the Helm/release step) additionally defaults both
// ACK_GENERATE_BIN_PATH and TEMPLATES_DIR to
// "<code-generator>/../../aws-controllers-k8s/code-generator/...". Those relative
// paths assume the workspace root directory is named "aws-controllers-k8s", so a
// workspace rooted anywhere else makes the scripts fail with "No such file or
// directory" (RUNTIME_CRD_DIR) or "Unable to find an ack-generate binary"
// (ACK_GENERATE_BIN_PATH). The builder resolves all three against the real
// workspace root and exports them so the full build succeeds regardless of the
// root's name.
package builder

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	// controllerSuffix is the conventional suffix of every service controller
	// repository name. A bare alias ("ecr") and its full form ("ecr-controller")
	// both normalize to the same repository.
	controllerSuffix = "-controller"
	// codegenDirName is the directory under the workspace root that holds the
	// ACK code-generator (its Makefile, scripts, and generated ack-generate
	// binary).
	codegenDirName = "code-generator"
	// runtimeDirName is the directory under the workspace root that holds the
	// ACK runtime (its config/crd tree is copied in during generation).
	runtimeDirName = "runtime"
	// makeTarget is the code-generator Makefile target that regenerates a
	// controller's code, CRDs, RBAC, and Helm chart in one shot.
	makeTarget = "build-controller"

	// The following are the environment-variable names the code-generator build
	// scripts read to locate their sibling repos. The builder sets each one so
	// the scripts resolve against the real workspace root rather than a path that
	// assumes the root is named "aws-controllers-k8s".

	// envRuntimeCRDDir points build-controller.sh at the runtime's config dir.
	envRuntimeCRDDir = "RUNTIME_CRD_DIR"
	// envAckGenerateBinPath points build-controller-release.sh at the compiled
	// ack-generate binary.
	envAckGenerateBinPath = "ACK_GENERATE_BIN_PATH"
	// envTemplatesDir points build-controller-release.sh at the code-generator
	// templates directory.
	envTemplatesDir = "TEMPLATES_DIR"
	// envAWSSDKGoVersion overrides the aws-sdk-go version the scripts otherwise
	// read from the controller's ack-generate-metadata.yaml. It is set only when
	// the caller pins one via --sdk-version.
	envAWSSDKGoVersion = "AWS_SDK_GO_VERSION"
	// envService names the AWS service the Makefile builds a controller for.
	envService = "SERVICE"
)

// UsageError is a typed argument/validation error returned by Build before any
// build is attempted (for example a missing service identifier). The cmd layer
// maps it to a distinct usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// MakeRunner runs the code-generator's `make build-controller` target for a
// service. It is the seam through which the real os/exec invocation is replaced
// by a scripted mock in tests, so the build flow can be exercised without
// running the actual code-generator.
type MakeRunner interface {
	// Run executes `make build-controller` for service in codegenDir (the
	// code-generator directory), with env (a list of "KEY=VALUE" entries)
	// appended to the inherited process environment.
	Run(ctx context.Context, codegenDir, service string, env []string) error
}

// Options controls build behavior. All fields are optional.
type Options struct {
	// SDKVersion, when non-empty, pins the aws-sdk-go version passed to the
	// code-generator via AWS_SDK_GO_VERSION. When empty the code-generator
	// scripts resolve it from the controller's ack-generate-metadata.yaml.
	SDKVersion string
}

// Builder implements the Controller_Builder.
type Builder struct {
	maker MakeRunner
}

// New returns a Builder that runs the real `make build-controller` target.
// Constructing it performs no external work; that happens only when Build runs.
func New() *Builder {
	return &Builder{maker: execMakeRunner{}}
}

// NewWithMakeRunner returns a Builder backed by the supplied MakeRunner. It is
// intended for tests that need to script or record the make invocation without
// running the real code-generator.
func NewWithMakeRunner(m MakeRunner) *Builder {
	return &Builder{maker: m}
}

// Build regenerates the controller named by service from its local
// implementation branch and returns a single-result Summary recording the
// outcome (built or failed).
//
// The returned error is non-nil only for a pre-flight validation failure (an
// empty service identifier); all execution problems are captured as a failed
// Result so the caller renders a uniform summary.
func (b *Builder) Build(ctx context.Context, ap app.App, service string, opts Options) (workspace.Summary, error) {
	alias := strings.TrimSuffix(strings.TrimSpace(service), controllerSuffix)
	if alias == "" {
		return workspace.Summary{}, &UsageError{Msg: "a service identifier is required (for example: ecr or ecr-controller)"}
	}

	result := b.process(ctx, ap, alias, opts)
	return workspace.Summary{Results: []workspace.Result{result}}, nil
}

// process runs the full build flow for one controller and returns its terminal
// Result. It never returns an error out-of-band: every failure is captured into
// a failed Result.
func (b *Builder) process(ctx context.Context, ap app.App, alias string, opts Options) workspace.Result {
	name := alias + controllerSuffix
	root := ap.Config.WorkspaceRoot
	controllerPath := filepath.Join(root, name)
	codegenPath := filepath.Join(root, codegenDirName)

	// Pre-flight: the controller and the code-generator must already be present
	// in the workspace. Building neither forks nor clones.
	if !dirExists(controllerPath) {
		return failed(name, fmt.Errorf("controller %s not found at %s; add it first with `ack-workspace add %s`", name, controllerPath, alias))
	}
	if !isGitRepo(controllerPath) {
		return failed(name, fmt.Errorf("%s is not a git repository", controllerPath))
	}
	if !dirExists(codegenPath) {
		return failed(name, fmt.Errorf("code-generator not found at %s; run `ack-workspace init` first", codegenPath))
	}

	// Read the checked-out branch for reporting only. The build regenerates
	// against whatever is currently checked out; it never switches branches.
	branch := b.describeBranch(ctx, controllerPath, ap.Git)

	// The environment overrides that make the code-generator scripts resolve
	// their sibling repos against the real workspace root (see the package doc).
	env := buildEnv(root, opts)

	// Dry-run: report the command that would be run without executing anything.
	if ap.DryRun {
		return b.preview(name, alias, branch, codegenPath, env)
	}

	if err := b.maker.Run(ctx, codegenPath, alias, env); err != nil {
		return failed(name, fmt.Errorf("building controller: %w", err))
	}

	return built(name, fmt.Sprintf(
		"built %s from branch %s with `make %s SERVICE=%s`",
		name, branch, makeTarget, alias))
}

// describeBranch returns a human-readable label for the controller's checked-out
// state ("branch <name>", "detached HEAD", or "unknown branch" when it cannot be
// determined). It never fails the build: the branch is reported for context
// only.
func (b *Builder) describeBranch(ctx context.Context, controllerPath string, runner git.Runner) string {
	repo := git.NewRepo(controllerPath, runner)
	name, detached, err := repo.CurrentBranch(ctx)
	switch {
	case err != nil:
		return "unknown branch"
	case detached:
		return "detached HEAD"
	default:
		return name
	}
}

// preview computes the build step for a dry-run without executing anything.
func (b *Builder) preview(name, alias, branch, codegenPath string, env []string) workspace.Result {
	reason := fmt.Sprintf(
		"would run `make %s SERVICE=%s` in %s against branch %s with %s",
		makeTarget, alias, codegenPath, branch, strings.Join(env, " "))
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: reason}
}

// buildEnv assembles the "KEY=VALUE" environment overrides the code-generator
// build scripts need when the workspace root is not literally
// ".../aws-controllers-k8s". SERVICE is passed as a make argument rather than an
// environment entry (see execMakeRunner.Run), so it is not included here.
func buildEnv(root string, opts Options) []string {
	env := []string{
		envRuntimeCRDDir + "=" + filepath.Join(root, runtimeDirName, "config"),
		envAckGenerateBinPath + "=" + filepath.Join(root, codegenDirName, "bin", "ack-generate"),
		envTemplatesDir + "=" + filepath.Join(root, codegenDirName, "templates"),
	}
	if v := strings.TrimSpace(opts.SDKVersion); v != "" {
		env = append(env, envAWSSDKGoVersion+"="+v)
	}
	return env
}

// execMakeRunner is the production MakeRunner. It invokes
// `make build-controller SERVICE=<service>` with the working directory set to
// the code-generator directory and the supplied overrides appended to the
// inherited environment.
type execMakeRunner struct{}

// Run executes `make build-controller SERVICE=<service>` in codegenDir with env
// appended to the inherited process environment. SERVICE is passed as a make
// variable assignment so the Makefile's $(SERVICE) resolves, while the path
// overrides travel through the environment where the build scripts read them. On
// failure it surfaces any make/script output to aid debugging.
func (execMakeRunner) Run(ctx context.Context, codegenDir, service string, env []string) error {
	cmd := exec.CommandContext(ctx, "make", makeTarget, envService+"="+service)
	cmd.Dir = codegenDir
	cmd.Env = append(os.Environ(), env...)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(combined.String())
		label := fmt.Sprintf("make %s %s=%s", makeTarget, envService, service)
		if out != "" {
			return fmt.Errorf("%s: %w\n%s", label, err, out)
		}
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// built builds a successful (OutcomeCreated) Result with the given reason.
func built(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: reason}
}

// failed builds a failed Result carrying the underlying error and its text.
func failed(name string, err error) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeFailed, Reason: err.Error(), Err: err}
}

// dirExists reports whether path exists.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isGitRepo reports whether dir contains a ".git" entry (a clone or worktree
// gitfile).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
