// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the License is
// located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed on
// an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package deployer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// --- fakes -----------------------------------------------------------------

// fakeBuilder records the image build request.
type fakeBuilder struct {
	called   bool
	imageRef string
}

func (f *fakeBuilder) Build(_ context.Context, _, _, imageRef string) error {
	f.called = true
	f.imageRef = imageRef
	return nil
}

// fakeRegistry answers with a fixed account/region and records pushes.
type fakeRegistry struct {
	account string
	region  string
	pushed  string
}

func (f *fakeRegistry) Identity(context.Context) (string, string, error) {
	return f.account, f.region, nil
}

func (f *fakeRegistry) EnsureRepository(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeRegistry) PushImage(_ context.Context, imageRef, _ string) error {
	f.pushed = imageRef
	return nil
}

// fakeCluster records the helm deploy.
type fakeCluster struct {
	params *DeployParams
}

func (f *fakeCluster) Deploy(_ context.Context, p DeployParams) error {
	got := p
	f.params = &got
	return nil
}

// fakeProvisioner records every cluster-lifecycle call so a test can assert
// both what was provisioned and, just as importantly, what was left alone.
type fakeProvisioner struct {
	exists    bool
	existsErr error

	createdSpec  *ClusterSpec
	kubeconfig   []string
	serviceAccts [][2]string
	identity     *PodIdentitySpec
}

func (f *fakeProvisioner) ClusterExists(context.Context, string, string) (bool, error) {
	return f.exists, f.existsErr
}

func (f *fakeProvisioner) CreateCluster(_ context.Context, spec ClusterSpec) error {
	got := spec
	f.createdSpec = &got
	return nil
}

func (f *fakeProvisioner) UpdateKubeconfig(_ context.Context, name, region string) error {
	f.kubeconfig = append(f.kubeconfig, name+"/"+region)
	return nil
}

func (f *fakeProvisioner) EnsureServiceAccount(_ context.Context, namespace, name string) error {
	f.serviceAccts = append(f.serviceAccts, [2]string{namespace, name})
	return nil
}

func (f *fakeProvisioner) EnsurePodIdentity(_ context.Context, spec PodIdentitySpec) (bool, error) {
	got := spec
	f.identity = &got
	return true, nil
}

// --- helpers ---------------------------------------------------------------

// workspaceWithController builds a temporary workspace root holding a git
// controller clone with a Helm chart plus a code-generator directory, which is
// the layout deploy pre-flights for.
func workspaceWithController(t *testing.T, controller string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, controller, ".git"),
		filepath.Join(root, controller, helmDirName),
		filepath.Join(root, codegenDirName),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return root
}

// appWith wires an App around the given root, with a git runner that resolves
// HEAD to a fixed sha so the image tag is deterministic.
func appWith(root string, dryRun bool) app.App {
	runner := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return "abc1234\n", nil
		}
		return "", nil
	}}
	return app.App{
		Config: config.Config{WorkspaceRoot: root, RepoPrefix: "ack-", Concurrency: 1},
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

// deployFixture assembles a Deployer over fresh fakes.
func deployFixture(exists bool) (*Deployer, *fakeBuilder, *fakeRegistry, *fakeCluster, *fakeProvisioner) {
	b := &fakeBuilder{}
	r := &fakeRegistry{account: "123456789012", region: "us-west-2"}
	c := &fakeCluster{}
	p := &fakeProvisioner{exists: exists}
	return NewWith(b, r, c, p), b, r, c, p
}

// --- cluster configuration rendering ---------------------------------------

// TestRenderClusterConfig_AutoModeAndPodIdentity pins the shape of the
// generated eksctl document: Auto Mode enabled (which is what makes compute,
// networking and the pod identity agent built-in capabilities rather than
// addons) and exactly one pod identity association for the controller service
// account.
func TestRenderClusterConfig_AutoModeAndPodIdentity(t *testing.T) {
	doc, err := renderClusterConfig(ClusterSpec{
		Name:           ClusterName,
		Region:         "us-west-2",
		Namespace:      "ack-system",
		ServiceAccount: SharedServiceAccount,
		RoleName:       PodIdentityRoleName(ClusterName, "ack-system"),
		PolicyARNs:     []string{DefaultPolicyARN},
	})
	if err != nil {
		t.Fatalf("renderClusterConfig returned error: %v", err)
	}

	var got eksctlClusterConfig
	if err := yaml.Unmarshal(doc, &got); err != nil {
		t.Fatalf("generated document is not valid YAML: %v\n%s", err, doc)
	}

	if got.APIVersion != eksctlAPIVersion || got.Kind != eksctlKind {
		t.Errorf("apiVersion/kind = %q/%q, want %q/%q", got.APIVersion, got.Kind, eksctlAPIVersion, eksctlKind)
	}
	if got.Metadata.Name != ClusterName || got.Metadata.Region != "us-west-2" {
		t.Errorf("metadata = %+v, want name %s in us-west-2", got.Metadata, ClusterName)
	}
	if !got.AutoMode.Enabled {
		t.Error("autoModeConfig.enabled = false, want true")
	}
	if len(got.IAM.PodIdentityAssociations) != 1 {
		t.Fatalf("want exactly one pod identity association, got %d", len(got.IAM.PodIdentityAssociations))
	}
	assoc := got.IAM.PodIdentityAssociations[0]
	if assoc.Namespace != "ack-system" || assoc.ServiceAccountName != SharedServiceAccount {
		t.Errorf("association key = %s/%s, want ack-system/%s", assoc.Namespace, assoc.ServiceAccountName, SharedServiceAccount)
	}
	// The service account is created separately so the same code path serves a
	// deploy onto an already-existing cluster.
	if assoc.CreateServiceAccount {
		t.Error("createServiceAccount = true, want false (deploy owns the service account)")
	}

	// The pod identity agent is built into Auto Mode, so an addon list would
	// conflict with it. Assert the rendered document declares none.
	if s := string(doc); strings.Contains(s, "addons") || strings.Contains(s, "managedNodeGroups") {
		t.Errorf("document should declare no addons or node groups under Auto Mode:\n%s", s)
	}
}

// TestRenderClusterConfig_VersionOmittedWhenUnset covers version drift: with no
// pinned version the document must leave it out so eksctl picks a currently
// supported default, rather than carrying a version that ages into
// unsupportability.
func TestRenderClusterConfig_VersionOmittedWhenUnset(t *testing.T) {
	doc, err := renderClusterConfig(ClusterSpec{Name: "c", Region: "us-west-2"})
	if err != nil {
		t.Fatalf("renderClusterConfig returned error: %v", err)
	}
	if strings.Contains(string(doc), "version:") {
		t.Errorf("want no version key when unset, got:\n%s", doc)
	}

	doc, err = renderClusterConfig(ClusterSpec{Name: "c", Region: "us-west-2", Version: "1.34"})
	if err != nil {
		t.Fatalf("renderClusterConfig returned error: %v", err)
	}
	if !strings.Contains(string(doc), `version: "1.34"`) {
		t.Errorf("want a quoted pinned version, got:\n%s", doc)
	}
}

func TestPodIdentityRoleName(t *testing.T) {
	if got, want := PodIdentityRoleName("ack-dev-auto", "ack-system"), "ack-dev-auto-ack-system-controller"; got != want {
		t.Errorf("PodIdentityRoleName = %q, want %q", got, want)
	}
}

// TestClusterNameIsFixed pins that the target cluster is a constant. Making it
// selectable again would reintroduce the possibility of a deploy landing
// somewhere unintended, which is what removing the option was for.
func TestClusterNameIsFixed(t *testing.T) {
	if ClusterName != "ack-dev-auto" {
		t.Errorf("ClusterName = %q, want ack-dev-auto", ClusterName)
	}
}

// --- deploy flow -----------------------------------------------------------

// TestDeploy_BootstrapsMissingCluster is the headline case: with the
// development cluster absent, a deploy creates it, binds credentials to the
// shared service account, and deploys the controller under that account.
func TestDeploy_BootstrapsMissingCluster(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, _, cluster, prov := deployFixture(false)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}

	if prov.createdSpec == nil {
		t.Fatal("cluster was not created")
	}
	spec := *prov.createdSpec
	if spec.Name != ClusterName || spec.Region != "us-west-2" {
		t.Errorf("created cluster %q in %q, want %s in us-west-2", spec.Name, spec.Region, ClusterName)
	}
	if spec.Namespace != defaultNamespace || spec.ServiceAccount != SharedServiceAccount {
		t.Errorf("association key = %s/%s, want %s/%s", spec.Namespace, spec.ServiceAccount, defaultNamespace, SharedServiceAccount)
	}
	if spec.RoleName != PodIdentityRoleName(ClusterName, defaultNamespace) {
		t.Errorf("role name = %q, want %q", spec.RoleName, PodIdentityRoleName(ClusterName, defaultNamespace))
	}
	if len(spec.PolicyARNs) != 1 || spec.PolicyARNs[0] != DefaultPolicyARN {
		t.Errorf("policy ARNs = %v, want [%s]", spec.PolicyARNs, DefaultPolicyARN)
	}

	// The kubeconfig must be repointed, otherwise helm would install onto whatever
	// context happened to be selected before.
	if len(prov.kubeconfig) != 1 || prov.kubeconfig[0] != ClusterName+"/us-west-2" {
		t.Errorf("kubeconfig updates = %v, want one for %s/us-west-2", prov.kubeconfig, ClusterName)
	}
	if len(prov.serviceAccts) != 1 || prov.serviceAccts[0] != [2]string{defaultNamespace, SharedServiceAccount} {
		t.Errorf("service accounts ensured = %v, want one %s/%s", prov.serviceAccts, defaultNamespace, SharedServiceAccount)
	}
	if prov.identity == nil {
		t.Fatal("pod identity association was not ensured")
	}

	// The controller must run under the account the association is attached to, or
	// it starts with no AWS credentials.
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.ServiceAccount != SharedServiceAccount {
		t.Errorf("deployed under service account %q, want %q", cluster.params.ServiceAccount, SharedServiceAccount)
	}
	if !builder.called {
		t.Error("image was not built")
	}
	if !strings.Contains(res.Reason, "bootstrapped") {
		t.Errorf("reason = %q, want it to report the cluster bootstrap", res.Reason)
	}
}

// TestDeploy_ReusesExistingClusterAndStillRepointsKubeconfig pins the one-time
// nature of the bootstrap alongside the guarantee that matters on every run: a
// later deploy creates no cluster, but still repoints the kubeconfig so it
// cannot install onto whichever context happened to be selected.
func TestDeploy_ReusesExistingClusterAndStillRepointsKubeconfig(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, prov := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if prov.createdSpec != nil {
		t.Errorf("cluster was created even though it exists: %+v", prov.createdSpec)
	}
	if len(prov.kubeconfig) != 1 || prov.kubeconfig[0] != ClusterName+"/us-west-2" {
		t.Errorf("kubeconfig updates = %v, want one for %s/us-west-2", prov.kubeconfig, ClusterName)
	}
	if prov.identity == nil {
		t.Error("association was not checked on an existing cluster")
	}
	if strings.Contains(res.Reason, "bootstrapped") {
		t.Errorf("reason = %q, should not claim a bootstrap for an existing cluster", res.Reason)
	}
}

// TestDeploy_ServiceAccountOverride covers a caller who keeps the chart's
// per-service account name: the association must be created for that account,
// since associations cannot cover a namespace at large.
func TestDeploy_ServiceAccountOverride(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, cluster, prov := deployFixture(true)

	_, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{
		ServiceAccount: "ack-ecr-controller",
	})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if prov.identity == nil || prov.identity.ServiceAccount != "ack-ecr-controller" {
		t.Errorf("association = %+v, want one for ack-ecr-controller", prov.identity)
	}
	if cluster.params == nil || cluster.params.ServiceAccount != "ack-ecr-controller" {
		t.Errorf("deployed under %+v, want service account ack-ecr-controller", cluster.params)
	}
}

// TestDeploy_AmbiguousClusterLookupDoesNotCreate is the safety case: when the
// existence check itself fails (expired credentials, no network, a denied
// call), deploy must fail rather than interpret the failure as "absent" and
// start a 15-25 minute cluster creation.
func TestDeploy_AmbiguousClusterLookupDoesNotCreate(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, _, _, prov := deployFixture(false)
	prov.existsErr = errors.New("ExpiredTokenException")

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if prov.createdSpec != nil {
		t.Error("cluster was created despite an inconclusive existence check")
	}
	if builder.called {
		t.Error("image was built despite an inconclusive existence check")
	}
}

// TestDeploy_DryRunPreviewsBootstrapWithoutCreating pins that a preview of a
// bootstrap creates nothing and says a cluster would be created, which is the
// most consequential part of the plan.
func TestDeploy_DryRunPreviewsBootstrapWithoutCreating(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, prov := deployFixture(false)

	summary, err := d.Deploy(context.Background(), appWith(root, true), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if prov.createdSpec != nil || prov.kubeconfig != nil || prov.identity != nil {
		t.Error("dry run provisioned cluster resources")
	}
	if builder.called || registry.pushed != "" || cluster.params != nil {
		t.Error("dry run built, pushed, or deployed")
	}
	if !strings.Contains(res.Reason, "would create EKS Auto Mode cluster") {
		t.Errorf("reason = %q, want it to lead with the cluster creation", res.Reason)
	}
}
