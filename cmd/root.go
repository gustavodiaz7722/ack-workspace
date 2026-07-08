package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/adder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/githubclient"
	"github.com/aws-controllers-k8s/ack-workspace/internal/initializer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/inspector"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
	"github.com/aws-controllers-k8s/ack-workspace/internal/refresher"
	"github.com/aws-controllers-k8s/ack-workspace/internal/releaser"
	"github.com/aws-controllers-k8s/ack-workspace/internal/remover"
	"github.com/aws-controllers-k8s/ack-workspace/internal/scanner"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// FlagDryRun is the persistent flag name for the global dry-run/preview mode.
// The other persistent flag names are shared with the configuration layer (see
// config.Flag* constants) so the Source-building helper can map a set flag
// directly onto a configuration key.
const FlagDryRun = "dry-run"

// Concurrency bounds. A resolved concurrency value outside this inclusive range
// is rejected before any work is performed (Requirement 7.3).
const (
	minConcurrency = 1
	maxConcurrency = 32
)

// UsageError marks an argument or validation failure (as opposed to a runtime
// failure during repository processing). The CLI entrypoint (Task 13.3) may map
// it to a distinct non-zero exit code for usage errors.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

// Result is the seam through which a subcommand hands its aggregated outcome to
// the process entrypoint (main.go, Task 13.3) so the entrypoint can render the
// summary and derive the exit code.
//
// The batch commands (init, add, refresh) and the read-only status command stash
// the workspace.Summary they produced via set; the config command produces no
// summary and leaves the Result empty. main.go obtains the Result from Execute
// and applies the exit-code policy: non-zero on a pre-flight/usage error (the
// error returned by Execute) or when the stashed Summary HasFailures, zero
// otherwise. Rendering of the created/skipped/failed summary itself is the
// responsibility of Task 13.3; 13.2 only populates this Result.
type Result struct {
	summary      workspace.Summary
	hasSummary   bool
	createdLabel string
}

// set records the Summary produced by a command, leaving the created-bucket
// label at its default ("created"). It is called by commands whose
// OutcomeCreated bucket is presented as "created" (init) or that produce a
// neutral summary (status).
func (r *Result) set(s workspace.Summary) {
	r.summary = s
	r.hasSummary = true
}

// setLabeled records the Summary together with the label the renderer should use
// for the OutcomeCreated bucket. The add command passes "added" (Requirement
// 4.9) and the refresh command passes "refreshed" so the human summary reads in
// the command's own terms.
func (r *Result) setLabeled(s workspace.Summary, createdLabel string) {
	r.summary = s
	r.hasSummary = true
	r.createdLabel = createdLabel
}

// Summary returns the stashed Summary and whether a command set one. A command
// that produces no summary (config) leaves ok false.
func (r *Result) Summary() (summary workspace.Summary, ok bool) {
	return r.summary, r.hasSummary
}

// CreatedLabel returns the label the entrypoint should use for the
// OutcomeCreated bucket when rendering the Summary. It is empty when a command
// did not override it, in which case the renderer falls back to "created".
func (r *Result) CreatedLabel() string {
	return r.createdLabel
}

// deps holds the injectable collaborators the subcommands delegate to. Wiring
// them through a struct (rather than calling the concrete components directly)
// is the testing seam for this layer: tests substitute a stubbed prereq.Checker
// to drive prerequisite enforcement deterministically (without depending on the
// real PATH) and fake component runners to assert the commands parse arguments
// and flags correctly and delegate without performing any network or git work.
// defaultDeps wires the production checker and components.
type deps struct {
	// checker verifies a command's declared prerequisites before it delegates.
	checker prereq.Checker
	// initRun runs the Workspace_Initializer for the init command.
	initRun func(ctx context.Context, a app.App) (workspace.Summary, error)
	// addRun runs the Controller_Adder for the add command.
	addRun func(ctx context.Context, a app.App, identifiers []string) (workspace.Summary, error)
	// refreshRun runs the Workspace_Refresher for the refresh command.
	refreshRun func(ctx context.Context, a app.App, only []string) (workspace.Summary, error)
	// statusRun runs the Workspace_Inspector for the status command. The writer
	// is threaded through so inspector output is directed at the command's
	// stdout (and is capturable in tests).
	statusRun func(ctx context.Context, a app.App, jsonOut bool, out io.Writer) (workspace.Summary, error)
	// removeRun runs the Controller_Remover for the remove command.
	removeRun func(ctx context.Context, a app.App, identifiers []string, opts remover.Options) (workspace.Summary, error)
	// releaseRun runs the Controller_Releaser for the release command.
	releaseRun func(ctx context.Context, a app.App, service, version, baseBranch string, skipPR bool, prBody string) (workspace.Summary, error)
	// deployRun runs the Controller_Deployer for the deploy command: it builds the
	// controller from local source and deploys it to the current kubeconfig
	// cluster.
	deployRun func(ctx context.Context, a app.App, service, namespace, imageTag, repository, region string) (workspace.Summary, error)
	// scanRun runs the scanner for the scan command. It constructs the Bedrock
	// model client (from the given region and model), directs the scanner's
	// findings at out, and (when debugOut is non-nil) its conversation transcript
	// at debugOut.
	scanRun func(ctx context.Context, a app.App, opts scanner.Options, region, model string, out, debugOut io.Writer) (workspace.Summary, error)
}

// defaultDeps returns the production wiring: the real prerequisite checker and
// the real feature components. Constructing the components performs no network
// or git work; that happens only when their methods run.
func defaultDeps() deps {
	return deps{
		checker: prereq.NewChecker(),
		initRun: func(ctx context.Context, a app.App) (workspace.Summary, error) {
			return initializer.New().Init(ctx, a)
		},
		addRun: func(ctx context.Context, a app.App, identifiers []string) (workspace.Summary, error) {
			return adder.New().Add(ctx, a, identifiers)
		},
		refreshRun: func(ctx context.Context, a app.App, only []string) (workspace.Summary, error) {
			return refresher.New().Refresh(ctx, a, only)
		},
		statusRun: func(ctx context.Context, a app.App, jsonOut bool, out io.Writer) (workspace.Summary, error) {
			return inspector.NewWithWriter(out).Status(ctx, a, jsonOut)
		},
		removeRun: func(ctx context.Context, a app.App, identifiers []string, opts remover.Options) (workspace.Summary, error) {
			return remover.New().Remove(ctx, a, identifiers, opts)
		},
		releaseRun: func(ctx context.Context, a app.App, service, version, baseBranch string, skipPR bool, prBody string) (workspace.Summary, error) {
			return releaser.New().Release(ctx, a, service, releaser.Options{
				Version:    version,
				BaseBranch: baseBranch,
				SkipPR:     skipPR,
				PRBody:     prBody,
			})
		},
		deployRun: func(ctx context.Context, a app.App, service, namespace, imageTag, repository, region string) (workspace.Summary, error) {
			return deployer.New().Deploy(ctx, a, service, deployer.Options{
				Namespace:  namespace,
				ImageTag:   imageTag,
				Repository: repository,
				Region:     region,
			})
		},
		scanRun: func(ctx context.Context, a app.App, opts scanner.Options, region, model string, out, debugOut io.Writer) (workspace.Summary, error) {
			client, err := scanner.NewBedrockClient(ctx, region, model)
			if err != nil {
				return workspace.Summary{}, err
			}
			s := scanner.NewWithWriterToken(client, out, a.Config.Token)
			if debugOut != nil {
				s.SetTraceWriter(debugOut)
			}
			return s.Scan(ctx, a, opts)
		},
	}
}

// NewRootCommand builds the ack-workspace root command, registers the persistent
// flags shared by every subcommand, and attaches the
// init/add/refresh/status/config subcommands wired to the production components.
func NewRootCommand() *cobra.Command {
	cmd, _ := newRootCmd(defaultDeps())
	return cmd
}

// newRootCmd builds the root command with the given dependencies and returns it
// together with the Result the subcommands populate. It is the shared
// constructor behind NewRootCommand (production wiring) and the command tests
// (fake wiring), keeping the flag set, App construction, and subcommand
// attachment in one place.
func newRootCmd(d deps) (*cobra.Command, *Result) {
	cmd := &cobra.Command{
		Use:   "ack-workspace",
		Short: "Automate the fork-based contributor workflow for AWS Controllers for Kubernetes (ACK)",
		Long: "ack-workspace streamlines local workspace setup for ACK contributors: it forks, " +
			"clones, and configures the core and service-controller repositories, keeps managed " +
			"forks current with upstream, and reports the state of every managed repository.",
		// The command layer prints its own errors and exit codes (Task 13.3),
		// so suppress cobra's automatic usage/error echo on failure.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addPersistentFlags(cmd)

	res := &Result{}
	cmd.AddCommand(
		newInitCommand(d, res),
		newAddCommand(d, res),
		newRefreshCommand(d, res),
		newStatusCommand(d, res),
		newRemoveCommand(d, res),
		newReleaseCommand(d, res),
		newDeployCommand(d, res),
		newScanCommand(d, res),
		newConfigCommand(),
	)
	return cmd, res
}

// Execute builds the root command and runs it, returning the Result a subcommand
// populated together with any error. The process entrypoint (main.go, Task 13.3)
// maps these to an exit code: non-zero on a non-nil error or when the Result's
// Summary HasFailures, zero otherwise.
func Execute() (*Result, error) {
	cmd, res := newRootCmd(defaultDeps())
	err := cmd.Execute()
	return res, err
}

// prepare performs the fail-fast pre-flight for a batch command: it builds the
// App from the resolved, validated configuration and then runs the prerequisite
// Check declared by need BEFORE any component (and therefore any git or GitHub
// operation) runs (Requirements 1.1, 1.3, 1.5, 1.7). buildApp performs no
// network work, so running the check after it preserves the "no side effects
// before validation" guarantee. A returned error is a pre-flight failure
// (config resolution, invalid concurrency, or a missing prerequisite) and stops
// the command before it delegates.
func (d deps) prepare(cmd *cobra.Command, need prereq.Need) (app.App, error) {
	a, err := buildApp(cmd)
	if err != nil {
		return app.App{}, err
	}
	if err := d.checker.Check(need, a.Config); err != nil {
		return app.App{}, err
	}
	return a, nil
}

// cmdContext returns the command's context, falling back to context.Background
// when cobra did not attach one (for example a command constructed directly in
// a test and invoked without ExecuteContext).
func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// addPersistentFlags registers the global flags available to every subcommand.
// The string/int flag names intentionally match the config.Flag* keys so the
// Source-building helper can copy a set flag straight onto its configuration
// key.
func addPersistentFlags(cmd *cobra.Command) {
	pf := cmd.PersistentFlags()
	pf.String(config.FlagWorkspaceRoot, "", "workspace root directory (default $GOPATH/src/github.com/aws-controllers-k8s)")
	pf.String(config.FlagGitHubUser, "", "GitHub username that owns the forks")
	pf.String(config.FlagRepoPrefix, "", fmt.Sprintf("prefix prepended to fork names (default %q)", config.DefaultRepoPrefix))
	pf.Int(config.FlagConcurrency, config.DefaultConcurrency, fmt.Sprintf("maximum repositories processed concurrently (%d-%d)", minConcurrency, maxConcurrency))
	pf.Bool(FlagDryRun, false, "preview the actions that would be taken without making any change")
	pf.String(config.FlagToken, "", "GitHub token (overrides the GITHUB_TOKEN environment variable; never persisted)")
}

// buildSource assembles a config.Source from the flags the user actually set on
// this invocation plus the relevant environment variables. Only flags reported
// as Changed are placed in Source.Flags, so an unset flag does not override a
// persisted or environment value (the precedence rules of Requirement 2.4).
// GITHUB_USER and GITHUB_TOKEN are captured into Source.Env when non-empty.
func buildSource(cmd *cobra.Command) config.Source {
	fs := cmd.Flags()

	flags := map[string]string{}
	for _, name := range []string{
		config.FlagWorkspaceRoot,
		config.FlagGitHubUser,
		config.FlagRepoPrefix,
		config.FlagToken,
	} {
		if fs.Changed(name) {
			v, _ := fs.GetString(name)
			flags[name] = v
		}
	}
	if fs.Changed(config.FlagConcurrency) {
		n, _ := fs.GetInt(config.FlagConcurrency)
		flags[config.FlagConcurrency] = strconv.Itoa(n)
	}

	env := map[string]string{}
	if v := os.Getenv(config.EnvGitHubUser); v != "" {
		env[config.EnvGitHubUser] = v
	}
	if v := os.Getenv(config.EnvToken); v != "" {
		env[config.EnvToken] = v
	}

	return config.Source{Flags: flags, Env: env}
}

// validateConcurrency enforces the inclusive 1..32 range (Requirement 7.3),
// returning a *UsageError that names the accepted range when the value is out
// of bounds.
func validateConcurrency(n int) error {
	if n < minConcurrency || n > maxConcurrency {
		return &UsageError{Msg: fmt.Sprintf(
			"invalid concurrency %d: accepted range is %d to %d inclusive",
			n, minConcurrency, maxConcurrency,
		)}
	}
	return nil
}

// buildApp resolves the effective configuration, validates it, and constructs
// the App context shared by the feature components. It fails fast, before any
// repository work: configuration resolution errors (a missing identity, an
// unparsable file) and an out-of-range concurrency value are returned here so
// the command never begins side-effecting work with invalid input
// (Requirements 2.4, 7.3). The GitHub adapter and git runner are real clients;
// constructing the adapter performs no network request (Requirements 2.4, 7.2).
func buildApp(cmd *cobra.Command) (app.App, error) {
	src := buildSource(cmd)

	cfg, err := config.NewManager().Resolve(src)
	if err != nil {
		return app.App{}, err
	}

	if err := validateConcurrency(cfg.Concurrency); err != nil {
		return app.App{}, err
	}

	dryRun, _ := cmd.Flags().GetBool(FlagDryRun)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	return app.App{
		Config: cfg,
		GitHub: githubclient.NewAdapter(ctx, cfg.Token),
		Git:    git.NewExecRunner(),
		DryRun: dryRun,
	}, nil
}
