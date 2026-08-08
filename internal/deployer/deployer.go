// Package deployer builds a service controller from its local checkout and
// deploys it to the shared development cluster (ClusterName):
//
//  1. resolve the caller's AWS account and region from the active credentials,
//  2. bring the development cluster into the state a controller needs: create it
//     (EKS Auto Mode plus an EKS Pod Identity association for the controller
//     service account) when it does not exist, point the local kubeconfig at it,
//     and ensure its service account and credential binding,
//  3. ensure an ECR repository for the controller exists in that account,
//     creating it when absent,
//  4. build the controller image from the local (checked-out) source using the
//     code-generator's build-controller-image.sh script, tagging it for ECR,
//  5. push the image to ECR, and
//  6. install or upgrade the controller's Helm chart on that cluster, pointing it
//     at the freshly pushed image.
//
// The target cluster is not selectable and the current kubeconfig context is
// never used as-is: step 2 repoints it on every deploy, so a deploy cannot land
// on whatever cluster happened to be selected.
//
// The deployer never touches git history; it reads the checked-out branch as-is
// so a developer can iterate on local changes. Every execution problem is
// captured as a failed Result rather than returned out-of-band, and a dry run
// reports the steps it would take while touching nothing.
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
	// codegenDirName is the directory under the workspace root that holds the ACK
	// code-generator (and its image build script).
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
// source. It is the seam through which the real code-generator script
// invocation is replaced in tests.
type Builder interface {
	// Build builds the image for service from the code-generator at codegenDir,
	// tagging it imageRef. The build uses the controller's checked-out source, so
	// it captures local implementation changes.
	Build(ctx context.Context, codegenDir, service, imageRef string) error
}

// Registry resolves the caller's AWS account and region and manages the ECR
// repository the controller image is pushed to. It is the seam through which
// the real aws/docker CLI invocations are replaced in tests.
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

// Cluster deploys the controller to the cluster the kubeconfig points at, which
// the Provisioner has already repointed at the development cluster. It is the
// seam through which the real helm invocation is replaced in tests.
type Cluster interface {
	// Deploy installs or upgrades the controller's Helm chart as described by p.
	Deploy(ctx context.Context, p DeployParams) error
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
	// ClusterVersion pins the Kubernetes version of the development cluster. It is
	// only consulted when the cluster is actually created. When empty, eksctl's
	// own default version is used.
	ClusterVersion string
	// PolicyARNs are the IAM policies attached to the cluster's pod identity role.
	// They default to DefaultPolicyARN (AdministratorAccess), which suits a
	// throwaway development account and nothing else. Like ClusterVersion, they
	// only apply when the role has to be created.
	PolicyARNs []string
	// ServiceAccount names an existing Kubernetes service account for the
	// controller to run under, instead of the one the chart creates by default
	// ("ack-<service>-controller").
	//
	// This matters because the chart's own service account carries no AWS
	// credential binding: it has no eks.amazonaws.com/role-arn annotation, and an
	// EKS Pod Identity association is attached to a specific service account name.
	// On a cluster that grants the controller credentials through either
	// mechanism, a chart-created service account leaves the controller with no way
	// to reach AWS and it exits at startup with "unable to determine account info:
	// ... NoCredentialProviders".
	//
	// It defaults to SharedServiceAccount, the account the development cluster's
	// pod identity association is attached to. Naming a different one makes the
	// deploy create an association for that account instead.
	ServiceAccount string
}

// DeployParams describes one `helm upgrade --install` of a controller chart. It
// groups what was previously a long positional argument list so call sites stay
// readable as deploy grows more knobs.
type DeployParams struct {
	// ChartDir is the path to the controller's Helm chart.
	ChartDir string
	// Namespace is the namespace the controller is installed into.
	Namespace string
	// Release is the Helm release name.
	Release string
	// ImageRepo is the image repository (registry host plus repository name).
	ImageRepo string
	// ImageTag is the tag of the image to deploy.
	ImageTag string
	// Region is the AWS region the controller is configured for.
	Region string
	// ServiceAccount, when non-empty, makes the deploy reuse an existing service
	// account of that name rather than letting the chart create its own.
	ServiceAccount string
}

// Deployer builds a controller and deploys it to the development cluster.
type Deployer struct {
	builder     Builder
	registry    Registry
	cluster     Cluster
	provisioner Provisioner
}

// New returns a Deployer wired to the production toolchain: the code-generator
// image build script, the aws/docker CLIs for ECR, kubectl/helm for the
// cluster, and eksctl for bootstrapping a managed development cluster.
// Constructing it performs no external work; that happens only when Deploy
// runs.
func New() *Deployer {
	return &Deployer{
		builder:     execBuilder{},
		registry:    execRegistry{},
		cluster:     execCluster{},
		provisioner: execProvisioner{},
	}
}

// NewWith returns a Deployer backed by the supplied collaborators. It is
// intended for tests that need to script build, registry, cluster, and
// provisioning behavior without invoking the real toolchain.
func NewWith(b Builder, r Registry, c Cluster, p Provisioner) *Deployer {
	return &Deployer{builder: b, registry: r, cluster: c, provisioner: p}
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

	// Pre-flight: the controller (with its Helm chart) and the code-generator must
	// already be present in the workspace. Deploying neither forks nor clones.
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

	// Resolve the image tag from the local implementation commit unless the caller
	// pinned one, so a build is traceable to the exact checked-out state.
	tag := strings.TrimSpace(opts.ImageTag)
	if tag == "" {
		repo := git.NewRepo(controllerPath, ap.Git)
		sha, err := repo.HeadSHA(ctx)
		if err != nil {
			return failed(name, fmt.Errorf("determining image tag: %w", err))
		}
		tag = sha
	}

	// Resolve the AWS account and region the image is pushed to. This comes first
	// because a managed cluster is looked up in that same account and region, so
	// an unusable AWS configuration fails before any cluster or image work.
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

	// The cluster binds AWS credentials to one specific service account through
	// its pod identity association, so default to that account rather than letting
	// the chart create its own, which would carry no credentials.
	serviceAccount := strings.TrimSpace(opts.ServiceAccount)
	if serviceAccount == "" {
		serviceAccount = SharedServiceAccount
	}

	params := DeployParams{
		ChartDir:       chartPath,
		Namespace:      namespace,
		Release:        release,
		ImageRepo:      imageRepo,
		ImageTag:       tag,
		Region:         region,
		ServiceAccount: serviceAccount,
	}

	// Look up the development cluster. An inconclusive answer is a failure rather
	// than an assumed "absent", so a credential or network problem never provokes
	// a cluster creation.
	clusterExisted, err := d.provisioner.ClusterExists(ctx, ClusterName, region)
	if err != nil {
		return failed(name, fmt.Errorf("checking whether cluster %q exists in %s: %w", ClusterName, region, err))
	}

	// Dry-run: report the steps that would be taken without mutating anything.
	if ap.DryRun {
		return d.preview(name, clusterExisted, imageRef, params)
	}

	// 1. Bring the cluster into the state a controller needs: create it when
	// absent, repoint the kubeconfig at it, and make sure the controller's service
	// account exists and is bound to AWS credentials.
	if err := d.provision(ctx, region, namespace, serviceAccount, opts, clusterExisted); err != nil {
		return failed(name, err)
	}

	// 2. Ensure the ECR repository exists, creating it in the current account
	// when absent.
	created, err := d.registry.EnsureRepository(ctx, repoName, region)
	if err != nil {
		return failed(name, fmt.Errorf("ensuring ECR repository %q: %w", repoName, err))
	}

	// 3. Build the controller image from the local implementation source.
	if err := d.builder.Build(ctx, codegenPath, alias, imageRef); err != nil {
		return failed(name, fmt.Errorf("building controller image: %w", err))
	}

	// 4. Push the image to ECR.
	if err := d.registry.PushImage(ctx, imageRef, region); err != nil {
		return failed(name, fmt.Errorf("pushing image %s: %w", imageRef, err))
	}

	// 5. Install or upgrade the controller on the cluster.
	if err := d.cluster.Deploy(ctx, params); err != nil {
		return failed(name, fmt.Errorf("deploying to cluster %q: %w", ClusterName, err))
	}

	repoNote := "existing ECR repository"
	if created {
		repoNote = "created ECR repository"
	}
	return deployed(name, fmt.Sprintf(
		"deployed %s to %s (namespace %s); %s %s",
		imageRef, describeCluster(clusterExisted), namespace, repoNote, repoName))
}

// describeCluster renders the target cluster for a human-readable outcome,
// distinguishing a cluster this deploy created from one it found.
func describeCluster(existed bool) string {
	if existed {
		return fmt.Sprintf("cluster %q", ClusterName)
	}
	return fmt.Sprintf("newly bootstrapped cluster %q", ClusterName)
}

// provision brings the development cluster into the state a controller needs:
// create it when it is absent, point the local kubeconfig at it, and make sure
// the controller's service account exists and is bound to AWS credentials
// through an EKS Pod Identity association.
//
// Every step is idempotent because the cluster is long-lived: this runs on every
// deploy against a cluster that usually already exists, and does only the work
// that is actually missing. The kubeconfig is repointed unconditionally, which is
// what makes a deploy target this cluster regardless of which context was
// selected beforehand. Creating the cluster carries the association for the
// default service account, but the association is still checked afterwards, since
// a caller who names a different service account needs one for it too.
func (d *Deployer) provision(
	ctx context.Context,
	region, namespace, serviceAccount string,
	opts Options,
	exists bool,
) error {
	policies := opts.PolicyARNs
	if len(policies) == 0 {
		policies = []string{DefaultPolicyARN}
	}
	roleName := PodIdentityRoleName(ClusterName, namespace)

	if !exists {
		spec := ClusterSpec{
			Name:           ClusterName,
			Region:         region,
			Version:        strings.TrimSpace(opts.ClusterVersion),
			Namespace:      namespace,
			ServiceAccount: serviceAccount,
			RoleName:       roleName,
			PolicyARNs:     policies,
		}
		if err := d.provisioner.CreateCluster(ctx, spec); err != nil {
			return fmt.Errorf("creating cluster %q: %w", ClusterName, err)
		}
	}

	if err := d.provisioner.UpdateKubeconfig(ctx, ClusterName, region); err != nil {
		return fmt.Errorf("pointing kubeconfig at cluster %q: %w", ClusterName, err)
	}
	if err := d.provisioner.EnsureServiceAccount(ctx, namespace, serviceAccount); err != nil {
		return fmt.Errorf("ensuring service account %s/%s: %w", namespace, serviceAccount, err)
	}
	if _, err := d.provisioner.EnsurePodIdentity(ctx, PodIdentitySpec{
		Cluster:        ClusterName,
		Region:         region,
		Namespace:      namespace,
		ServiceAccount: serviceAccount,
		RoleName:       roleName,
		PolicyARNs:     policies,
	}); err != nil {
		return fmt.Errorf("ensuring pod identity association for %s/%s: %w", namespace, serviceAccount, err)
	}
	return nil
}

// preview computes the deploy steps for a dry-run without mutating anything.
func (d *Deployer) preview(name string, clusterExists bool, imageRef string, p DeployParams) workspace.Result {
	// Lead with the cluster creation when there is one: it is by far the most
	// consequential thing a deploy can do, so a preview must not bury it behind
	// the image steps.
	var bootstrap string
	if !clusterExists {
		bootstrap = fmt.Sprintf(
			"would create EKS Auto Mode cluster %q in %s with a pod identity association for %s/%s, then ",
			ClusterName, p.Region, p.Namespace, p.ServiceAccount)
	}

	reason := fmt.Sprintf(
		"%swould point the kubeconfig at %s, ensure ECR repository, build %s from local source, push it, and helm upgrade --install %s into namespace %s under service account %q",
		bootstrap, describeCluster(clusterExists), imageRef, p.Release, p.Namespace, p.ServiceAccount)
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSucceeded, Reason: reason}
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
// CLIs, which the deploy command declares as prerequisites, so both are known to
// be present by the time any of this runs.
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

// EnsureRepository checks for the repository with `aws ecr
// describe-repositories` and creates it with `aws ecr create-repository` when
// it is absent. A describe failure is interpreted as "not present" and triggers
// creation, so a genuine creation error is still surfaced.
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

// execCluster is the production Cluster. It shells out to helm, which honors
// the current kubeconfig context — repointed at the development cluster by the
// Provisioner earlier in the same deploy.
type execCluster struct{}

// Deploy installs or upgrades the controller's Helm chart with `helm upgrade
// --install`, overriding the image repository and tag, setting the controller's
// AWS region, and optionally binding it to an existing service account. It
// creates the target namespace when necessary.
func (execCluster) Deploy(ctx context.Context, p DeployParams) error {
	args := helmUpgradeArgs(p)
	cmd := exec.CommandContext(ctx, "helm", args...)
	if out, err := runCombined(cmd); err != nil {
		return annotate(fmt.Sprintf("helm upgrade --install %s", p.Release), out, err)
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
//
// When p.ServiceAccount is set, the chart is told not to create a service
// account and to reference the named one instead. The name goes through
// `--set-string` for the same coercion reason as the tag: an all-digit name is
// a valid Kubernetes object name but Helm would otherwise turn it into a
// number.
func helmUpgradeArgs(p DeployParams) []string {
	args := []string{
		"upgrade", "--install", p.Release, p.ChartDir,
		"--namespace", p.Namespace,
		"--create-namespace",
		"--set", "image.repository=" + p.ImageRepo,
		"--set-string", "image.tag=" + p.ImageTag,
		"--set", "aws.region=" + p.Region,
	}
	if p.ServiceAccount != "" {
		args = append(args,
			"--set", "serviceAccount.create=false",
			"--set-string", "serviceAccount.name="+p.ServiceAccount,
		)
	}
	return args
}

// runCombined runs cmd capturing both stdout and stderr into a single buffer
// and returns the combined output together with any error, mirroring the git
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

// deployed builds a successful (OutcomeSucceeded) Result with the given reason.
func deployed(name, reason string) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeSucceeded, Reason: reason}
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
