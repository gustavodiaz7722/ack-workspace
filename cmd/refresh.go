package cmd

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

// newRefreshCommand builds the `refresh` subcommand, which reconciles each
// managed repository to a clean, up-to-date default branch ready for
// development. For each repository it syncs the fork's default branch from
// upstream via the GitHub API, fetches all upstream tags, discards local
// changes, checks out the default branch (main), and resets it to match
// upstream.
//
// Because it syncs the fork via the GitHub API, refresh declares the git,
// token, and identity prerequisites: the token authorizes the merge-upstream
// call and the identity names the fork. The optional positional arguments
// select a subset of repositories to refresh.
//
// Reaching the baseline discards uncommitted changes and untracked files on the
// default branch, so refresh requires an interactive confirmation unless
// --dry-run or --yes is given. Work committed on other branches is left intact.
func newRefreshCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh [repos...]",
		Short: "Reconcile managed repositories to a clean, up-to-date main (destructive)",
		Long: "refresh brings each managed repository to a known-good baseline ready for " +
			"development: it syncs your fork's default branch (main) from upstream via the GitHub " +
			"API, fetches every upstream tag, discards local changes, checks out main, and resets " +
			"main to exactly match upstream (and therefore your fork). With no arguments it " +
			"refreshes every managed repository; positional arguments restrict it to the named " +
			"subset.\n\n" +
			"This is destructive: uncommitted changes and untracked files are permanently lost and " +
			"a diverged local main is reset. Work committed on other branches is left intact. Use " +
			"--dry-run to preview and --yes to skip the confirmation prompt.",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{Git: true, Token: true, Identity: true})
			if err != nil {
				return err
			}

			yes, _ := cmd.Flags().GetBool(flagYes)

			// Require explicit confirmation for the destructive action unless
			// the user opted out with --yes or is only previewing with --dry-run.
			if !a.DryRun && !yes {
				ok, cerr := confirmRefresh(cmd, args)
				if cerr != nil {
					return cerr
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted; nothing was refreshed.")
					return nil
				}
			}

			// args is the optional subset of repositories to refresh; an empty
			// slice means "all managed repositories".
			summary, err := d.refreshRun(cmdContext(cmd), a, args)
			if err != nil {
				return err
			}
			res.setLabeled(summary, "refreshed")
			return nil
		},
	}
	cmd.Flags().Bool(flagYes, false, "skip the confirmation prompt")
	return cmd
}

// confirmRefresh prints a warning describing the destructive action and reads a
// confirmation line from the command's input. It returns true only when the user
// types "yes". Input/output go through the cobra command so tests can drive them.
func confirmRefresh(cmd *cobra.Command, identifiers []string) (bool, error) {
	target := describeRefreshTargets(identifiers)
	fmt.Fprintf(cmd.OutOrStdout(),
		"WARNING: this permanently discards uncommitted changes and untracked files in %s\n"+
			"and resets main to match upstream. Work committed on other branches is kept.\n"+
			"Type 'yes' to continue: ",
		target)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		// EOF or read error with no input: treat as "no".
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

// describeRefreshTargets renders a human description of what will be refreshed.
func describeRefreshTargets(identifiers []string) string {
	if len(identifiers) == 0 {
		return "ALL managed repositories in the workspace"
	}
	return strings.Join(identifiers, ", ")
}
