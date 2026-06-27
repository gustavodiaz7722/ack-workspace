package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// flagPush is the `sync` flag that requests pushing updated default branches to
// the contributor's Origin_Remote (Requirement 5.6).
const flagPush = "push"

// newSyncCommand builds the `sync` subcommand, which updates Managed_Repositories
// from their Upstream_Remote using fast-forward-only semantics and, optionally,
// pushes the updated branches to the contributor's fork.
//
// sync uses git remotes (which carry their own credentials) rather than the
// GitHub API, so it declares only the git prerequisite (Requirements 1.1, 1.7);
// it needs neither a token nor a configured identity. The optional positional
// arguments select a subset of repositories to synchronize (Requirement 5.5).
func newSyncCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync [repos...]",
		Short: "Update managed forks from upstream (fast-forward only)",
		Long: "sync fetches each managed repository's upstream remote and fast-forwards its " +
			"default branch when possible, skipping repositories with uncommitted changes or " +
			"diverged history so local work is never lost. With no arguments it synchronizes " +
			"every managed repository; positional arguments restrict it to the named subset.",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true})
			if err != nil {
				return err
			}
			push, _ := cmd.Flags().GetBool(flagPush)
			// args is the optional subset of repositories to synchronize; an
			// empty slice means "all managed repositories" (Requirement 5.5).
			summary, err := d.syncRun(cmdContext(cmd), a, args, push)
			if err != nil {
				return err
			}
			res.setLabeled(summary, "updated")
			return nil
		},
	}
	cmd.Flags().Bool(flagPush, false, "push each updated default branch to the origin remote (your fork)")
	return cmd
}
