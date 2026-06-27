package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// newConfigCommand builds the `config` subcommand group for viewing and
// persisting configuration. It performs neither git nor GitHub API operations
// and requires no identity, so it declares no prerequisites (the Need table's
// `config` row is empty) and never invokes the prerequisite Checker.
//
// The persistent flags defined on the root command (--github-user,
// --workspace-root, --prefix, --concurrency, --token) are inherited here, so the
// same precedence (flag > env > persisted > default) the Configuration_Manager
// applies elsewhere governs `config set`/`get`.
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and persist ack-workspace configuration",
		Long: "config manages the persisted settings stored at $HOME/.ack-workspace/config. " +
			"Use 'config set' to save values, 'config get' to print the resolved values, and " +
			"'config path' to print the configuration file path.",
	}
	cmd.AddCommand(newConfigSetCommand(), newConfigGetCommand(), newConfigPathCommand())
	return cmd
}

// newConfigSetCommand builds `config set`, which persists the GitHub identity,
// workspace root, fork prefix, and concurrency to the configuration file
// (Requirement 2.1). It resolves the current configuration (which already
// applies any values supplied via flags or environment with the correct
// precedence), validates the resolved concurrency so an out-of-range value is
// never persisted (Requirement 7.3), and saves it. The token is never written
// to disk (Requirement 2.5) because Manager.Save excludes it.
func newConfigSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set",
		Short: "Persist configuration values to the config file",
		Long: "set resolves the effective configuration from your flags, environment, and any " +
			"existing config file, then writes the GitHub identity, workspace root, fork prefix, " +
			"and concurrency to $HOME/.ack-workspace/config. The GitHub token is never persisted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr := config.NewManager()
			cfg, err := mgr.Resolve(buildSource(cmd))
			if err != nil {
				return err
			}
			if err := validateConcurrency(cfg.Concurrency); err != nil {
				return err
			}
			if err := mgr.Save(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved configuration to %s\n", mgr.Path())
			return nil
		},
	}
}

// newConfigGetCommand builds `config get`, which prints the resolved
// configuration values (flag > env > persisted > default) so the contributor
// can see exactly what the tool would use for an invocation. The token is not
// printed.
func newConfigGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the resolved configuration values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.NewManager().Resolve(buildSource(cmd))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "github-user:    %s\n", cfg.GitHubUser)
			fmt.Fprintf(out, "workspace-root: %s\n", cfg.WorkspaceRoot)
			fmt.Fprintf(out, "prefix:         %s\n", cfg.RepoPrefix)
			fmt.Fprintf(out, "concurrency:    %d\n", cfg.Concurrency)
			return nil
		},
	}
}

// newConfigPathCommand builds `config path`, which prints the configuration file
// path ($HOME/.ack-workspace/config). It requires no resolution and works even
// when no configuration file exists yet.
func newConfigPathCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), config.NewManager().Path())
			return nil
		},
	}
}
