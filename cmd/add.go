package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// newAddCommand builds the `add` subcommand, which forks, clones, and configures
// one or more Service_Controller_Repositories named by the supplied service
// identifiers.
//
// add performs git and GitHub API operations and needs the contributor's
// identity, so it declares all three prerequisites (Requirements 1.1, 1.3, 1.5,
// 1.7), enforced before the Controller_Adder runs.
//
// The empty-identifier case (Requirement 4.2) is intentionally NOT enforced via
// cobra Args. Delegating to the Controller_Adder, which returns an
// *adder.UsageError for an empty list, keeps the "at least one identifier"
// requirement enforced in a single place; that typed usage error propagates out
// of RunE so the entrypoint (Task 13.3) can map it to a usage exit code.
func newAddCommand(d deps, res *Result) *cobra.Command {
	return &cobra.Command{
		Use:   "add [identifiers...|all]",
		Short: "Fork, clone, and configure service controller repositories",
		Long: "add forks the named service controller repositories under your GitHub account, " +
			"clones them into the workspace root, and configures the origin and upstream remotes. " +
			"Each identifier may be a bare service alias (s3) or its full form (s3-controller).\n\n" +
			"Pass the special identifier 'all' to set up every controller repository available in " +
			"the aws-controllers-k8s organization. When 'all' is given it supersedes any other " +
			"identifiers.",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true, Token: true, Identity: true})
			if err != nil {
				return err
			}
			// args are the service identifiers. An empty list is rejected by the
			// Controller_Adder (Requirement 4.2) rather than by cobra so the rule
			// lives in one place.
			summary, err := d.addRun(cmdContext(cmd), a, args)
			if err != nil {
				return err
			}
			res.setLabeled(summary, "added")
			return nil
		},
	}
}
