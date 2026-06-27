package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// flagJSON is the `status` flag that requests machine-readable JSON output
// (Requirement 6.8).
const flagJSON = "json"

// newStatusCommand builds the `status` subcommand, which reports the state of
// every Managed_Repository under the workspace root.
//
// status reads local git state only; it declares the git prerequisite
// (Requirements 1.1, 1.7) and needs neither a token nor an identity. The
// Workspace_Inspector renders its own output (a table, or a single JSON
// document with --json) to the command's stdout, so it is capturable in tests.
// status is read-only and records no failures, so the Result it stashes never
// drives a non-zero exit code.
func newStatusCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the state of every managed repository",
		Long: "status lists each managed repository under the workspace root with its current " +
			"branch, whether its working tree is dirty, and how its default branch compares to " +
			"upstream (up to date, ahead, behind, or unavailable).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true})
			if err != nil {
				return err
			}
			jsonOut, _ := cmd.Flags().GetBool(flagJSON)
			summary, err := d.statusRun(cmdContext(cmd), a, jsonOut, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			res.set(summary)
			return nil
		},
	}
	cmd.Flags().Bool(flagJSON, false, "emit machine-readable JSON instead of a table")
	return cmd
}
