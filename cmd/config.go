package cmd

import (
	"fmt"
	"path/filepath"

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
// same precedence (flag > env > workspace-local file > global file > default) the
// configuration manager applies elsewhere governs `config set`/`get`.
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and persist ack-workspace configuration",
		Long: "config manages persisted settings. Settings live in a global file at " +
			"$HOME/.ack-workspace/config and, optionally, in a workspace-local file at " +
			"<workspace-root>/.ack-workspace/config that overrides the global one for work " +
			"anywhere inside that tree. Use 'config set' to save values, 'config set --local' " +
			"to save them for the current workspace only, 'config get' to print the resolved " +
			"values and the file they came from, and 'config path' to print the file in effect.",
	}
	cmd.AddCommand(newConfigSetCommand(), newConfigGetCommand(), newConfigPathCommand())
	return cmd
}

// newConfigSetCommand builds `config set`, which persists the GitHub identity,
// workspace root, fork prefix, and concurrency to a configuration file. It
// resolves the current configuration (which already applies any values supplied
// via flags or environment with the correct precedence), validates the resolved
// concurrency so an out-of-range value is never persisted, and saves it. The
// token is never written to disk because Manager.SaveTo excludes it.
//
// By default it writes to the file already in effect, which is the workspace-local
// one when the working directory sits inside a workspace that has one. --local
// writes to ./.ack-workspace/config instead, which is how a workspace-local file
// is created in the first place.
func newConfigSetCommand() *cobra.Command {
	var local bool

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Persist configuration values to the config file",
		Long: "set resolves the effective configuration from your flags, environment, and any " +
			"existing config files, then writes the GitHub identity, workspace root, fork " +
			"prefix, and concurrency to the file in effect — or, with --local, to " +
			"./.ack-workspace/config. The GitHub token is never persisted.",
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

			path := mgr.Path()
			if local {
				path = mgr.LocalPath()
				if path == "" {
					return fmt.Errorf("determining the working directory for --local")
				}
				// Creating a workspace-local file in a directory declares that directory
				// to be the workspace, so the root it records must come from here rather
				// than from whatever the global file happens to name. An explicit
				// --workspace-root still wins, for the case where the config lives outside
				// the tree it configures.
				if !cmd.Flags().Changed(config.FlagWorkspaceRoot) {
					cfg.WorkspaceRoot = filepath.Dir(filepath.Dir(path))
				}
			}

			if err := mgr.SaveTo(cfg, path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved configuration to %s\n", path)
			return nil
		},
	}

	cmd.Flags().BoolVar(&local, "local", false,
		"write to a workspace-local config file (./.ack-workspace/config) instead of the file currently in effect")

	return cmd
}

// newConfigGetCommand builds `config get`, which prints the resolved
// configuration values (flag > env > workspace-local file > global file >
// default) so the contributor can see exactly what the tool would use for an
// invocation. It also names the file those persisted values came from, since with
// two possible layers "which config am I using?" is otherwise guesswork. The
// token is not printed.
func newConfigGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print the resolved configuration values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr := config.NewManager()
			cfg, err := mgr.Resolve(buildSource(cmd))
			if err != nil {
				return err
			}
			scope := "global"
			if cfg.Path != mgr.GlobalPath() {
				scope = "workspace-local"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "github-user:    %s\n", cfg.GitHubUser)
			fmt.Fprintf(out, "workspace-root: %s\n", cfg.WorkspaceRoot)
			fmt.Fprintf(out, "prefix:         %s\n", cfg.RepoPrefix)
			fmt.Fprintf(out, "concurrency:    %d\n", cfg.Concurrency)
			fmt.Fprintf(out, "config-file:    %s (%s)\n", cfg.Path, scope)
			return nil
		},
	}
}

// newConfigPathCommand builds `config path`, which prints the configuration file
// in effect for the working directory: the nearest workspace-local file, or the
// global one when there is none. It requires no resolution and works even when no
// configuration file exists yet.
func newConfigPathCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path in effect",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), config.NewManager().Path())
			return nil
		},
	}
}
