package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

const (
	// flagVersion is the required `release` flag naming the version to cut.
	flagVersion = "version"
	// flagBaseBranch overrides the branch a release is cut from (default main).
	flagBaseBranch = "base-branch"
	// flagSkipPR pushes the release branch but does not open a pull request.
	flagSkipPR = "skip-pr"
	// flagPRBody overrides the default pull request body.
	flagPRBody = "pr-body"
)

// newReleaseCommand builds the `release` subcommand, which cuts a release for a
// single service controller: it updates the controller's base branch from
// upstream, creates a "release-<version>" branch, regenerates the release
// artifacts via the code-generator script, commits and pushes them to the
// contributor's fork, and (unless --skip-pr) opens a pull request against
// upstream.
//
// release performs git operations, pushes to the fork, and opens a GitHub pull
// request, so it declares the git, token, and identity prerequisites
// (Requirements 1.1, 1.3, 1.5, 1.7). The empty-service and invalid-version cases
// are enforced by the Controller_Releaser, which returns a *releaser.UsageError
// so the rule lives in one place and maps to a usage exit code.
func newReleaseCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release <service> --version <version>",
		Short: "Cut a release for a service controller and open a pull request",
		Long: "release mechanizes the ACK controller release workflow for a single service " +
			"controller. It updates the controller's base branch from upstream, creates a branch " +
			"named release-<version>, regenerates the release artifacts with the code-generator's " +
			"build-controller-release.sh script (RELEASE_VERSION set to the requested version), " +
			"commits the artifacts, pushes the branch to your fork, and opens a pull request " +
			"against the upstream repository.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller). The version " +
			"is normalized to carry a leading 'v' (1.0.1 and v1.0.1 are equivalent). Pass --skip-pr " +
			"to push the branch without opening a pull request, --base-branch to cut from a branch " +
			"other than main, --pr-body to override the generated pull request body, and --dry-run " +
			"to preview the steps without making any change.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true, Token: true, Identity: true})
			if err != nil {
				return err
			}

			version, _ := cmd.Flags().GetString(flagVersion)
			base, _ := cmd.Flags().GetString(flagBaseBranch)
			skipPR, _ := cmd.Flags().GetBool(flagSkipPR)
			prBody, _ := cmd.Flags().GetString(flagPRBody)

			// A missing service identifier or version is validated by the
			// Controller_Releaser (which returns a *releaser.UsageError) so the
			// rule is enforced in a single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.releaseRun(cmdContext(cmd), a, service, version, base, skipPR, prBody)
			if err != nil {
				return err
			}
			res.setLabeled(summary, "released")
			return nil
		},
	}
	cmd.Flags().String(flagVersion, "", "release version to cut, for example v1.0.1 (required)")
	cmd.Flags().String(flagBaseBranch, "", "branch to cut the release from (default \"main\")")
	cmd.Flags().Bool(flagSkipPR, false, "push the release branch but do not open a pull request")
	cmd.Flags().String(flagPRBody, "", "pull request body to use instead of the generated default")
	return cmd
}
