// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package deployer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// ClusterName is the one development cluster every deploy targets. It is
	// deliberately fixed rather than configurable: a single well-known cluster
	// per account is all local iteration needs, and pinning the name means every
	// deploy converges on the same bootstrapped state instead of quietly creating
	// a second cluster after a typo.
	ClusterName = "ack-dev-auto"

	// SharedServiceAccount is the Kubernetes service account every controller on
	// a bootstrapped cluster runs under.
	//
	// EKS Pod Identity associations are keyed on (namespace, serviceAccountName)
	// and do not support wildcards, so there is no way to grant credentials to a
	// whole namespace at once. Pointing every controller at one shared account
	// means the single association created with the cluster covers all of them,
	// instead of needing one association per controller.
	SharedServiceAccount = "ack-controller"

	// DefaultPolicyARN is the IAM policy attached to the pod identity role on a
	// bootstrapped cluster.
	//
	// It is deliberately broad: one role has to work for any ACK controller
	// dropped into a throwaway development account, and the service a controller
	// calls is not known when the cluster is created. It is not appropriate for a
	// shared or production account; scope it down with the policy-ARN option
	// there.
	DefaultPolicyARN = "arn:aws:iam::aws:policy/AdministratorAccess"

	// eksctlAPIVersion and eksctlKind identify the generated eksctl cluster
	// configuration document.
	eksctlAPIVersion = "eksctl.io/v1alpha5"
	eksctlKind       = "ClusterConfig"

	// clusterConfigFileName is the name given to the rendered eksctl cluster
	// configuration inside its temporary directory. It is retained on failure so
	// the caller can inspect or reuse it.
	clusterConfigFileName = "cluster-config.yaml"

	// notFoundMarkers are the substrings AWS error output carries when a cluster
	// genuinely does not exist, as opposed to the call itself failing. Only these
	// are treated as "absent"; anything else (expired credentials, no network, a
	// denied call) is surfaced as an error rather than triggering a 20-minute
	// cluster creation.
	notFoundException = "ResourceNotFoundException"
	notFoundMessage   = "No cluster found"
)

// autoModeNodePools are the EKS Auto Mode node pools enabled on a bootstrapped
// cluster: "general-purpose" runs the controllers, "system" runs cluster
// add-ons.
var autoModeNodePools = []string{"general-purpose", "system"}

// clusterTags are applied to a bootstrapped cluster so it is identifiable (and
// safe to delete) later.
var clusterTags = map[string]string{
	"purpose":    "ack-controller-development",
	"managed-by": "ack-workspace",
}

// ClusterSpec describes the development cluster deploy creates when the target
// cluster is absent.
type ClusterSpec struct {
	// Name is the EKS cluster name.
	Name string
	// Region is the region the cluster is created in.
	Region string
	// Version pins the Kubernetes version. When empty, eksctl's own default
	// version is used, so this does not go stale as EKS releases new versions.
	Version string
	// Namespace is the namespace controllers are installed into, and the
	// namespace half of the pod identity association key.
	Namespace string
	// ServiceAccount is the service account controllers run under, and the
	// service-account half of the pod identity association key.
	ServiceAccount string
	// RoleName is the name of the IAM role the association points at.
	RoleName string
	// PolicyARNs are the IAM policies attached to that role.
	PolicyARNs []string
}

// PodIdentitySpec describes one EKS Pod Identity association: the binding that
// gives a controller pod AWS credentials without a static secret or an IRSA
// annotation.
type PodIdentitySpec struct {
	// Cluster is the cluster the association is created on.
	Cluster string
	// Region is the cluster's region.
	Region string
	// Namespace and ServiceAccount form the association key.
	Namespace      string
	ServiceAccount string
	// RoleName is the IAM role the association points at. It is reused when a
	// role of that name already exists and created otherwise.
	RoleName string
	// PolicyARNs are attached to the role when it has to be created.
	PolicyARNs []string
}

// Provisioner manages the lifecycle of the development cluster a deploy targets:
// checking whether it exists, creating it with EKS Auto Mode when it does not,
// pointing the local kubeconfig at it, and making sure the controller's service
// account exists and is bound to AWS credentials through an EKS Pod Identity
// association. It is the seam through which the real eksctl/aws/kubectl
// invocations are replaced in tests.
type Provisioner interface {
	// ClusterExists reports whether an EKS cluster named name exists in region.
	// A failure to answer the question (rather than a definitive "no") is
	// returned as an error, so an unrelated failure never provokes a cluster
	// creation.
	ClusterExists(ctx context.Context, name, region string) (bool, error)
	// CreateCluster creates the cluster described by spec, together with the pod
	// identity association for its controller service account. It blocks until
	// the cluster is ready, which typically takes 15-25 minutes.
	CreateCluster(ctx context.Context, spec ClusterSpec) error
	// UpdateKubeconfig points the local kubeconfig (and therefore kubectl and
	// helm) at the named cluster and makes it the current context.
	UpdateKubeconfig(ctx context.Context, name, region string) error
	// EnsureServiceAccount creates namespace and the named service account when
	// they are absent. The controller chart is told not to create its own
	// service account, so this owns it instead: one account, shared by every
	// controller, matching the single pod identity association.
	EnsureServiceAccount(ctx context.Context, namespace, name string) error
	// EnsurePodIdentity ensures an association exists for spec's
	// (namespace, service account), creating it when absent and reporting
	// whether it did.
	EnsurePodIdentity(ctx context.Context, spec PodIdentitySpec) (created bool, err error)
}

// eksctl cluster configuration document types. Only the fields a development
// cluster needs are modeled.
//
// Auto Mode makes compute, VPC CNI networking, EBS block storage, load
// balancing and CoreDNS built-in cluster capabilities rather than addons, so
// there is no managedNodeGroups block and no addon list to maintain. The EKS Pod
// Identity Agent is built in as well, which is why the eks-pod-identity-agent
// addon must not be installed on such a cluster.
type eksctlClusterConfig struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   eksctlMetadata   `yaml:"metadata"`
	AutoMode   eksctlAutoMode   `yaml:"autoModeConfig"`
	IAM        eksctlClusterIAM `yaml:"iam"`
}

type eksctlMetadata struct {
	Name    string            `yaml:"name"`
	Region  string            `yaml:"region"`
	Version string            `yaml:"version,omitempty"`
	Tags    map[string]string `yaml:"tags,omitempty"`
}

type eksctlAutoMode struct {
	Enabled   bool     `yaml:"enabled"`
	NodePools []string `yaml:"nodePools"`
}

type eksctlClusterIAM struct {
	PodIdentityAssociations []eksctlPodIdentityAssociation `yaml:"podIdentityAssociations"`
}

type eksctlPodIdentityAssociation struct {
	Namespace            string   `yaml:"namespace"`
	ServiceAccountName   string   `yaml:"serviceAccountName"`
	RoleName             string   `yaml:"roleName"`
	CreateServiceAccount bool     `yaml:"createServiceAccount"`
	PermissionPolicyARNs []string `yaml:"permissionPolicyARNs"`
}

// PodIdentityRoleName returns the conventional name of the IAM role a
// bootstrapped cluster binds its controller service account to. Deriving it from
// the cluster and namespace keeps it unique per cluster, so two development
// clusters do not contend for one role.
func PodIdentityRoleName(cluster, namespace string) string {
	return fmt.Sprintf("%s-%s-controller", cluster, namespace)
}

// renderClusterConfig marshals spec into an eksctl ClusterConfig document.
//
// The association sets createServiceAccount: false because the service account
// is created separately (EnsureServiceAccount): a deploy onto an already-created
// cluster has to be able to create it too, so owning it in one place keeps both
// paths identical.
func renderClusterConfig(spec ClusterSpec) ([]byte, error) {
	cfg := eksctlClusterConfig{
		APIVersion: eksctlAPIVersion,
		Kind:       eksctlKind,
		Metadata: eksctlMetadata{
			Name:    spec.Name,
			Region:  spec.Region,
			Version: spec.Version,
			Tags:    clusterTags,
		},
		AutoMode: eksctlAutoMode{
			Enabled:   true,
			NodePools: autoModeNodePools,
		},
		IAM: eksctlClusterIAM{
			PodIdentityAssociations: []eksctlPodIdentityAssociation{{
				Namespace:            spec.Namespace,
				ServiceAccountName:   spec.ServiceAccount,
				RoleName:             spec.RoleName,
				CreateServiceAccount: false,
				PermissionPolicyARNs: spec.PolicyARNs,
			}},
		},
	}
	return yaml.Marshal(cfg)
}

// execProvisioner is the production Provisioner. It shells out to eksctl for
// cluster and association creation (eksctl owns the CloudFormation stacks and
// the pod identity trust policy), to the aws CLI for read-only lookups, and to
// kubectl for the namespace and service account.
type execProvisioner struct{}

// ClusterExists answers with `aws eks describe-cluster`. Only an explicit
// not-found response counts as absent; any other failure is returned, because
// mistaking a credential or network error for "absent" would start a
// 15-25 minute cluster creation nobody asked for.
func (execProvisioner) ClusterExists(ctx context.Context, name, region string) (bool, error) {
	cmd := exec.CommandContext(ctx, "aws", "eks", "describe-cluster",
		"--name", name, "--region", region, "--query", "cluster.status", "--output", "text")
	out, err := runCombined(cmd)
	if err == nil {
		return true, nil
	}
	if strings.Contains(out, notFoundException) || strings.Contains(out, notFoundMessage) {
		return false, nil
	}
	return false, annotate(fmt.Sprintf("aws eks describe-cluster --name %s", name), out, err)
}

// CreateCluster renders spec into an eksctl configuration file and runs
// `eksctl create cluster -f <file>`.
//
// Unlike the other steps this one streams eksctl's output to stderr as it runs:
// cluster creation takes 15-25 minutes, and a silent process for that long is
// indistinguishable from a hung one. The configuration file is removed on
// success and retained on failure, with its path named in the error, so a failed
// creation can be inspected or retried with eksctl directly.
func (execProvisioner) CreateCluster(ctx context.Context, spec ClusterSpec) error {
	if err := requireTool("eksctl"); err != nil {
		return err
	}

	doc, err := renderClusterConfig(spec)
	if err != nil {
		return fmt.Errorf("rendering eksctl cluster configuration: %w", err)
	}

	dir, err := os.MkdirTemp("", "ack-workspace-cluster-")
	if err != nil {
		return fmt.Errorf("creating temporary directory for cluster configuration: %w", err)
	}
	path := filepath.Join(dir, clusterConfigFileName)
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("writing cluster configuration %s: %w", path, err)
	}

	cmd := exec.CommandContext(ctx, "eksctl", "create", "cluster", "-f", path)
	if out, err := runStreaming(cmd, os.Stderr); err != nil {
		return annotate(fmt.Sprintf("eksctl create cluster -f %s (configuration retained)", path), out, err)
	}
	os.RemoveAll(dir)
	return nil
}

// UpdateKubeconfig runs `aws eks update-kubeconfig`, which writes the cluster's
// entry into the local kubeconfig and selects it as the current context. It runs
// on every deploy to a named cluster, not just after creation, so the deploy
// targets the cluster the caller asked for regardless of which context happened
// to be selected.
func (execProvisioner) UpdateKubeconfig(ctx context.Context, name, region string) error {
	cmd := exec.CommandContext(ctx, "aws", "eks", "update-kubeconfig", "--name", name, "--region", region)
	if out, err := runCombined(cmd); err != nil {
		return annotate(fmt.Sprintf("aws eks update-kubeconfig --name %s", name), out, err)
	}
	return nil
}

// EnsureServiceAccount creates the namespace and service account when they are
// absent, treating an AlreadyExists response as success so concurrent or
// repeated deploys are safe.
func (execProvisioner) EnsureServiceAccount(ctx context.Context, namespace, name string) error {
	if err := ensureKubeObject(ctx, "namespace", namespace, ""); err != nil {
		return err
	}
	return ensureKubeObject(ctx, "serviceaccount", name, namespace)
}

// EnsurePodIdentity creates an association for (namespace, service account) when
// one does not already exist.
//
// When an IAM role of the configured name already exists (the usual case on a
// cluster this tool bootstrapped) the association reuses it by ARN. Otherwise
// eksctl is asked to create the role with the given policies attached, which
// also gets the pods.eks.amazonaws.com trust policy right.
func (execProvisioner) EnsurePodIdentity(ctx context.Context, spec PodIdentitySpec) (bool, error) {
	exists, err := podIdentityExists(ctx, spec)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := requireTool("eksctl"); err != nil {
		return false, err
	}

	args := []string{
		"create", "podidentityassociation",
		"--cluster", spec.Cluster,
		"--region", spec.Region,
		"--namespace", spec.Namespace,
		"--service-account-name", spec.ServiceAccount,
	}
	if arn := existingRoleARN(ctx, spec.RoleName); arn != "" {
		args = append(args, "--role-arn", arn)
	} else {
		args = append(args, "--role-name", spec.RoleName)
		if len(spec.PolicyARNs) > 0 {
			args = append(args, "--permission-policy-arns", strings.Join(spec.PolicyARNs, ","))
		}
	}

	if out, err := runCombined(exec.CommandContext(ctx, "eksctl", args...)); err != nil {
		return false, annotate(fmt.Sprintf("eksctl create podidentityassociation --namespace %s --service-account-name %s",
			spec.Namespace, spec.ServiceAccount), out, err)
	}
	return true, nil
}

// podIdentityExists reports whether an association already covers spec's
// (namespace, service account) pair. The aws CLI prints "None" for an empty
// result, which is treated as absent.
func podIdentityExists(ctx context.Context, spec PodIdentitySpec) (bool, error) {
	cmd := exec.CommandContext(ctx, "aws", "eks", "list-pod-identity-associations",
		"--cluster-name", spec.Cluster,
		"--region", spec.Region,
		"--namespace", spec.Namespace,
		"--service-account", spec.ServiceAccount,
		"--query", "associations[0].associationId",
		"--output", "text")
	out, err := runCombined(cmd)
	if err != nil {
		return false, annotate("aws eks list-pod-identity-associations", out, err)
	}
	id := strings.TrimSpace(out)
	return id != "" && id != "None", nil
}

// existingRoleARN returns the ARN of the named IAM role, or "" when it does not
// exist or cannot be read. A lookup failure is deliberately not an error: it
// only decides whether the association reuses a role or asks eksctl to create
// one, and a genuine problem surfaces from that call with better context.
func existingRoleARN(ctx context.Context, roleName string) string {
	cmd := exec.CommandContext(ctx, "aws", "iam", "get-role",
		"--role-name", roleName, "--query", "Role.Arn", "--output", "text")
	out, err := runCombined(cmd)
	if err != nil {
		return ""
	}
	arn := strings.TrimSpace(out)
	if !strings.HasPrefix(arn, "arn:") {
		return ""
	}
	return arn
}

// ensureKubeObject creates a cluster-scoped (namespace == "") or namespaced
// Kubernetes object of the given kind when it is absent. An AlreadyExists
// response is success: the object being there is the desired end state.
func ensureKubeObject(ctx context.Context, kind, name, namespace string) error {
	get := []string{"get", kind, name}
	create := []string{"create", kind, name}
	if namespace != "" {
		get = append(get, "--namespace", namespace)
		create = append(create, "--namespace", namespace)
	}

	if _, err := runCombined(exec.CommandContext(ctx, "kubectl", get...)); err == nil {
		return nil
	}
	out, err := runCombined(exec.CommandContext(ctx, "kubectl", create...))
	if err != nil && !strings.Contains(out, "AlreadyExists") && !strings.Contains(out, "already exists") {
		return annotate(fmt.Sprintf("kubectl create %s %s", kind, name), out, err)
	}
	return nil
}

// requireTool reports a missing executable as an actionable error before any
// work depending on it is attempted.
func requireTool(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("no %q executable was found on your PATH; install %s and ensure it is on your PATH", name, name)
	}
	return nil
}

// runStreaming runs cmd with its combined output both captured and copied to
// progress, so a long-running command reports as it goes while its output
// remains available for error annotation.
func runStreaming(cmd *exec.Cmd, progress io.Writer) (string, error) {
	var combined strings.Builder
	w := io.MultiWriter(&combined, progress)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return combined.String(), err
}
