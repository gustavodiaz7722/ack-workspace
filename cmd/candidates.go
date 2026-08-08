package cmd

import (
	"github.com/spf13/cobra"

	"github.com/aws-controllers-k8s/ack-workspace/internal/candidates"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
)

const (
	// flagResource selects which resource within a controller to index, or "all".
	flagResource = "resource"
	// flagOutDir names the directory that receives one candidate index file per
	// resource.
	flagOutDir = "out-dir"
)

// newCandidatesCommand builds the `candidates` subcommand, which emits the
// deterministic cross-resource-reference candidate index for a resource: every
// string-valued CRD spec field, fused with the generator.yaml markings that
// bear on whether it is a reference and with the API model's documentation and
// validation patterns.
//
// This is the mechanical narrowing step of a reference audit, separated from
// the judgment about which candidates are genuine references: it produces the
// field set a reviewer decides over, so an audit can be split across
// independent reviewers who all start from identical input, and so two runs
// over an unchanged repo produce the same set.
//
// It reads the CRDs and generator.yaml locally and fetches the service's Smithy
// model over HTTP, so it needs no AWS credentials, git, or GitHub identity.
func newCandidatesCommand(d deps, res *Result) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "candidates [controller|all]",
		Short: "Emit the cross-resource-reference candidate index for a resource",
		Long: "candidates emits the pre-filtered cross-resource-reference candidate index as JSON " +
			"Lines: every string-valued spec field of a resource's CRD, fused with its generator.yaml " +
			"markings (is_reference and the configured target, is_immutable, is_primary_key) and with " +
			"the service API model's field documentation and validation patterns.\n\n" +
			"Model documentation is resolved by walking the model's shape graph to each field path, so " +
			"a description or pattern is attributed to the field that actually declares it. Nested " +
			"fields are where reference gaps concentrate and are undocumented in the CRD, so this " +
			"enrichment is what makes them judgeable.\n\n" +
			"The controller argument may be a bare alias (eks), its full form (eks-controller), or " +
			"'all'; --resource likewise accepts a Kind or 'all'. Records stream to stdout by default, " +
			"or use --out-dir to write one <Resource>.jsonl per resource. Progress lines and the " +
			"ignore.field_paths suppression notes go to stderr, so stdout stays machine-readable.\n\n" +
			"Reads local repositories and the public API models; no AWS credentials, git, or GitHub " +
			"identity required.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := d.prepare(cmd, prereq.Need{})
			if err != nil {
				return err
			}

			controller := candidates.All
			if len(args) > 0 {
				controller = args[0]
			}
			resource, _ := cmd.Flags().GetString(flagResource)
			outDir, _ := cmd.Flags().GetString(flagOutDir)

			opts := candidates.Options{
				Controller: controller,
				Resource:   resource,
				OutDir:     outDir,
			}
			summary, err := d.candidatesRun(cmdContext(cmd), a, opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res.set(summary)
			return nil
		},
	}
	cmd.Flags().String(flagResource, candidates.All, "resource within the controller to index, or \"all\"")
	cmd.Flags().String(flagOutDir, "", "write one <Resource>.jsonl per resource under this directory instead of streaming to stdout")
	return cmd
}
