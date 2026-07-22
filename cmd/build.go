package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// flagSDKVersion pins the aws-sdk-go version passed to the code-generator
// (default: read from the controller's ack-generate-metadata.yaml).
const flagSDKVersion = "sdk-version"

// newBuildCommand builds the `build` subcommand, which regenerates a single
// service controller's code from its local (checked-out) source by running the
// code-generator's `make build-controller` target: it locates the controller
// and the code-generator in the workspace, reports the controller's checked-out
// branch, and runs `make build-controller SERVICE=<alias>` in the code-generator
// directory with the environment overrides the build scripts need when the
// workspace root is not literally ".../aws-controllers-k8s".
//
// build reads the controller's checked-out branch, so it declares the git
// prerequisite. It does not fork, clone, push to GitHub, or open a pull request,
// so it needs no GitHub token or identity. The make and go toolchain must be
// available at runtime; a build failure is reported as a failed Result. The
// empty-service case is enforced by the Controller_Builder, which returns a
// *builder.UsageError so the rule lives in one place and maps to a usage exit
// code.
func newBuildCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build <service>",
		Short: "Regenerate a controller's code from local source with the code-generator",
		Long: "build regenerates a single service controller's code from its local implementation " +
			"branch by running the code-generator's `make build-controller` target. It locates the " +
			"controller and the code-generator in your workspace and runs " +
			"`make build-controller SERVICE=<alias>` in the code-generator directory against " +
			"whatever the controller repository currently has checked out; it never switches " +
			"branches or touches git history.\n\n" +
			"build wires up the environment overrides (RUNTIME_CRD_DIR, ACK_GENERATE_BIN_PATH, and " +
			"TEMPLATES_DIR) that the code-generator scripts otherwise resolve relative to a " +
			"workspace root named 'aws-controllers-k8s'. Setting them against your real workspace " +
			"root lets the build succeed from a non-standard root, which would otherwise fail with " +
			"'No such file or directory' or 'Unable to find an ack-generate binary'.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller). By default the " +
			"aws-sdk-go version is read from the controller's ack-generate-metadata.yaml; pass " +
			"--sdk-version to pin it. Pass --dry-run to print the command that would run without " +
			"executing it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true})
			if err != nil {
				return err
			}

			sdkVersion, _ := cmd.Flags().GetString(flagSDKVersion)

			// A missing service identifier is validated by the Controller_Builder
			// (which returns a *builder.UsageError) so the rule is enforced in a
			// single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.buildRun(cmdContext(cmd), a, service, sdkVersion)
			if err != nil {
				return err
			}
			res.setLabeled(summary, "built")
			return nil
		},
	}
	cmd.Flags().String(flagSDKVersion, "", "aws-sdk-go version to build with (default: read from ack-generate-metadata.yaml)")
	return cmd
}
