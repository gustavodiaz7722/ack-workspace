package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

const (
	// flagNamespace overrides the Kubernetes namespace the controller is
	// installed into (default "ack-system").
	flagNamespace = "namespace"
	// flagImageTag overrides the tag applied to the built image (default the
	// controller's checked-out HEAD short SHA).
	flagImageTag = "image-tag"
	// flagRepository overrides the ECR repository name (default
	// "<service>-controller").
	flagRepository = "repository"
	// flagServiceAccount names an existing service account for the controller to
	// run under, instead of the one the chart creates.
	flagServiceAccount = "service-account"
	// flagClusterVersion pins the Kubernetes version of the development cluster
	// when deploy has to create it.
	flagClusterVersion = "cluster-version"
	// flagClusterPolicyARN sets the IAM policies attached to the cluster's pod
	// identity role.
	flagClusterPolicyARN = "cluster-policy-arn"
)

// newDeployCommand builds the `deploy` subcommand, which builds a single service
// controller from its local implementation branch and deploys it to the shared
// development cluster: it resolves the caller's AWS account, brings that cluster
// into existence when it is absent and repoints the local kubeconfig at it,
// ensures an ECR repository for the controller exists (creating it when absent),
// builds the controller image from the checked-out source with the
// code-generator's build-controller-image.sh script, pushes the image to ECR, and
// installs or upgrades the controller's Helm chart on that cluster pointing at
// the freshly built image.
//
// The cluster is fixed rather than selectable, and the current kubeconfig context
// is never used as-is, so a deploy cannot land on an unintended cluster.
//
// deploy reads the controller's checked-out HEAD to tag the image, so it
// declares the git prerequisite. It does not fork, clone, push to GitHub, or
// open a pull request, so it needs no GitHub token or identity. The docker, aws,
// kubectl, and helm executables must be available at runtime; a missing tool is
// reported as a failed Result. The empty-service case is enforced by the
// Controller_Deployer, which returns a *deployer.UsageError so the rule lives in
// one place and maps to a usage exit code.
func newDeployCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <service>",
		Short: "Build a controller from local source and deploy it to the development cluster",
		Long: "deploy builds a single service controller from its local implementation branch and " +
			"deploys it to the shared ACK development cluster (" + deployer.ClusterName + "). It " +
			"resolves your AWS account and region from the active credentials, brings the cluster into " +
			"existence when it is absent, ensures an ECR repository for the controller exists " +
			"(creating it in the current account when absent), builds the controller image from the " +
			"checked-out source with the code-generator's build-controller-image.sh script, pushes " +
			"the image to ECR, and runs `helm upgrade --install` to deploy the controller with the " +
			"freshly built image.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller). By default the " +
			"image is tagged with the controller's checked-out HEAD short SHA and the ECR repository " +
			"is named after the controller; override these with --image-tag and --repository. Use " +
			"--namespace to install into a namespace other than ack-system and --region to push to " +
			"and configure a region other than the one resolved from your AWS configuration.\n\n" +
			"Target cluster: every deploy targets " + deployer.ClusterName + " in the resolved " +
			"region. The cluster is not selectable and your current kubeconfig context is never used " +
			"as-is; deploy repoints the kubeconfig at " + deployer.ClusterName + " on every run, so a " +
			"deploy cannot land on a cluster you did not intend. When the cluster does not exist yet, " +
			"deploy creates it: an EKS Auto Mode cluster with an EKS Pod Identity association that " +
			"gives controllers in the target namespace AWS credentials. That first run is a one-time " +
			"bootstrap which takes 15-25 minutes and creates billable AWS resources (an EKS cluster, " +
			"its VPC, and an IAM role); later deploys reuse it and only fill in what is missing. " +
			"eksctl must be on your PATH for the bootstrap. The association attaches " +
			"AdministratorAccess by default, which suits a throwaway development account and nothing " +
			"else -- scope it down with --cluster-policy-arn anywhere else. Pin the Kubernetes " +
			"version with --cluster-version; by default eksctl chooses it.\n\n" +
			"Service account: the controller runs under the " + deployer.SharedServiceAccount + " " +
			"service account the cluster binds credentials to, because pod identity associations are " +
			"keyed on a single (namespace, service account) pair and cannot cover a namespace as a " +
			"whole. The chart's own service account would carry no credential binding, leaving the " +
			"controller to exit at startup with \"unable to determine account info\". Pass " +
			"--service-account to use a different account and an association is created for it.\n\n" +
			"Pass --dry-run to preview the steps without creating a cluster, changing your " +
			"kubeconfig, building, pushing, or modifying anything.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true})
			if err != nil {
				return err
			}

			namespace, _ := cmd.Flags().GetString(flagNamespace)
			imageTag, _ := cmd.Flags().GetString(flagImageTag)
			repository, _ := cmd.Flags().GetString(flagRepository)
			region, _ := cmd.Flags().GetString(flagRegion)
			serviceAccount, _ := cmd.Flags().GetString(flagServiceAccount)
			clusterVersion, _ := cmd.Flags().GetString(flagClusterVersion)
			policyARNs, _ := cmd.Flags().GetStringSlice(flagClusterPolicyARN)

			// A missing service identifier is validated by the Controller_Deployer
			// (which returns a *deployer.UsageError) so the rule is enforced in a
			// single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.deployRun(cmdContext(cmd), a, service, deployer.Options{
				Namespace:      namespace,
				ImageTag:       imageTag,
				Repository:     repository,
				Region:         region,
				ServiceAccount: serviceAccount,
				ClusterVersion: clusterVersion,
				PolicyARNs:     policyARNs,
			})
			if err != nil {
				return err
			}
			res.setLabeled(summary, "deployed")
			return nil
		},
	}
	cmd.Flags().String(flagNamespace, "", "Kubernetes namespace to install the controller into (default \"ack-system\")")
	cmd.Flags().String(flagImageTag, "", "image tag to build and deploy (default the controller's HEAD short SHA)")
	cmd.Flags().String(flagRepository, "", "ECR repository name (default \"<service>-controller\")")
	cmd.Flags().String(flagRegion, "", "AWS region to push to and configure the controller for (default the resolved AWS config region)")
	cmd.Flags().String(flagServiceAccount, "", fmt.Sprintf("run the controller under this service account, creating a pod identity association for it (default %q, the account the cluster already binds credentials to)", deployer.SharedServiceAccount))
	cmd.Flags().String(flagClusterVersion, "", fmt.Sprintf("Kubernetes version used if the %s cluster has to be created (default eksctl's own default version)", deployer.ClusterName))
	cmd.Flags().StringSlice(flagClusterPolicyARN, nil, fmt.Sprintf("IAM policy ARNs attached to the cluster's pod identity role when it has to be created; repeat or comma-separate for several (default %q, appropriate only for a throwaway development account)", deployer.DefaultPolicyARN))
	return cmd
}
