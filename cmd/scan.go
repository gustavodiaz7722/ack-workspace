package cmd

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
	"github.com/aws-controllers-k8s/ack-workspace/internal/scanner"
)

const (
	// flagResource selects which resource within a controller to scan, or "all".
	flagResource = "resource"
	// flagIssue selects which known issue to investigate, or "all".
	flagIssue = "issue"
	// flagModel overrides the Bedrock model identifier used for the scan.
	flagModel = "model"
	// flagRegion overrides the AWS region used to reach Bedrock.
	flagRegion = "region"
	// flagDebug enables a full conversation transcript on stderr.
	flagDebug = "debug"
)

// newScanCommand builds the `scan` subcommand, which uses a Bedrock, tool-using
// agent to investigate known issues in ACK service controllers and report
// structured findings.
//
// The unit of work is a single (controller, resource, issue) triple, each run as
// one independent agent conversation. Any of the three dimensions may be "all"
// to fan out: `scan` with no arguments scans every issue against every resource
// of every controller under the workspace root.
//
// scan reads local controller artifacts (generator.yaml and the CRDs) directly
// and calls Amazon Bedrock; it needs no git, GitHub token, or GitHub identity,
// so it declares no prerequisites. AWS credentials are resolved from the default
// chain and exercised only when a conversation runs. Like status, the scanner
// renders its own output and stashes a neutral Summary.
func newScanCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan [controller|all]",
		Short: "Investigate known issues in ACK controllers with a Bedrock agent",
		Long: "scan runs a Bedrock, tool-using agent that investigates a known issue against a " +
			"single resource of a single service controller and reports structured findings. Each " +
			"(controller, resource, issue) triple is one independent agent conversation.\n\n" +
			"The controller argument may be a bare alias (acm), its full form (acm-controller), or " +
			"'all'. Use --resource and --issue to narrow or widen the scan; each accepts 'all'. " +
			"With no arguments, scan investigates every issue against every resource of every " +
			"controller under the workspace root.\n\n" +
			"AWS credentials are read from the default chain; override the model and region with " +
			"--model and --region.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{})
			if err != nil {
				return err
			}

			controller := scanner.All
			if len(args) > 0 {
				controller = args[0]
			}
			resource, _ := cmd.Flags().GetString(flagResource)
			issue, _ := cmd.Flags().GetString(flagIssue)
			jsonOut, _ := cmd.Flags().GetBool(flagJSON)
			model, _ := cmd.Flags().GetString(flagModel)
			region, _ := cmd.Flags().GetString(flagRegion)
			debug, _ := cmd.Flags().GetBool(flagDebug)

			opts := scanner.Options{
				Controller:  controller,
				Resource:    resource,
				Issue:       issue,
				JSON:        jsonOut,
				Concurrency: a.Config.Concurrency,
			}
			// The transcript goes to stderr so it never corrupts the findings
			// (in particular the --json document) written to stdout.
			var debugOut io.Writer
			if debug {
				debugOut = cmd.ErrOrStderr()
			}
			summary, err := d.scanRun(cmdContext(cmd), a, opts, region, model, cmd.OutOrStdout(), debugOut)
			if err != nil {
				return err
			}
			res.set(summary)
			return nil
		},
	}
	cmd.Flags().String(flagResource, scanner.All, "resource within the controller to scan, or \"all\"")
	cmd.Flags().String(flagIssue, scanner.All, "known issue number to investigate, or \"all\"")
	cmd.Flags().Bool(flagJSON, false, "emit machine-readable JSON instead of a table")
	cmd.Flags().String(flagModel, "", "Bedrock model identifier (default the scanner's built-in model)")
	cmd.Flags().String(flagRegion, "", "AWS region for Bedrock (default the resolved AWS config region)")
	cmd.Flags().Bool(flagDebug, false, "print a full conversation transcript (prompts, tool calls and results) to stderr; runs serially")
	return cmd
}
