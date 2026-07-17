package scanner

import (
	"context"
	"strings"
)

// sourceControllerResources is the sub-resource issue's source listing the
// Kinds the controller already manages as CRDs. The issue reuses sourceFields
// (the structural field index) and the Terraform sources (sourceTerraformIndex,
// sourceTerraformDoc) declared for the document issue.
const sourceControllerResources = "controller_resources"

// subresourceTools returns the sub-resource issue's tools: a single sandboxed
// grep tool over its sources. As with the other issues, the model can search
// only what the issue declares.
func subresourceTools(fetcher DocsFetcher) []Tool {
	return []Tool{grepTool(subresourceSources(fetcher))}
}

// subresourceSources declares the four documents the sub-resource issue may
// grep: the resource's structural field index, the Kinds the controller already
// manages, the Terraform resource index, and individual Terraform resource docs.
func subresourceSources(fetcher DocsFetcher) []Source {
	return []Source{
		{
			Name: sourceFields,
			Description: "Every spec field of the resource's CRD as JSON, one field per line: path (dot " +
				"notation), type, and description. Grep it to find nested object/array fields (candidate " +
				"embedded sub-resources) and whether they carry their own id/arn/tags sub-fields.",
			Load: loadFieldTypeSource,
		},
		{
			Name: sourceControllerResources,
			Description: "The resource Kinds this controller already manages as CRDs, one per line. Grep it " +
				"to check whether a candidate concept is already its own CRD (in which case it is not the " +
				"anti-pattern this issue looks for).",
			Load: loadControllerResourcesSource,
		},
		{
			Name:        sourceTerraformIndex,
			Description: "The list of all Terraform AWS provider resource doc slugs, one per line. Grep it to check whether a candidate concept is modeled as its own standalone resource (e.g. security_group_rule).",
			Load:        loadTerraformIndexSource(fetcher),
		},
		{
			Name:        sourceTerraformDoc,
			Description: "A Terraform AWS provider resource doc. Pass the standalone resource slug as 'ref' (e.g. vpc_security_group_ingress_rule) and grep it for arn, id, and tags attributes to confirm the concept has its own identity and tagging.",
			Load:        loadTerraformDocSource(fetcher),
		},
	}
}

// loadFieldTypeSource returns the structural (path/type/description) field index
// for the target resource.
func loadFieldTypeSource(_ context.Context, target Target, _ string) (string, error) {
	return buildFieldTypeIndex(target.RepoPath, target.Resource)
}

// loadControllerResourcesSource returns the controller's declared resource Kinds
// as a newline-joined list, so the agent can check whether a concept is already
// a separate CRD.
func loadControllerResourcesSource(_ context.Context, target Target, _ string) (string, error) {
	resources, err := discoverResources(target.RepoPath)
	if err != nil {
		return "", err
	}
	return strings.Join(resources, "\n") + "\n", nil
}
