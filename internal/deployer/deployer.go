// Package deployer implements the Controller_Deployer, which builds a service
// controller from its local implementation branch and deploys it to the cluster
// named by the caller's current kubeconfig context:
//
//  1. resolve the target cluster from the current kubeconfig context,
//  2. resolve the caller's AWS account and region from the active credentials,
//  3. ensure an ECR repository for the controller exists in that account,
//     creating it when absent,
//  4. build the controller image from the local (checked-out) source using the
//     code-generator's build-controller-image.sh script, tagging it for ECR,
//  5. push the image to ECR, and
//  6. install or upgrade the controller's Helm chart on the current cluster,
//     pointing it at the freshly pushed image.
//
// Unlike the releaser, the deployer never touches git history: it reads the
// checked-out branch as-is so a developer can iterate on local changes. It is
// deliberately conservative about reporting: every execution problem is captured
// as a failed Result rather than returned out-of-band, and in dry-run mode it
// reports the steps it would take and touches nothing (no image is built, no
// repository is created, and the cluster is not modified).
package deployer

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
	// ACK code-generator (and its image build script).
	codegenDirName = "code-generator"
	// helmDirName is the controller subdirectory holding its Helm chart.
	helmDirName = "helm"
	// imageBuildScript is the code-generator script that builds a controller's
	// container image. It is invoked from the code-generator directory and honors
	// the AWS_SERVICE_DOCKER_IMG environment variable for the output image
	// reference.
	imageBuildScript = "./scripts/build-controller-image.sh"
	// defaultNamespace is the Kubernetes namespace ACK controllers are installed
	// into when the caller does not override it.
	defaultNamespace = "ack-system"
	// releasePrefix is prepended to the controller name to form the Helm release
	// name (for example "ack-ecr-controller").
	releasePrefix = "ack-"
)

// UsageError is a typed argument/validation error returned by Deploy before any
// build, push, or deploy is attempted (for example a missing service
// identifier). The cmd layer maps it to a distinct usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Builder builds the controller container image from the local implementation
// source. It is the seam through which the real code-generator script invocation
// is replaced in tests.
type Builder interface {
	// Build builds the image for service from the code-generator at codegenDir,
	// tagging it imageRef. The build uses the controller's checked-out source, so
	// it captures local implementation changes.
	Build(ctx context.Context, codegenDir, service, imageRef string) error
}

// Registry resolves the caller's AWS account and region and manages the ECR
// repository the controller image is pushed to. It is the seam through which the
// real aws/docker CLI invocations are replaced in tests.
type Registry interface {
	// Identity returns the AWS account ID and region resolved from the active
	// credentials and configuration.
	Identity(ctx context.Context) (account, region string, err error)
	// EnsureRepository ensures an ECR repository named repo exists in region,
	// creating it when absent. It reports whether it created the repository.
	EnsureRepository(ctx context.Context, repo, region string) (created bool, err error)
	// PushImage authenticates the local docker client to the ECR registry and
	// pushes imageRef.
	PushImage(ctx context.Context, imageRef, region string) error
}

// Cluster deploys the controller to the cluster named by the current kubeconfig
// context. It is the seam through which the real kubectl/helm invocations are
// replaced in tests.
type Cluster interface {
	// CurrentContext returns the active kubeconfig context name (the cluster a
	// deploy targets).
	CurrentContext(ctx context.Context) (string, error)
	// Deploy installs or upgrades the controller's Helm chart at chartDir into
	// namespace under release, pointing the deployment at imageRepo:imageTag and
	// configuring the controller for region.
	Deploy(ctx context.Context, chartDir, namespace, release, imageRepo, imageTag, region string) error
}

// Options controls deploy behavior. All fields are optional; each falls back to
// a sensible default described on the field.
type Options struct {
	// Namespace is the Kubernetes namespace the controller is installed into. It
	// defaults to "ack-system" when empty.
	Namespace string
	// ImageTag is the tag applied to the built image. It defaults to the
	// abbreviated SHA of the controller's checked-out HEAD when empty, so each
	// build is traceable to the exact local commit.
	ImageTag string
	// Repository overrides the ECR repository name. It defaults to the controller
	// repository name ("<service>-controller") when empty.
	Repository string
	// Region overrides the AWS region the image is pushed to and the controller is
	// configured for. It defaults to the region resolved from the active AWS
	// configuration when empty.
	Region string
}

// Deployer implements the Controller_Deployer.
type Deployer struct {
	builder  Builder
	registry Registry
	cluster  Cluster
}

// New returns a Deployer wired to the production toolchain: the code-generator
// image build script, the aws/docker CLIs for ECR, and kubectl/helm for the
// cluster. Constructing it performs no external work; that happens only when
// Deploy runs.
func New() *Deployer {
	return &Deployer{
		builder:  execBuilder{},
		registry: execRegistry{},
		cluster:  execCluster{},
	}
}

// NewWith returns a Deployer backed by the supplied collaborators. It is intended
// for tests that need to script build, registry, and cluster behavior without
// invoking the real toolchain.
func NewWith(b Builder, r Registry, c Cluster) *Deployer {
	return &Deployer{builder: b, registry: r, cluster: c}
}

// Deploy builds the controller named by service from its local implementation
// branch and deploys it to the current kubeconfig cluster, returning a
// single-result Summary recording the outcome (deployed, skipped, or failed).
//
// The returned error is non-nil only for a pre-flight validation failure (an
// empty service identifier); all execution problems are captured as a failed
// Result so the caller renders a uniform summary.
func (d *Deployer) Deploy(ctx context.Context, ap app.App, service string, opts Options) (workspace.Summary, error) {
	alias := strings.TrimSuffix(strings.TrimSpace(service), controllerSuffix)
	if alias == "" {
		return workspace.Summary{}, &UsageError{Msg: "a service identifier is required (for example: ecr or ecr-controller)"}
	}

	result := d.process(ctx, ap, alias, opts)
	return workspace.Summary{Results: []workspace.Result{result}}, nil
}

// process runs the full build/push/deploy flow for one controller and returns
// its terminal Result. It never returns an error out-of-band: every failure is
// captured into a failed Result.
func (d *Deployer) process(ctx context.Context, ap app.App, alias string, opts Options) workspace.Result {
	name := alias + controllerSuffix
	root := ap.Config.WorkspaceRoot
	controllerPath := filepath.Join(root, name)
	codegenPath := filepath.Join(root, codegenDirName)
	chartPath := filepath.Join(controllerPath, helmDirName)

	// Pre-flight: the controller (with its Helm chart) and the code-generator
	// must already be present in the workspace. Deploying neither forks nor
	// clones.
	if !dirExists(controllerPath) {
		return failed(name, fmt.Errorf("controller %s not found at %s; add it first with `ack-workspace add %s`", name, controllerPath, alias))
	}
	if !isGitRepo(controllerPath) {
		return failed(name, fmt.Errorf("%s is not a git repository", controllerPath))
	}
	if !dirExists(codegenPath) {
		return failed(name, fmt.Errorf("code-generator not found at %s; run `ack-workspace init` first", codegenPath))
	}
	if !dirExists(chartPath) {
		return failed(name, fmt.Errorf("Helm chart not found at %s", chartPath))
	}

	// Resolve the image tag from the local implementation commit unless the
	// caller pinned one, so a build is traceable to the exact checked-out state.
	tag := strings.TrimSpace(opts.ImageTag)
	if tag == "" {
		repo := git.NewRepo(controllerPath, ap.Git)
		sha, err := repo.HeadSHA(ctx)
		if err != nil {
			return failed(name, fmt.Errorf("determining image tag: %w", err))
		}
		tag = sha
	}

	// Identify the target cluster from the current kubeconfig context. Doing this
	// first means an unreachable or unset context fails before any image work.
	kubeContext, err := d.cluster.CurrentContext(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("determining current kubeconfig context: %w", err))
	}
	kubeContext = strings.TrimSpace(kubeContext)
	if kubeContext == "" {
		return failed(name, fmt.Errorf("no current kubeconfig context is set; select a cluster with `kubectl config use-context`"))
	}

	// Resolve the AWS account and region the image is pushed to.
	account, region, err := d.registry.Identity(ctx)
	if err != nil {
		return failed(name, fmt.Errorf("resolving AWS account and region: %w", err))
	}
	if r := strings.TrimSpace(opts.Region); r != "" {
		region = r
	}
	if account == "" || region == "" {
		return failed(name, fmt.Errorf("could not resolve AWS account (%q) and region (%q); configure your AWS credentials and region", account, region))
	}

	repoName := strings.TrimSpace(opts.Repository)
	if repoName == "" {
		repoName = name
	}
	namespace := strings.TrimSpace(opts.Namespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	release := releasePrefix + name

	registryHost := fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", account, region)
	imageRepo := registryHost + "/" + repoName
	imageRef := imageRepo + ":" + tag

	// Dry-run: report the steps that would be taken without mutating anything.
	if ap.DryRun {
		return d.preview(name, kubeContext, imageRef, namespace, release)
	}

	// 1. Ensure the ECR repository exists, creating it in the current account
	// when absent.
	created, err := d.registry.EnsureRepository(ctx, repoName, region)
	if err != nil {
		return failed(name, fmt.Errorf("ensuring ECR repository %q: %w", repoName, err))
	}

	// 2. Build the controller image from the local implementation source.
	if err := d.builder.Build(ctx, codegenPath, alias, imageRef); err != nil {
		return failed(name, fmt.Errorf("building controller image: %w", err))
	}

	// 3. Push the image to ECR.
	if err := d.registry.PushImage(ctx, imageRef, region); err != nil {
		return failed(name, fmt.Errorf("pushing image %s: %w", imageRef, err))
	}

	// 4. Install or upgrade the controller on the current cluster.
	if err := d.cluster.Deploy(ctx, chartPath, namespace, release, imageRepo, tag, region); err != nil {
		return failed(name, fmt.Errorf("deploying to cluster %q: %w", kubeContext, err))
	}

	repoNote := "existing ECR repository"
	if created {
		repoNote = "created ECR repository"
	}
	return deployed(name, fmt.Sprintf(
		"deployed %s to cluster %q (namespace %s); %s %s",
		imageRef, kubeContext, namespace, repoNote, repoName))
}

// preview computes the deploy steps for a dry-run without mutating anything.
func (d *Deployer) preview(name, kubeContext, imageRef, namespace, release string) workspace.Result {
	reason := fmt.Sprintf(
		"would ensure ECR repository, build %s from local source, push it, and helm upgrade --install %s into namespace %s on cluster %q",
		imageRef, release, namespace, kubeContext)
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeCreated, Reason: reason}
}

// execBuilder is the production Builder. It invokes the code-generator image
// build script with the working directory set to the code-generator directory
// and AWS_SERVICE_DOCKER_IMG exported so the script tags the image for ECR.
type execBuilder struct{}

// Build runs `./scripts/build-controller-image.sh <service>` in codegenDir with
// AWS_SERVICE_DOCKER_IMG=<imageRef> added to the inherited environment. On
// failure it surfaces any script output to aid debugging.
func (execBuilder) Build(ctx context.Context, codegenDir, service, imageRef string) error {
	cmd := exec.CommandContext(ctx, imageBuildScript, service)
	cmd.Dir = codegenDir
	cmd.Env = append(os.Environ(), "AWS_SERVICE_DOCKER_IMG="+imageRef)
	if out, err := runCombined(cmd); err != nil {
		return annotate(fmt.Sprintf("%s %s", imageBuildScript, service), out, err)
	}
	return nil
}

// execRegistry is the production Registry. It shells out to the aws and docker
// CLIs (whose presence is a runtime prerequisite of the deploy command).
type execRegistry struct{}

// Identity resolves the AWS account via `aws sts get-caller-identity` and the
// region via `aws configure get region`, falling back to the AWS_REGION and
// AWS_DEFAULT_REGION environment variables when the CLI reports no configured
// region.
func (execRegistry) Identity(ctx context.Context) (string, string, error) {
	accountOut, err := runCombined(exec.CommandContext(ctx, "aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text"))
	if err != nil {
		return "", "", annotate("aws sts get-caller-identity", accountOut, err)
	}
	account := strings.TrimSpace(accountOut)

	// `aws configure get region` exits non-zero when no region is configured;
	// treat that as "unset" and fall back to the environment rather than an error.
	regionOut, _ := runCombined(exec.CommandContext(ctx, "aws", "configure", "get", "region"))
	region := strings.TrimSpace(regionOut)
	if region == "" {
		region = firstNonEmptyEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	}
	return account, region, nil
}

// EnsureRepository checks for the repository with `aws ecr describe-repositories`
// and creates it with `aws ecr create-repository` when it is absent. A describe
// failure is interpreted as "not present" and triggers creation, so a genuine
// creation error is still surfaced.
func (execRegistry) EnsureRepository(ctx context.Context, repo, region string) (bool, error) {
	describe := exec.CommandContext(ctx, "aws", "ecr", "describe-repositories", "--repository-names", repo, "--region", region)
	if _, err := runCombined(describe); err == nil {
		return false, nil
	}
	create := exec.CommandContext(ctx, "aws", "ecr", "create-repository", "--repository-name", repo, "--region", region)
	if out, err := runCombined(create); err != nil {
		return false, annotate(fmt.Sprintf("aws ecr create-repository --repository-name %s", repo), out, err)
	}
	return true, nil
}

// PushImage authenticates docker to the ECR registry using an authorization
// token from `aws ecr get-login-password` piped into `docker login`, then runs
// `docker push`. The registry host is the portion of imageRef before the first
// "/".
func (execRegistry) PushImage(ctx context.Context, imageRef, region string) error {
	host := imageRef
	if i := strings.IndexByte(imageRef, '/'); i >= 0 {
		host = imageRef[:i]
	}

	pwOut, err := runCombined(exec.CommandContext(ctx, "aws", "ecr", "get-login-password", "--region", region))
	if err != nil {
		return annotate("aws ecr get-login-password", pwOut, err)
	}
	password := strings.TrimSpace(pwOut)

	login := exec.CommandContext(ctx, "docker", "login", "--username", "AWS", "--password-stdin", host)
	login.Stdin = strings.NewReader(password)
	if out, err := runCombined(login); err != nil {
		return annotate(fmt.Sprintf("docker login %s", host), out, err)
	}

	push := exec.CommandContext(ctx, "docker", "push", imageRef)
	if out, err := runCombined(push); err != nil {
		return annotate(fmt.Sprintf("docker push %s", imageRef), out, err)
	}
	return nil
}

// execCluster is the production Cluster. It shells out to kubectl and helm, both
// of which honor the caller's current kubeconfig context.
type execCluster struct{}

// CurrentContext returns the active kubeconfig context via
// `kubectl config current-context`.
func (execCluster) CurrentContext(ctx context.Context) (string, error) {
	out, err := runCombined(exec.CommandContext(ctx, "kubectl", "config", "current-context"))
	if err != nil {
		return "", annotate("kubectl config current-context", out, err)
	}
	return strings.TrimSpace(out), nil
}

// Deploy installs or upgrades the controller's Helm chart with
// `helm upgrade --install`, overriding the image repository and tag and setting
// the controller's AWS region. It creates the target namespace when necessary.
func (execCluster) Deploy(ctx context.Context, chartDir, namespace, release, imageRepo, imageTag, region string) error {
	args := helmUpgradeArgs(chartDir, namespace, release, imageRepo, imageTag, region)
	cmd := exec.CommandContext(ctx, "helm", args...)
	if out, err := runCombined(cmd); err != nil {
		return annotate(fmt.Sprintf("helm upgrade --install %s", release), out, err)
	}
	return nil
}

// helmUpgradeArgs builds the argument list for the `helm upgrade --install`
// invocation used to deploy a controller chart.
//
// The image tag is passed with `--set-string` rather than `--set` so that tags
// which look like numbers (for example an all-digit commit SHA such as
// "4881291", or a semver-like "1.2") are not type-coerced by Helm into a number
// and rejected by the chart's values schema, which requires image.tag to be a
// string.
func helmUpgradeArgs(chartDir, namespace, release, imageRepo, imageTag, region string) []string {
	return []string{
		"upgrade", "--install", release, chartDir,
		"--namespace", namespace,
		"--create-namespace",
		"--set", "image.repository=" + imageRepo,
		"--set-string", "image.tag=" + imageTag,
		"--set", "aws.region=" + region,
	}
}

// runCombined runs cmd capturing both stdout and stderr into a single buffer and
// returns the combined output together with any error, mirroring the git
// ExecRunner so external-tool failures carry their diagnostic output.
func runCombined(cmd *exec.Cmd) (string, error) {
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}

// annotate wraps a command failure with the command label and any captured
// output so the failed Result is actionable.
func annotate(label, out string, err error) error {
	out = strings.TrimSpace(out)
	if out != "" {
		return fmt.Errorf("%s: %w\n%s", label, err, out)
	}
	return fmt.Errorf("%s: %w", label, err)
}

// firstNonEmptyEnv returns the value of the first environment variable in names
// that is set to a non-empty value, or "" when none are.
func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// deployed builds a successful (OutcomeCreated) Result with the given reason.
func deployed(name, reason string) workspace.Result {
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
