package scanner

import (
	"context"
)

// sourceModel is the reference issue's Smithy API model source name. The
// reference issue reuses sourceFields (the CRD field index), but with a Load
// that marks reference configuration rather than document markings.
const sourceModel = "model"

// referenceTools returns the reference issue's tools: a single sandboxed grep
// tool over its two sources (the CRD field index and the Smithy API model). As
// with the document issue, the model can search only what the issue declares.
func referenceTools(fetcher ModelFetcher) []Tool {
	return []Tool{grepTool(referenceSources(fetcher))}
}

// referenceSources declares the two documents the reference issue may grep: the
// resource's CRD field index (with reference configuration folded in) and the
// service's AWS Smithy API model (filtered to the resource's shapes).
func referenceSources(fetcher ModelFetcher) []Source {
	return []Source{
		{
			Name: sourceFields,
			Description: "Every spec field of the resource's CRD as JSON, one field per line: " +
				"path (dot notation), type, description, and is_reference (whether generator.yaml already " +
				"configures the field as a cross-resource reference). Grep it to find the CRD field matching " +
				"a model field and whether its reference is already configured.",
			Load: loadReferenceFieldsSource,
		},
		{
			Name: sourceModel,
			Description: "The service's AWS Smithy JSON API model, filtered to the shapes relevant to this " +
				"resource. Grep it for the aws.api#arnReference trait (the definitive reference signal), for " +
				"members whose names end in Arn/Id/Name, and for member documentation (smithy.api#documentation) " +
				"that names another AWS resource. Pass a different model name as 'ref' if the default is wrong.",
			Load: loadModelSource(fetcher),
		},
	}
}

// loadReferenceFieldsSource returns the CRD field index for the target resource
// with each field marked by whether generator.yaml configures it as a reference.
func loadReferenceFieldsSource(_ context.Context, target Target, _ string) (string, error) {
	return buildReferenceFieldIndex(target.RepoPath, target.Resource)
}

// loadModelSource returns the service's Smithy API model, filtered to the shapes
// relevant to the target resource. The model name is resolved from the
// controller's generator.yaml (sdk_names.model_name, defaulting to the
// controller alias) unless the caller supplies one as ref.
func loadModelSource(fetcher ModelFetcher) func(context.Context, Target, string) (string, error) {
	return func(ctx context.Context, target Target, ref string) (string, error) {
		modelName := ref
		if modelName == "" {
			modelName = resolveModelName(target.RepoPath, target.Controller)
		}
		content, err := fetcher.FetchModel(ctx, modelName)
		if err != nil {
			return "", err
		}
		return filterModelContent(content, target.Resource), nil
	}
}
