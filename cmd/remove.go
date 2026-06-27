package cmd

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
	"github.com/aws-controllers-k8s/ack-workspace/internal/remover"
)

const (
	// flagYes skips the interactive confirmation prompt.
	flagYes = "yes"
	// flagKeepFork deletes only the local clone, leaving the GitHub fork intact.
	flagKeepFork = "keep-fork"
	// flagForce removes a repository even if its working tree is dirty.
	flagForce = "force"
)

// newRemoveCommand builds the `remove` subcommand, the inverse of `add`: it
// permanently deletes a service controller's local clone and its GitHub fork.
//
// This is destructive and irreversible (a deleted fork cannot be recovered), so
// unless --dry-run or --yes is given the command requires an interactive
// confirmation. It deletes git via the filesystem and the fork via the GitHub
// API, and inspects the working tree for the dirty-check, so it declares the
// git, token, and identity prerequisites (Requirements 1.1, 1.3, 1.5, 1.7). The
// remover refuses to delete anything owned by the upstream organization.
func newRemoveCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove [identifiers...|all]",
		Short: "Delete a controller's local clone and GitHub fork (destructive)",
		Long: "remove permanently deletes the local clone and the GitHub fork of each named " +
			"service controller. Each identifier may be a bare service alias (s3) or its full " +
			"form (s3-controller). Pass 'all' to remove every managed controller found under the " +
			"workspace root.\n\n" +
			"This is destructive and cannot be undone: the fork is deleted from your GitHub " +
			"account. Repositories with uncommitted changes are skipped unless --force is given. " +
			"Use --dry-run to preview, --keep-fork to delete only the local clone, and --yes to " +
			"skip the confirmation prompt.",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true, Token: true, Identity: true})
			if err != nil {
				return err
			}

			keepFork, _ := cmd.Flags().GetBool(flagKeepFork)
			force, _ := cmd.Flags().GetBool(flagForce)
			yes, _ := cmd.Flags().GetBool(flagYes)

			// Require explicit confirmation for the destructive action, unless
			// the user opted out with --yes or is only previewing with --dry-run.
			if !a.DryRun && !yes {
				ok, cerr := confirmRemoval(cmd, args, keepFork)
				if cerr != nil {
					return cerr
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted; nothing was removed.")
					return nil
				}
			}

			summary, err := d.removeRun(cmdContext(cmd), a, args, remover.Options{KeepFork: keepFork, Force: force})
			if err != nil {
				return err
			}
			res.setLabeled(summary, "removed")
			return nil
		},
	}
	cmd.Flags().Bool(flagYes, false, "skip the confirmation prompt")
	cmd.Flags().Bool(flagKeepFork, false, "delete only the local clone; keep the GitHub fork")
	cmd.Flags().Bool(flagForce, false, "remove even if the working tree has uncommitted changes")
	return cmd
}

// confirmRemoval prints a warning describing the destructive action and reads a
// confirmation line from the command's input. It returns true only when the user
// types "yes". Input/output go through the cobra command so tests can drive them.
func confirmRemoval(cmd *cobra.Command, identifiers []string, keepFork bool) (bool, error) {
	target := describeTargets(identifiers)
	what := "local clone(s) and GitHub fork(s)"
	if keepFork {
		what = "local clone(s) (forks kept)"
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"WARNING: this permanently deletes the %s for %s.\nA deleted fork cannot be recovered. Type 'yes' to continue: ",
		what, target)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		// EOF or read error with no input: treat as "no".
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

// describeTargets renders a human description of what will be removed.
func describeTargets(identifiers []string) string {
	for _, id := range identifiers {
		if strings.EqualFold(strings.TrimSpace(id), "all") {
			return "ALL managed controllers in the workspace"
		}
	}
	if len(identifiers) == 0 {
		return "the selected controllers"
	}
	return strings.Join(identifiers, ", ")
}
