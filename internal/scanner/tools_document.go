package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// toolGrep is the single tool the document issue exposes: a grep over the
// issue's declared sources (and nothing else).
const toolGrep = "grep"

// Source names for the document issue.
const (
	sourceFields         = "fields"
	sourceTerraformIndex = "terraform_index"
	sourceTerraformDoc   = "terraform_doc"
)

// documentTools returns the document issue's tools: a single sandboxed grep tool
// over the issue's sources. The controller repo files and the Terraform docs are
// all reached through grep on a fixed set of named sources, so the model can
// search only what the issue declares — no arbitrary shell, no wandering into
// unrelated files.
func documentTools(fetcher DocsFetcher) []Tool {
	return []Tool{grepTool(documentSources(fetcher))}
}

// documentSources declares the four documents the document issue may grep.
func documentSources(fetcher DocsFetcher) []Source {
	return []Source{
		{
			Name: sourceFields,
			Description: "Every spec field of the resource's CRD as JSON, one field per line: " +
				"path (dot notation), type, description, is_document, is_iam_policy (the last two from " +
				"generator.yaml). Grep it to find candidate document fields and whether they are already marked.",
			Load: loadFieldsSource,
		},
		{
			Name:        sourceTerraformIndex,
			Description: "The list of all Terraform AWS provider resource doc slugs, one per line. Grep it (e.g. for the controller name) to find this resource's slug.",
			Load:        loadTerraformIndexSource(fetcher),
		},
		{
			Name:        sourceTerraformDoc,
			Description: "A Terraform AWS provider resource doc. Pass the slug as 'ref' (e.g. sns_topic_subscription); defaults to <controller>_<resource>.",
			Load:        loadTerraformDocSource(fetcher),
		},
	}
}

// loadFieldsSource returns the combined CRD + generator field index for the
// target resource.
func loadFieldsSource(_ context.Context, target Target, _ string) (string, error) {
	return buildFieldIndex(target.RepoPath, target.Resource)
}

// loadTerraformIndexSource returns the newline-joined list of all Terraform
// resource doc slugs.
func loadTerraformIndexSource(fetcher DocsFetcher) func(context.Context, Target, string) (string, error) {
	return func(ctx context.Context, _ Target, _ string) (string, error) {
		slugs, err := fetcher.ListResources(ctx)
		if err != nil {
			// The index is served by the GitHub REST API, which can be rate
			// limited or degraded. It is only a convenience for discovering the
			// exact slug, so tell the agent to stop retrying it and fall back to
			// the terraform_doc source (served by a separate, more reliable raw
			// endpoint) with a slug it derives itself.
			return "", fmt.Errorf("%w: the terraform_index is temporarily unavailable — do not retry it; "+
				"instead derive the likely resource slug (snake_case) and query the terraform_doc source "+
				"directly with that slug as \"ref\"", err)
		}
		return strings.Join(slugs, "\n") + "\n", nil
	}
}

// loadTerraformDocSource returns the markdown of one Terraform resource doc,
// selected by the ref slug (defaulting to the conventional slug for the target).
func loadTerraformDocSource(fetcher DocsFetcher) func(context.Context, Target, string) (string, error) {
	return func(ctx context.Context, target Target, ref string) (string, error) {
		slug := resolveSlug(target, ref)
		content, err := fetcher.FetchDoc(ctx, slug)
		if err != nil {
			return "", err
		}
		// Prefix with the slug so a grep result makes the doc identity obvious.
		return fmt.Sprintf("# terraform resource: %s\n%s", slug, content), nil
	}
}

// grepTool builds the sandboxed grep tool over the given sources. The model
// chooses a source and a pattern; the tool resolves the source to its content
// and greps it with the real grep. It can only touch the declared sources.
func grepTool(sources []Source) Tool {
	byName := make(map[string]Source, len(sources))
	names := make([]string, 0, len(sources))
	var descriptions strings.Builder
	for _, s := range sources {
		byName[s.Name] = s
		names = append(names, s.Name)
		fmt.Fprintf(&descriptions, "\n  - %s: %s", s.Name, s.Description)
	}

	return Tool{
		Name: toolGrep,
		Description: "Search one of this issue's source documents with grep (extended regex, -E), " +
			"returning matching lines with line numbers and surrounding context. Available sources:" +
			descriptions.String(),
		InputSchema: grepSchema(names),
		Run: func(ctx context.Context, target Target, input json.RawMessage) (string, error) {
			var args struct {
				Source     string `json:"source"`
				Ref        string `json:"ref"`
				Pattern    string `json:"pattern"`
				Context    *int   `json:"context"`
				IgnoreCase *bool  `json:"ignore_case"`
			}
			if len(input) > 0 {
				_ = json.Unmarshal(input, &args)
			}
			src, ok := byName[args.Source]
			if !ok {
				return "", fmt.Errorf("unknown source %q; valid sources: %s", args.Source, strings.Join(names, ", "))
			}
			content, err := src.Load(ctx, target, args.Ref)
			if err != nil {
				return "", err
			}
			contextLines := defaultGrepContext
			if args.Context != nil {
				contextLines = *args.Context
			}
			ignoreCase := true
			if args.IgnoreCase != nil {
				ignoreCase = *args.IgnoreCase
			}
			out, err := grepContent(ctx, content, args.Pattern, grepOptions{
				contextLines: contextLines,
				ignoreCase:   ignoreCase,
				lineNumbers:  true,
			})
			if err != nil {
				return "", err
			}
			header := fmt.Sprintf("source: %s (%d lines)", args.Source, countLines(content))
			if strings.TrimSpace(out) == "" {
				return header + "\nno matches", nil
			}
			return header + "\n" + out, nil
		},
	}
}

// grepSchema builds the grep tool's input schema with the source enum.
func grepSchema(sourceNames []string) json.RawMessage {
	enum, _ := json.Marshal(sourceNames)
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "source": {"type": "string", "enum": %s, "description": "Which source document to search."},
    "ref": {"type": "string", "description": "Optional source-specific selector; for terraform_doc it is the resource slug."},
    "pattern": {"type": "string", "description": "POSIX extended regular expression (grep -E), e.g. \"json|policy\"."},
    "context": {"type": "integer", "description": "Lines of surrounding context per match (grep -C). Optional; defaults to 2."},
    "ignore_case": {"type": "boolean", "description": "Case-insensitive matching. Optional; defaults to true."}
  },
  "required": ["source", "pattern"],
  "additionalProperties": false
}`, enum))
}

// resolveSlug returns the model-supplied slug when present, otherwise the
// conventional default for the target.
func resolveSlug(target Target, supplied string) string {
	if s := strings.TrimSpace(supplied); s != "" {
		return s
	}
	return defaultTerraformSlug(target)
}

// defaultTerraformSlug builds the conventional Terraform resource slug for a
// target: the controller alias, an underscore, and the resource Kind converted
// to snake_case (Certificate -> certificate, LoadBalancer -> load_balancer).
func defaultTerraformSlug(target Target) string {
	return target.Controller + "_" + toSnakeCase(target.Resource)
}

// toSnakeCase converts a PascalCase/camelCase identifier to snake_case.
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
