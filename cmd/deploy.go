package cmd

import (
	"github.com/spf13/cobra"

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
)

// newDeployCommand builds the `deploy` subcommand, which builds a single service
// controller from its local implementation branch and deploys it to the cluster
// named by the current kubeconfig context: it resolves the target cluster and
// the caller's AWS account, ensures an ECR repository for the controller exists
// (creating it when absent), builds the controller image from the checked-out
// source with the code-generator's build-controller-image.sh script, pushes the
// image to ECR, and installs or upgrades the controller's Helm chart on the
// current cluster pointing at the freshly built image.
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
		Short: "Build a controller from local source and deploy it to the current cluster",
		Long: "deploy builds a single service controller from its local implementation branch and " +
			"deploys it to the cluster named by your current kubeconfig context. It resolves the " +
			"target cluster from `kubectl config current-context` and your AWS account and region " +
			"from the active credentials, ensures an ECR repository for the controller exists " +
			"(creating it in the current account when absent), builds the controller image from the " +
			"checked-out source with the code-generator's build-controller-image.sh script, pushes " +
			"the image to ECR, and runs `helm upgrade --install` to deploy the controller with the " +
			"freshly built image.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller). By default the " +
			"image is tagged with the controller's checked-out HEAD short SHA and the ECR repository " +
			"is named after the controller; override these with --image-tag and --repository. Use " +
			"--namespace to install into a namespace other than ack-system and --region to push to " +
			"and configure a region other than the one resolved from your AWS configuration.\n\n" +
			"deploy installs onto whatever cluster your current kubeconfig context points at; verify " +
			"the context before running. Pass --dry-run to preview the steps without building, " +
			"pushing, or modifying the cluster.",
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

			// A missing service identifier is validated by the Controller_Deployer
			// (which returns a *deployer.UsageError) so the rule is enforced in a
			// single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.deployRun(cmdContext(cmd), a, service, namespace, imageTag, repository, region)
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
	return cmd
}
