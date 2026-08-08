package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

const (
	// flagClusterVersion pins the Kubernetes version of the development cluster
	// when deploy has to create it.
	flagClusterVersion = "cluster-version"
	// flagClusterPolicyARN sets the IAM policies attached to the cluster's pod
	// identity role.
	flagClusterPolicyARN = "cluster-policy-arn"
	// flagResyncPeriod overrides the controller's default resync period, in
	// seconds, so reconcile-dependent behavior can be observed in a test session
	// instead of over the chart default of ten hours.
	flagResyncPeriod = "resync-period"
)

// newDeployCommand builds the `deploy` subcommand, which builds a single
// service controller from its local checkout and deploys it to the
// shared development cluster: it resolves the caller's AWS account, brings that
// cluster into existence when it is absent and repoints the local kubeconfig at
// it, ensures an ECR repository for the controller exists (creating it when
// absent), builds the controller image from the checked-out source with the
// code-generator's build-controller-image.sh script, pushes the image to ECR,
// and installs or upgrades the controller's Helm chart on that cluster pointing
// at the freshly built image.
//
// The cluster is fixed rather than selectable, and the current kubeconfig
// context is never used as-is, so a deploy cannot land on an unintended
// cluster.
//
// deploy declares the whole toolchain it drives — git, docker, aws, kubectl,
// helm and eksctl — so a missing one is a pre-flight error naming every absent
// tool at once. It does not fork, clone, push to GitHub, or open a pull request,
// so it needs no GitHub token or identity. The empty-service case is enforced by
// the deployer, which returns a *deployer.UsageError so the rule lives in one
// place and maps to a usage exit code.
func newDeployCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <service>",
		Short: "Build a controller from local source and deploy it to the development cluster",
		Long: "deploy builds a single service controller from its local checkout and " +
			"deploys it to the shared ACK development cluster (" + deployer.ClusterName + "). It " +
			"resolves your AWS account and region from the active credentials, brings the cluster into " +
			"existence when it is absent, ensures an ECR repository for the controller exists " +
			"(creating it in the current account when absent), builds the controller image from the " +
			"checked-out source with the code-generator's build-controller-image.sh script, pushes " +
			"the image to ECR, and runs `helm upgrade --install` to deploy the controller with the " +
			"freshly built image.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller).\n\n" +
			"Fixed destination: a controller always occupies the same place, and none of it is " +
			"selectable. The ECR repository is named after the controller (<service>-controller), the " +
			"install goes into the " + deployer.Namespace + " namespace, the Helm release is " +
			"ack-<service>-controller, and the controller runs under the " +
			deployer.SharedServiceAccount + " service account.\n\n" +
			"The namespace and service account are fixed together because they are the two halves of " +
			"one key: an EKS Pod Identity association is keyed on exactly one (namespace, service " +
			"account) pair and supports no wildcards, so credentials reach a controller only if its " +
			"install matches the association on both. One shared account lets a single association " +
			"cover every controller on the cluster; a controller deployed under any other account, " +
			"including the per-service one the chart would create, starts with no credentials and " +
			"exits with \"unable to determine account info\". Fixing the repository and release the " +
			"same way means two deploys of one controller can never disagree about where it lives. " +
			"Use --region to push to and configure a region other than the one resolved from your " +
			"AWS configuration.\n\n" +
			"Image identity: the image is always tagged with the controller's checked-out HEAD short " +
			"SHA, and the working tree must be clean -- a controller with uncommitted changes is " +
			"refused. The tag therefore identifies exactly the source the image was built from, which " +
			"makes the deploy deterministic: the same commit always produces the same tag, and the " +
			"tag always describes what is running. There is no tag override, because a tag chosen " +
			"independently of the source cannot carry that guarantee.\n\n" +
			"Image reuse: because the tag identifies the source, a tag already present in ECR proves " +
			"an image built from this exact source exists, so the build and push are skipped and that " +
			"image is deployed. Redeploying the same commit -- to retry a failed rollout, or to change " +
			"a chart value such as the resync period -- therefore costs a rollout rather than a full " +
			"image build, with no flag needed and no risk of deploying something other than the " +
			"checked-out commit. A registry lookup that fails is an error and the deploy stops: " +
			"build-or-reuse is decided from the registry or not at all.\n\n" +
			"Resync period: the chart resyncs every 36000 seconds (ten hours) by default, which " +
			"makes any behavior that only appears across reconciles — a perpetual delta from a " +
			"reference resolving to a different form than the API returns, or a server-side default " +
			"that is never captured into the spec — impractical to observe. Pass --resync-period 60 " +
			"to shorten it. Setting it on the deploy rather than a follow-up `helm upgrade` matters " +
			"because deploy installs the chart with its default values, so an override applied " +
			"beforehand is discarded.\n\n" +
			"Target cluster: every deploy targets " + deployer.ClusterName + " in the resolved " +
			"region. The cluster is not selectable and your current kubeconfig context is never used " +
			"as-is; deploy repoints the kubeconfig at " + deployer.ClusterName + " on every run, so a " +
			"deploy cannot land on a cluster you did not intend. When the cluster does not exist yet, " +
			"deploy creates it: an EKS Auto Mode cluster with an EKS Pod Identity association that " +
			"gives controllers in the target namespace AWS credentials. The cluster is meant to be " +
			"long-lived: that first run takes 15-25 minutes and creates billable AWS resources (an " +
			"EKS cluster, its VPC, and an IAM role), and every run after it reuses the cluster and " +
			"only fills in what is missing, so keep it in an account you are happy to leave running. " +
			"deploy checks for docker, aws, kubectl, helm and eksctl before it starts, so a " +
			"missing one is reported up front rather than partway through. The association attaches " +
			"AdministratorAccess by default, which suits a throwaway development account and nothing " +
			"else -- scope it down with --cluster-policy-arn anywhere else. Pin the Kubernetes " +
			"version with --cluster-version; by default eksctl chooses it.\n\n" +
			"Pass --dry-run to preview the steps without creating a cluster, changing your " +
			"kubeconfig, building, pushing, or modifying anything.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Tools: prereq.Git | prereq.Docker | prereq.AWS |
				prereq.Kubectl | prereq.Helm | prereq.Eksctl})
			if err != nil {
				return err
			}

			region, _ := cmd.Flags().GetString(flagRegion)
			clusterVersion, _ := cmd.Flags().GetString(flagClusterVersion)
			policyARNs, _ := cmd.Flags().GetStringSlice(flagClusterPolicyARN)
			resyncPeriod, _ := cmd.Flags().GetInt(flagResyncPeriod)

			// A missing service identifier is validated by the deployer (which returns a
			// *deployer.UsageError) so the rule is enforced in a single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.deployRun(cmdContext(cmd), a, service, deployer.Options{
				Region:         region,
				ClusterVersion: clusterVersion,
				PolicyARNs:     policyARNs,
				ResyncPeriod:   resyncPeriod,
			})
			if err != nil {
				return err
			}
			res.setLabeled(summary, "deployed")
			return nil
		},
	}
	cmd.Flags().String(flagRegion, "", "AWS region to push to and configure the controller for (default the resolved AWS config region)")
	cmd.Flags().String(flagClusterVersion, "", fmt.Sprintf("Kubernetes version used if the %s cluster has to be created (default eksctl's own default version)", deployer.ClusterName))
	cmd.Flags().StringSlice(flagClusterPolicyARN, nil, fmt.Sprintf("IAM policy ARNs attached to the cluster's pod identity role when it has to be created; repeat or comma-separate for several (default %q, appropriate only for a throwaway development account)", deployer.DefaultPolicyARN))
	cmd.Flags().Int(flagResyncPeriod, 0, "controller default resync period in seconds, set as reconcile.defaultResyncPeriod on the chart (default the chart's own 36000, ten hours); use a small value such as 60 to observe behavior that only appears across reconciles")
	return cmd
}
