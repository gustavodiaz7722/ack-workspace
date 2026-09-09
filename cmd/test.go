package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/deployer"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
	"github.com/aws-controllers-k8s/ack-workspace/internal/tester"
)

const (
	// flagMarkers selects tests by pytest marker.
	flagMarkers = "markers"
	// flagMethods selects tests by name, using pytest's -k expression syntax.
	flagMethods = "methods"
	// flagThreads sets the pytest-xdist worker count.
	flagThreads = "threads"
	// flagLogLevel sets the pytest log level.
	flagLogLevel = "log-level"
	// flagSkipCleanup leaves the suite's AWS fixtures in place after the run.
	flagSkipCleanup = "skip-cleanup"
)

// newTestCommand builds the `test` subcommand, which runs a single service
// controller's end-to-end suite against the controller already running on the
// development cluster: it locates the controller's test/e2e suite, confirms the
// kubeconfig selects that cluster and that the controller has a ready replica on
// it, provisions a Python environment holding the suite's pinned requirements,
// then creates the suite's AWS fixtures, runs pytest, and deletes the fixtures
// again.
//
// test declares git, kubectl and python3 as prerequisites — git because pip
// installs the acktest dependency straight from a git URL — so a missing one is
// reported before an environment is built rather than from inside pip. It does
// not fork, clone, push to GitHub, or open a pull request, so it needs no GitHub
// token or identity. The empty-service case is enforced by the tester, which
// returns a *tester.UsageError so the rule lives in one place and maps to a
// usage exit code.
func newTestCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test <service>",
		Short: "Run a controller's end-to-end suite against the deployed controller",
		Long: "test runs a single service controller's end-to-end test suite (its test/e2e " +
			"directory) against the controller already running on the shared ACK development " +
			"cluster (" + deployer.ClusterName + "). It creates the suite's AWS fixtures with " +
			"service_bootstrap.py, runs pytest over the suite, and deletes those fixtures again " +
			"with service_cleanup.py.\n\n" +
			"The service may be a bare alias (ecr) or its full form (ecr-controller).\n\n" +
			"test is the follow-on to `ack-workspace deploy <service>`, not a replacement for it: " +
			"it never builds an image, creates a cluster, or installs a controller. Before running " +
			"anything it confirms the kubeconfig selects the " + deployer.ClusterName + " cluster, " +
			"that the cluster answers, and that the controller has a ready replica in the " +
			deployer.Namespace + " namespace. A kubeconfig pointing somewhere else, or a controller " +
			"that is not running, is reported as skipped with the deploy to run first — where the " +
			"suite ran is the one thing its output cannot tell you afterwards, which is also why " +
			"there is no flag to target another cluster.\n\n" +
			"Python environment: the suite's pinned requirements are installed into a virtual " +
			"environment under $HOME/.ack-workspace/venvs/<service>-controller, created on the " +
			"first run and reused afterwards; it is reinstalled automatically when the suite " +
			"repins its requirements. It lives outside the controller checkout deliberately, " +
			"because an in-checkout .venv is not covered by a controller's .gitignore and would " +
			"leave the repository dirty — which `status` reports and `deploy` refuses. An existing " +
			"test/e2e/.venv is used when it is already there, so a hand-built environment is not " +
			"duplicated.\n\n" +
			"Test selection: pass --markers or --methods to run a subset (repeat or comma-separate " +
			"either; several values are combined into one pytest expression). Use --threads to " +
			"change the pytest-xdist worker count, which is the knob to reach for when a suite " +
			"trips AWS API throttling, and --log-level for pytest's log verbosity.\n\n" +
			"Outcome: the command's result is the pytest result and nothing else, so a failure " +
			"always means a test failed. The fixtures are deleted even when tests fail; a teardown " +
			"that itself fails is reported in the summary, naming the AWS resources that may " +
			"remain, rather than changing the outcome.\n\n" +
			"Suites create real AWS resources in the account your credentials resolve to, and " +
			"delete them again on teardown. Pass --region to pin the region they are created in; " +
			"by default the suite resolves it the way boto3 does, from your environment and AWS " +
			"profile. Pass --dry-run to print the commands that would run without provisioning an " +
			"environment or creating anything.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// git is needed because the suites pin acktest as a git URL, which pip
			// resolves by cloning; python3 builds the environment pip then runs from.
			a, err := d.prepare(cmd, prereq.Need{Tools: prereq.Git | prereq.Kubectl | prereq.Python})
			if err != nil {
				return err
			}

			markers, _ := cmd.Flags().GetStringSlice(flagMarkers)
			methods, _ := cmd.Flags().GetStringSlice(flagMethods)
			region, _ := cmd.Flags().GetString(flagRegion)
			threads, _ := cmd.Flags().GetString(flagThreads)
			logLevel, _ := cmd.Flags().GetString(flagLogLevel)
			skipCleanup, _ := cmd.Flags().GetBool(flagSkipCleanup)

			// A missing service identifier is validated by the tester (which returns a
			// *tester.UsageError) so the rule is enforced in a single place.
			var service string
			if len(args) > 0 {
				service = args[0]
			}

			summary, err := d.testRun(cmdContext(cmd), a, service, tester.Options{
				Markers:     markers,
				Methods:     methods,
				Region:      region,
				Threads:     threads,
				LogLevel:    logLevel,
				SkipCleanup: skipCleanup,
			}, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			res.setLabeled(summary, "tested")
			return nil
		},
	}
	cmd.Flags().StringSlice(flagMarkers, nil, "run only tests carrying these pytest markers; repeat or comma-separate for several (default all tests)")
	cmd.Flags().StringSlice(flagMethods, nil, "run only tests whose names match these pytest -k expressions; repeat or comma-separate for several (default all tests)")
	cmd.Flags().String(flagRegion, "", "AWS region the suite creates its resources in (default the region boto3 resolves from your environment and AWS profile)")
	cmd.Flags().String(flagThreads, "", "pytest-xdist worker count, or \"auto\" for one per CPU (default \"auto\"); lower it when a suite trips AWS API throttling")
	cmd.Flags().String(flagLogLevel, "", "pytest log level (default \"info\")")
	cmd.Flags().Bool(flagSkipCleanup, false, "leave the suite's AWS fixtures in place so a failure can be inspected; they then cost money until deleted by hand")
	return cmd
}
