package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/attributor"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

const (
	// flagRef names the git ref the remote build generates from.
	flagRef = "ref"
	// flagRepo overrides the repository URL the remote build clones.
	flagRepo = "repo"
	// flagUpstream targets the aws-controllers-k8s organization instead of the
	// contributor's fork.
	flagUpstream = "upstream"
	// flagOutput overrides where the generated document is written.
	flagOutput = "output"
	// flagProject overrides the CodeBuild project name.
	flagProject = "project"
	// flagRole overrides the IAM role CodeBuild assumes.
	flagRole = "role"
	// flagBucket overrides the S3 bucket the document is staged in.
	flagBucket = "bucket"
	// flagImage overrides the CodeBuild container image.
	flagImage = "image"
	// flagGoVersion overrides the golang runtime requested in the buildspec.
	flagGoVersion = "go-version"
	// flagTimeout bounds how long a single build is waited on.
	flagTimeout = "timeout"
)

// newAttributionCommand builds the `attribution` subcommand, which regenerates
// a controller's ATTRIBUTION.md by running the upstream attribution-gen tool on
// ephemeral AWS CodeBuild compute.
//
// The remote compute is a requirement, not an optimization: generating the
// document walks the module dependency graph, which needs the public Go module
// proxy, and those fetches are blocked from inside the Amazon corporate
// network. Running the generator on CodeBuild is what makes the command work
// from a corporate desktop at all.
//
// The command reads the controller's checked-out branch and lists refs on the
// remote, so it declares the git prerequisite. It deliberately does not declare
// the GitHub identity prerequisite: an identity is only needed to derive the
// fork URL, and --upstream or --repo remove that need, so declaring it here
// would wrongly block those paths. When an identity is genuinely required and
// missing, the attributor reports it with a targeted, actionable error.
//
// AWS credentials come from the default chain. Note that this command
// provisions resources in the caller's AWS account on first use (an IAM role,
// an S3 bucket, and a CodeBuild project); --dry-run previews all of it without
// creating anything.
func newAttributionCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attribution <service>... | all",
		Short: "Generate a controller's ATTRIBUTION.md on ephemeral CodeBuild compute",
		Long: "attribution regenerates a service controller's ATTRIBUTION.md by running the " +
			"upstream attribution-gen tool on ephemeral AWS CodeBuild compute and writing the " +
			"result into your local checkout.\n\n" +
			"The work runs remotely because it has to: generating the document walks the module " +
			"dependency graph and fetches every dependency from the public Go module proxy, which " +
			"is blocked from inside the Amazon corporate network. CodeBuild runs the generator " +
			"outside that network.\n\n" +
			"By default the build clones your fork at the controller's currently checked-out " +
			"branch, so push your work before running it; the build reads the remote and cannot " +
			"see unpushed commits. Use --ref to name a branch, tag, or 'pr/123', --upstream to " +
			"generate from the aws-controllers-k8s organization, or --repo for an arbitrary " +
			"repository.\n\n" +
			"Services may be bare aliases (ecr), full forms (ecr-controller), or 'all' for every " +
			"managed controller. Note that 'all' starts one build per controller, so it is both " +
			"slow and billable.\n\n" +
			"On first use this provisions an IAM role, an S3 bucket, and a CodeBuild project in " +
			"your AWS account; subsequent runs reuse them. Pass --dry-run to preview everything " +
			"without creating any resource or starting any build.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Tools: prereq.Git})
			if err != nil {
				return err
			}

			ref, _ := cmd.Flags().GetString(flagRef)
			repo, _ := cmd.Flags().GetString(flagRepo)
			upstream, _ := cmd.Flags().GetBool(flagUpstream)
			output, _ := cmd.Flags().GetString(flagOutput)
			region, _ := cmd.Flags().GetString(flagRegion)
			project, _ := cmd.Flags().GetString(flagProject)
			role, _ := cmd.Flags().GetString(flagRole)
			bucket, _ := cmd.Flags().GetString(flagBucket)
			image, _ := cmd.Flags().GetString(flagImage)
			goVersion, _ := cmd.Flags().GetString(flagGoVersion)
			timeout, _ := cmd.Flags().GetDuration(flagTimeout)

			opts := attributor.Options{
				Ref:      ref,
				RepoURL:  repo,
				Upstream: upstream,
				Output:   output,
				Timeout:  timeout,
				Infra: attributor.Infrastructure{
					Project:   project,
					Role:      role,
					Bucket:    bucket,
					Image:     image,
					GoVersion: goVersion,
				},
			}

			// An empty identifier list is validated by the attributor so the rule lives
			// in one place and maps to the usage exit code.
			summary, err := d.attributionRun(cmdContext(cmd), a, args, opts, region, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			res.setLabeled(summary, "generated")
			return nil
		},
	}
	cmd.Flags().String(flagRef, "", "git ref to generate from: branch, tag, or pr/<number> (default: the controller's checked-out branch)")
	cmd.Flags().String(flagRepo, "", "repository URL the build clones (default: your fork of the controller)")
	cmd.Flags().Bool(flagUpstream, false, "generate from the "+attributor.UpstreamOwner+" organization instead of your fork")
	cmd.Flags().String(flagOutput, "", "write the document here instead of <controller>/ATTRIBUTION.md (single controller only)")
	cmd.Flags().String(flagRegion, "", "AWS region for CodeBuild (default: the resolved AWS config region)")
	cmd.Flags().String(flagProject, "", "CodeBuild project name (default \""+attributor.DefaultProject+"\")")
	cmd.Flags().String(flagRole, "", "IAM role CodeBuild assumes (default \""+attributor.DefaultRole+"\")")
	cmd.Flags().String(flagBucket, "", "S3 bucket used to stage the document (default: an account-scoped bucket)")
	cmd.Flags().String(flagImage, "", "CodeBuild container image (default \""+attributor.DefaultImage+"\")")
	cmd.Flags().String(flagGoVersion, "", "golang runtime version for the build; must exist in the image (default \""+attributor.DefaultGoVersion+"\")")
	cmd.Flags().Duration(flagTimeout, 20*time.Minute, "how long to wait for a single build to finish")
	return cmd
}
