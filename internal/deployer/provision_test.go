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
	"runtime"
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
	// imageExists is what the tag lookup reports, and imageExistsErr makes the
	// lookup inconclusive so the fall-through-to-build path can be exercised.
	imageExists    bool
	imageExistsErr error
	// lookedUp records the (repo, tag) the deploy asked about.
	lookedUp [2]string
}

func (f *fakeRegistry) Identity(context.Context) (string, string, error) {
	return f.account, f.region, nil
}

func (f *fakeRegistry) EnsureRepository(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeRegistry) ImageExists(_ context.Context, repo, tag, _ string) (bool, error) {
	f.lookedUp = [2]string{repo, tag}
	if f.imageExistsErr != nil {
		return false, f.imageExistsErr
	}
	return f.imageExists, nil
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
// controller clone with a Helm chart, a code-generator directory, and
// test-infra's build_config.yaml, which is the layout deploy pre-flights for.
func workspaceWithController(t *testing.T, controller string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, controller, ".git"),
		filepath.Join(root, controller, helmDirName),
		filepath.Join(root, codegenDirName),
		filepath.Join(root, testInfraDirName),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	buildConfig := filepath.Join(root, testInfraDirName, testInfraBuildConfig)
	if err := os.WriteFile(buildConfig, []byte("go_version: 1.26\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", buildConfig, err)
	}
	return root
}

// appWith wires an App around the given root, with a git runner that resolves
// HEAD to a fixed sha so the image tag is deterministic and reports a clean
// working tree.
func appWith(root string, dryRun bool) app.App {
	return appWithGit(root, dryRun, gitClean())
}

// appWithGit is appWith with an explicit git runner, for the cases that need the
// working tree to look dirty or the status query to fail.
func appWithGit(root string, dryRun bool, runner git.Runner) app.App {
	return app.App{
		Config: config.Config{WorkspaceRoot: root, RepoPrefix: "ack-", Concurrency: 1},
		Git:    runner,
		DryRun: dryRun,
	}
}

// gitClean resolves HEAD to a fixed sha and reports an empty `status
// --porcelain`, i.e. a clean tree.
func gitClean() git.Runner {
	return &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return "abc1234\n", nil
		}
		return "", nil
	}}
}

// gitDirty reports a modified file from `status --porcelain`, which is how the
// deploy sees uncommitted work.
func gitDirty() git.Runner {
	return &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		switch {
		case len(args) > 0 && args[0] == "rev-parse":
			return "abc1234\n", nil
		case len(args) > 0 && args[0] == "status":
			return " M generator.yaml\n", nil
		}
		return "", nil
	}}
}

// gitStatusFails makes the working-tree query itself fail, which must not be
// read as "clean".
func gitStatusFails() git.Runner {
	return &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "status" {
			return "", errors.New("not a git repository")
		}
		return "abc1234\n", nil
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

// --- kubeconfig exec API check ---------------------------------------------

// TestClassifyKubeconfigExecAPI pins which read-back values stop a deploy. Only
// the removed v1alpha1 API does; v1beta1, v1, and an entry with no exec plugin
// at all are left to kubectl. The CLI is described only when there is a
// failure to explain.
func TestClassifyKubeconfigExecAPI(t *testing.T) {
	for _, tc := range []struct {
		name       string
		apiVersion string
		wantErr    bool
	}{
		{name: "removed v1alpha1", apiVersion: "client.authentication.k8s.io/v1alpha1", wantErr: true},
		{name: "v1alpha1 with trailing newline", apiVersion: "client.authentication.k8s.io/v1alpha1\n", wantErr: true},
		{name: "supported v1beta1", apiVersion: "client.authentication.k8s.io/v1beta1"},
		{name: "supported v1", apiVersion: "client.authentication.k8s.io/v1"},
		{name: "no exec plugin", apiVersion: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			described := false
			err := classifyKubeconfigExecAPI(tc.apiVersion, func() (string, string) {
				described = true
				return "/usr/bin/aws", "aws-cli/1.18.147"
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if described != tc.wantErr {
				t.Errorf("CLI described = %v, want %v (only to explain a failure)", described, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			// The error has to name the cause and the fix, since kubectl's own
			// message names neither.
			for _, want := range []string{"v1alpha1", "/usr/bin/aws", "aws-cli/1.18.147", "AWS CLI v2", "which -a aws"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestStaleExecPluginError_WithoutCLIDetails covers the best-effort path: when
// the aws executable cannot be described, the error still reads cleanly and
// still says what to do.
func TestStaleExecPluginError_WithoutCLIDetails(t *testing.T) {
	msg := staleExecPluginError(staleExecAPIVersion, "", "").Error()
	if !strings.Contains(msg, "the `aws` executable on your PATH is too old") {
		t.Errorf("error %q, want it to name the PATH executable without empty placeholders", msg)
	}
	if strings.Contains(msg, "()") || strings.Contains(msg, ", ,") {
		t.Errorf("error %q contains an empty placeholder", msg)
	}
}

// writeFakeTool writes an executable shell script named name into dir.
func writeFakeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
}

// TestExecUpdateKubeconfig_ReadsBackExecAPI drives the real UpdateKubeconfig
// against fake aws and kubectl executables, so the read-back command and its
// wiring are exercised end to end: an old CLI's v1alpha1 entry fails the deploy
// with the CLI named, a current entry passes, and a read-back that cannot run
// is not itself a failure.
func TestExecUpdateKubeconfig_ReadsBackExecAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are shell scripts")
	}
	for _, tc := range []struct {
		name    string
		kubectl string
		wantErr bool
	}{
		{
			name:    "old CLI wrote v1alpha1",
			kubectl: `echo "client.authentication.k8s.io/v1alpha1"`,
			wantErr: true,
		},
		{
			name:    "current CLI wrote v1beta1",
			kubectl: `echo "client.authentication.k8s.io/v1beta1"`,
		},
		{
			name:    "read-back fails",
			kubectl: `echo "error: unable to read config" >&2; exit 1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// AWS CLI v1 prints its version to stderr; update-kubeconfig succeeds.
			writeFakeTool(t, dir, "aws", `if [ "$1" = "--version" ]; then echo "aws-cli/1.18.147 Python/2.7.18" >&2; fi
exit 0
`)
			// The fake only answers the exact read-back the code issues, so a change
			// to that command shows up here as a failure to detect v1alpha1.
			writeFakeTool(t, dir, "kubectl", `if [ "$*" = "config view --minify -o jsonpath={.users[0].user.exec.apiVersion}" ]; then
`+tc.kubectl+`
else
  echo "unexpected kubectl args: $*" >&2; exit 2
fi
`)
			t.Setenv("PATH", dir)

			err := execProvisioner{}.UpdateKubeconfig(context.Background(), ClusterName, "us-west-2")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErr {
				for _, want := range []string{filepath.Join(dir, "aws"), "aws-cli/1.18.147"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				if strings.Contains(err.Error(), "Python") {
					t.Errorf("error %q should carry only the first field of `aws --version`", err)
				}
			}
		})
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
	if spec.Namespace != Namespace || spec.ServiceAccount != SharedServiceAccount {
		t.Errorf("association key = %s/%s, want %s/%s", spec.Namespace, spec.ServiceAccount, Namespace, SharedServiceAccount)
	}
	if spec.RoleName != PodIdentityRoleName(ClusterName, Namespace) {
		t.Errorf("role name = %q, want %q", spec.RoleName, PodIdentityRoleName(ClusterName, Namespace))
	}
	if len(spec.PolicyARNs) != 1 || spec.PolicyARNs[0] != DefaultPolicyARN {
		t.Errorf("policy ARNs = %v, want [%s]", spec.PolicyARNs, DefaultPolicyARN)
	}

	// The kubeconfig must be repointed, otherwise helm would install onto whatever
	// context happened to be selected before.
	if len(prov.kubeconfig) != 1 || prov.kubeconfig[0] != ClusterName+"/us-west-2" {
		t.Errorf("kubeconfig updates = %v, want one for %s/us-west-2", prov.kubeconfig, ClusterName)
	}
	if len(prov.serviceAccts) != 1 || prov.serviceAccts[0] != [2]string{Namespace, SharedServiceAccount} {
		t.Errorf("service accounts ensured = %v, want one %s/%s", prov.serviceAccts, Namespace, SharedServiceAccount)
	}
	if prov.identity == nil {
		t.Fatal("pod identity association was not ensured")
	}

	// The controller must run under the account the association is attached to, or
	// it starts with no AWS credentials. The chart settings that pin it are
	// asserted in TestHelmUpgradeArgs_AlwaysPinsSharedServiceAccount; here it is
	// enough that the association covers the account and namespace the install
	// uses.
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if prov.identity.ServiceAccount != SharedServiceAccount || prov.identity.Namespace != cluster.params.Namespace {
		t.Errorf("association %s/%s does not match the install namespace %q under %s",
			prov.identity.Namespace, prov.identity.ServiceAccount, cluster.params.Namespace, SharedServiceAccount)
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

// TestDeploy_ServiceAccountIsTheSharedOne pins that the association the deploy
// ensures is keyed on the shared account, and that nothing can point the install
// somewhere else. A pod identity association covers exactly one (namespace,
// service account) pair, so this and the chart's own service-account settings
// have to name the same account or the controller starts without credentials.
func TestDeploy_ServiceAccountIsTheSharedOne(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, prov := deployFixture(true)

	if _, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{}); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if prov.identity == nil || prov.identity.ServiceAccount != SharedServiceAccount {
		t.Errorf("association = %+v, want one for %s", prov.identity, SharedServiceAccount)
	}
	if prov.identity.Namespace != Namespace {
		t.Errorf("association namespace = %q, want %q", prov.identity.Namespace, Namespace)
	}
	if len(prov.serviceAccts) != 1 || prov.serviceAccts[0] != [2]string{Namespace, SharedServiceAccount} {
		t.Errorf("service accounts ensured = %v, want one %s/%s", prov.serviceAccts, Namespace, SharedServiceAccount)
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

// TestDeploy_ResyncPeriodReachesChart pins the whole path from the option to
// the chart override. The value matters for validating reference behavior that
// only shows up across reconciles, and the chart's ten-hour default makes that
// impractical, so a silently dropped option would leave a tester waiting on a
// resync that never comes within the session.
func TestDeploy_ResyncPeriodReachesChart(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, cluster, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{ResyncPeriod: 60})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if res := only(t, summary); res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded; reason: %s", res.Outcome, res.Reason)
	}
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.ResyncPeriod != 60 {
		t.Errorf("DeployParams.ResyncPeriod = %d, want 60", cluster.params.ResyncPeriod)
	}
}

// An omitted period must arrive as 0 so helmUpgradeArgs emits no override and
// the chart keeps its own default.
func TestDeploy_UnsetResyncPeriodLeavesChartDefault(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, cluster, _ := deployFixture(true)

	if _, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{}); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.ResyncPeriod != 0 {
		t.Errorf("DeployParams.ResyncPeriod = %d, want 0", cluster.params.ResyncPeriod)
	}
}

// A negative period is rejected up front, like an empty service identifier: it
// returns a *UsageError out-of-band (mapping to the usage exit code) and does
// no cluster, build, or deploy work.
func TestDeploy_NegativeResyncPeriodIsUsageErrorBeforeAnyWork(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, prov := deployFixture(true)

	_, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{ResyncPeriod: -5})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *UsageError", err, err)
	}
	if builder.called || registry.pushed != "" || cluster.params != nil || prov.kubeconfig != nil {
		t.Error("a rejected resync period still built, pushed, deployed, or touched the cluster")
	}
}

// TestDeploy_DryRunReportsResyncPeriod pins that a preview reflects the
// override. A dry run that omitted it would misreport how the deployed
// controller behaves.
func TestDeploy_DryRunReportsResyncPeriod(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, true), "ecr", Options{ResyncPeriod: 60})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if !strings.Contains(res.Reason, "60s default resync period") {
		t.Errorf("reason = %q, want it to mention the 60s resync period", res.Reason)
	}
}

// TestDeploy_LogLevelReachesChartNormalized pins the whole path from the option
// to the chart override, including normalization: the level is matched
// case-insensitively and reaches helm in the lowercase form the chart expects.
func TestDeploy_LogLevelReachesChartNormalized(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, cluster, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{LogLevel: " Debug "})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if res := only(t, summary); res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded; reason: %s", res.Outcome, res.Reason)
	}
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.LogLevel != "debug" {
		t.Errorf("DeployParams.LogLevel = %q, want %q", cluster.params.LogLevel, "debug")
	}
}

// TestDeploy_OutcomeReportsChartOverrides pins that a real deploy's outcome,
// not just its preview, names the overrides it applied. It is the only
// confirmation the operator gets that the controller is running with them.
func TestDeploy_OutcomeReportsChartOverrides(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr",
		Options{ResyncPeriod: 60, LogLevel: "debug"})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded; reason: %s", res.Outcome, res.Reason)
	}
	if want := "(namespace ack-system) with a 60s default resync period and log level debug;"; !strings.Contains(res.Reason, want) {
		t.Errorf("reason = %q, want it to contain %q", res.Reason, want)
	}
}

// Without overrides the outcome keeps its original shape, with no dangling
// clause.
func TestDeploy_OutcomeOmitsOverridesWhenNoneSet(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if !strings.Contains(res.Reason, "(namespace ack-system); ") || strings.Contains(res.Reason, " with ") {
		t.Errorf("reason = %q, want no override clause", res.Reason)
	}
}

// An omitted level must arrive empty so helmUpgradeArgs emits no override and
// the chart keeps its own default.
func TestDeploy_UnsetLogLevelLeavesChartDefault(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, cluster, _ := deployFixture(true)

	if _, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{}); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.LogLevel != "" {
		t.Errorf("DeployParams.LogLevel = %q, want empty", cluster.params.LogLevel)
	}
}

// An unknown level is rejected up front as a usage error and does no cluster,
// build, or deploy work. Passing it through would deploy successfully: the
// runtime ignores the parse error and logs at info, so the run would look fine
// while hiding the debug output it was meant to produce.
func TestDeploy_UnknownLogLevelIsUsageErrorBeforeAnyWork(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, prov := deployFixture(true)

	_, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{LogLevel: "verbose"})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *UsageError", err, err)
	}
	if !strings.Contains(ue.Msg, "debug, info, warn, error") {
		t.Errorf("message = %q, want it to list the accepted levels", ue.Msg)
	}
	if builder.called || registry.pushed != "" || cluster.params != nil || prov.kubeconfig != nil {
		t.Error("a rejected log level still built, pushed, deployed, or touched the cluster")
	}
}

// TestDeploy_DryRunReportsChartOverrides pins that a preview names every chart
// override, joined into one clause, so it describes the controller that would
// actually run.
func TestDeploy_DryRunReportsChartOverrides(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, true), "ecr",
		Options{ResyncPeriod: 60, LogLevel: "debug"})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if want := "with a 60s default resync period and log level debug"; !strings.Contains(res.Reason, want) {
		t.Errorf("reason = %q, want it to contain %q", res.Reason, want)
	}
}

// --- image reuse -----------------------------------------------------------

// TestDeploy_ReusesExistingImageTag is the headline of the optimization: with
// the tag already in ECR the deploy neither builds nor pushes, and still
// installs the chart pointing at that image.
func TestDeploy_ReusesExistingImageTag(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, _ := deployFixture(true)
	registry.imageExists = true

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeSucceeded {
		t.Fatalf("outcome = %q, want succeeded; reason: %s", res.Outcome, res.Reason)
	}
	if builder.called {
		t.Error("image was built despite the tag already existing in ECR")
	}
	if registry.pushed != "" {
		t.Errorf("image was pushed despite the tag already existing in ECR: %q", registry.pushed)
	}
	// The deploy must still happen — skipping the build must not skip the rollout.
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.ImageTag != "abc1234" {
		t.Errorf("deployed tag = %q, want the reused abc1234", cluster.params.ImageTag)
	}
	// The outcome has to say the image was reused, otherwise a deploy that
	// finished in seconds looks like one that silently did nothing.
	if !strings.Contains(res.Reason, "deployed existing") {
		t.Errorf("reason = %q, want it to report the image was reused", res.Reason)
	}
	// The lookup must ask about the repository and tag actually being deployed.
	if registry.lookedUp != [2]string{"ecr-controller", "abc1234"} {
		t.Errorf("looked up %v, want [ecr-controller abc1234]", registry.lookedUp)
	}
}

// The absent-tag case must still build and push, which is the pre-existing
// behavior the reuse check must not disturb.
func TestDeploy_BuildsWhenImageTagAbsent(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, _, _ := deployFixture(true)
	registry.imageExists = false

	if _, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{}); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if !builder.called {
		t.Error("image was not built despite the tag being absent from ECR")
	}
	if registry.pushed == "" {
		t.Error("image was not pushed despite the tag being absent from ECR")
	}
}

// An inconclusive lookup fails the deploy. Guessing either way would defeat the
// point of a tag that identifies its source: assuming "absent" wastes a build,
// assuming "present" deploys a tag that may not exist, and both decide silently
// what the design settles by fact.
func TestDeploy_InconclusiveImageLookupFails(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, _ := deployFixture(true)
	registry.imageExistsErr = errors.New("ExpiredTokenException")

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
	if builder.called || registry.pushed != "" || cluster.params != nil {
		t.Error("an inconclusive tag lookup still built, pushed, or deployed")
	}
}

// A preview must report reuse for the same reason the real run does: it is the
// difference between a deploy that takes minutes and one that takes seconds.
func TestDeploy_DryRunReportsImageReuse(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, _ := deployFixture(true)
	registry.imageExists = true

	summary, err := d.Deploy(context.Background(), appWith(root, true), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if !strings.Contains(res.Reason, "reuse the existing image") {
		t.Errorf("reason = %q, want it to report image reuse", res.Reason)
	}
	if builder.called || registry.pushed != "" || cluster.params != nil {
		t.Error("dry run built, pushed, or deployed")
	}
}

// --- clean working tree required -------------------------------------------

// TestDeploy_RefusesDirtyWorkingTree is what makes the tag trustworthy. The tag
// is the HEAD SHA, so uncommitted edits are invisible in it: allowing a dirty
// deploy would either bake unversioned source into a tag that claims to be the
// commit, or — once the commit has been built before — reuse the older image and
// silently validate code the developer had just changed.
func TestDeploy_RefusesDirtyWorkingTree(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, registry, cluster, prov := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWithGit(root, false, gitDirty()), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "uncommitted changes") {
		t.Errorf("reason = %q, want it to name the uncommitted changes", res.Reason)
	}
	// Nothing may happen: not a build, not a push, not a rollout, and above all
	// not a cluster or kubeconfig change, since the deploy never had a valid tag.
	if builder.called || registry.pushed != "" || cluster.params != nil {
		t.Error("a dirty working tree still built, pushed, or deployed")
	}
	if prov.kubeconfig != nil || prov.createdSpec != nil {
		t.Error("a dirty working tree still touched the cluster")
	}
}

// The guard applies under --dry-run too. A preview of a dirty tree would
// otherwise print a plan naming a tag that does not describe the working tree,
// which is worse than no preview.
func TestDeploy_DryRunAlsoRefusesDirtyWorkingTree(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWithGit(root, true, gitDirty()), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
}

// A working-tree query that fails is not a clean tree. Treating an error as
// clean would reopen the hole the guard exists to close.
func TestDeploy_UnreadableWorkingTreeFails(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, builder, _, _, _ := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWithGit(root, false, gitStatusFails()), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
	if builder.called {
		t.Error("built despite being unable to determine whether the tree was clean")
	}
}

// --- fixed destination ------------------------------------------------------

// TestDeploy_DestinationDerivedFromServiceName pins that the ECR repository,
// namespace, and Helm release all come from the controller name and nothing
// else. The namespace is the load-bearing one: the cluster binds credentials
// through a pod identity association keyed on a single (namespace, service
// account) pair, so an install anywhere else starts with no credentials.
func TestDeploy_DestinationDerivedFromServiceName(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	d, _, registry, cluster, prov := deployFixture(true)

	if _, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{}); err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	if cluster.params == nil {
		t.Fatal("controller was not deployed")
	}
	if cluster.params.Namespace != Namespace {
		t.Errorf("namespace = %q, want the fixed %q", cluster.params.Namespace, Namespace)
	}
	if cluster.params.Release != "ack-ecr-controller" {
		t.Errorf("release = %q, want ack-ecr-controller", cluster.params.Release)
	}
	// The ECR repository is named after the controller, which is also the tag
	// lookup's subject, so a divergent second repository cannot come about.
	if registry.lookedUp[0] != "ecr-controller" {
		t.Errorf("looked up repository %q, want ecr-controller", registry.lookedUp[0])
	}
	if !strings.HasSuffix(cluster.params.ImageRepo, "/ecr-controller") {
		t.Errorf("image repo = %q, want it to end in /ecr-controller", cluster.params.ImageRepo)
	}
	// The pod identity association must be keyed on the same namespace the chart
	// is installed into, or the deployed controller has no credentials.
	if len(prov.serviceAccts) != 1 || prov.serviceAccts[0][0] != Namespace {
		t.Errorf("service accounts ensured = %v, want one in %s", prov.serviceAccts, Namespace)
	}
}

// --- workspace layout -------------------------------------------------------

// TestDeploy_MissingTestInfraFailsBeforeAnyWork covers a workspace without
// test-infra. build-controller-image.sh reads the Go and base-image versions
// from test-infra's build_config.yaml, so without it the deploy used to get as
// far as the image build, after touching the cluster and ECR, and stop with a
// bare `pushd: ... test-infra: No such file or directory`.
func TestDeploy_MissingTestInfraFailsBeforeAnyWork(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	if err := os.RemoveAll(filepath.Join(root, testInfraDirName)); err != nil {
		t.Fatalf("removing test-infra: %v", err)
	}
	for _, dryRun := range []bool{false, true} {
		d, builder, registry, cluster, prov := deployFixture(true)
		summary, err := d.Deploy(context.Background(), appWith(root, dryRun), "ecr", Options{})
		if err != nil {
			t.Fatalf("dry-run=%v: Deploy returned error: %v", dryRun, err)
		}
		res := only(t, summary)
		if res.Outcome != workspace.OutcomeFailed {
			t.Fatalf("dry-run=%v: outcome = %q, want failed; reason: %s", dryRun, res.Outcome, res.Reason)
		}
		if !strings.Contains(res.Reason, testInfraBuildConfig) {
			t.Errorf("dry-run=%v: reason = %q, want it to name %s", dryRun, res.Reason, testInfraBuildConfig)
		}
		if builder.called || registry.pushed != "" || cluster.params != nil || prov.kubeconfig != nil {
			t.Errorf("dry-run=%v: a workspace without test-infra still built, pushed, deployed, or touched the cluster", dryRun)
		}
	}
}

// TestDeploy_RefusesCodeGeneratorOutsideWorkspaceRoot covers a code-generator
// symlinked in from another workspace. The image build's Docker context is the
// code-generator's parent with symlinks resolved, so the deploy would build the
// other workspace's controller and push it under this checkout's tag.
func TestDeploy_RefusesCodeGeneratorOutsideWorkspaceRoot(t *testing.T) {
	root := workspaceWithController(t, "ecr-controller")
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, codegenDirName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, codegenDirName)); err != nil {
		t.Fatalf("removing code-generator: %v", err)
	}
	if err := os.Symlink(filepath.Join(other, codegenDirName), filepath.Join(root, codegenDirName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	d, builder, registry, cluster, prov := deployFixture(true)

	summary, err := d.Deploy(context.Background(), appWith(root, false), "ecr", Options{})
	if err != nil {
		t.Fatalf("Deploy returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed; reason: %s", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "outside the workspace root") {
		t.Errorf("reason = %q, want it to say the code-generator is outside the workspace root", res.Reason)
	}
	if builder.called || registry.pushed != "" || cluster.params != nil || prov.kubeconfig != nil {
		t.Error("a misplaced code-generator still built, pushed, deployed, or touched the cluster")
	}
}
