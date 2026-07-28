package scanner

import (
	"context"
)

// referenceTools returns the reference issue's tools: a single sandboxed grep
// tool over a single source, the resource's CRD field index.
//
// The issue previously exposed the service's Smithy model as a second grep
// source. That is gone: the model is still consulted, but in Go rather than by
// the agent (see smithy.go), and its documentation is fused into the field index
// before the agent ever sees it. The model was only ever useful for member
// documentation — the aws.api#arnReference trait the old prompt treated as the
// definitive signal is absent from all but 8 of the 75 published service models —
// and joining that documentation onto CRD paths is a deterministic mapping the
// code-generator already defines, not a judgment call worth spending model turns
// on. Folding it in also removes a grep target that reached 1.5MB for a wide
// resource like ec2's Instance.
func referenceTools(fetcher ModelFetcher) []Tool {
	return []Tool{grepTool(referenceSources(fetcher))}
}

// referenceSources declares the single document the reference issue may grep: the
// resource's CRD field index, with reference configuration and the model's
// documentation and validation patterns folded in.
func referenceSources(fetcher ModelFetcher) []Source {
	return []Source{
		{
			Name: sourceFields,
			Description: "Every spec field of the resource's CRD as JSON, one field per line: " +
				"path (dot notation), type, description, description_source (\"crd\" or \"model\"), " +
				"pattern (the API model's validation pattern, often an ARN template naming the " +
				"referenced service and resource type), is_reference (whether generator.yaml already " +
				"configures the field as a cross-resource reference), is_immutable, and is_primary_key. " +
				"Grep it to find reference candidates and whether each is already configured; " +
				"is_immutable is a supporting signal for a reference (references are often set once), " +
				"and is_primary_key flags the resource's own primary key.",
			Load: loadReferenceFieldsSource(fetcher),
		},
	}
}

// loadReferenceFieldsSource returns the CRD field index for the target resource,
// enriched with the service's Smithy model documentation and validation patterns.
//
// A model that cannot be fetched or parsed is not an error: the index is still
// built, just without the model-sourced descriptions. Failing here would take out
// the issue's only source over a transient network problem, whereas degrading
// costs the agent nested-field documentation and nothing else.
func loadReferenceFieldsSource(fetcher ModelFetcher) func(context.Context, Target, string) (string, error) {
	return func(ctx context.Context, target Target, ref string) (string, error) {
		modelName := ref
		if modelName == "" {
			modelName = resolveModelName(target.RepoPath, target.Controller)
		}
		var docs docIndex
		if content, err := fetcher.FetchModel(ctx, modelName); err == nil {
			if idx, err := buildDocIndex(content, target.RepoPath, target.Resource); err == nil {
				docs = idx
			}
		}
		return buildReferenceFieldIndex(target.RepoPath, target.Resource, docs)
	}
}
