package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// newInitCommand builds the `init` subcommand, which bootstraps a contributor
// workspace by forking, cloning, and configuring the core Common_Repositories.
//
// init performs both git operations and GitHub API operations and needs the
// contributor's identity to name the forks, so it declares all three
// prerequisites (Requirements 1.1, 1.3, 1.5): the prerequisite Check runs before
// the Workspace_Initializer touches GitHub or the filesystem, and an aggregated
// error names every missing prerequisite at once (Requirement 1.7).
func newInitCommand(d deps, res *Result) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Fork, clone, and configure the core ACK repositories",
		Long: "init bootstraps a contributor workspace: it forks the core ACK repositories " +
			"(runtime, code-generator, test-infra, and ack-dev-skills) under your GitHub account, " +
			"clones them into the workspace root, and configures the origin and upstream remotes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true, Token: true, Identity: true})
			if err != nil {
				return err
			}
			summary, err := d.initRun(cmdContext(cmd), a)
			if err != nil {
				return err
			}
			res.set(summary)
			return nil
		},
	}
}
