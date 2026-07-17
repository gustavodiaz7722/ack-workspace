package scanner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// issueSubresourceNumber is the stable identifier of the embedded-subresource
// issue on the command line and in the registry.
const issueSubresourceNumber = 3

// subresourceOutputSchema is the JSON Schema the agent's findings must conform
// to for the sub-resource issue. It becomes the report tool's input schema, so
// the model is steered to emit exactly this shape: per embedded field, the
// concept it represents, the standalone Terraform resource (if any), whether the
// concept has its own id/arn/tagging, whether it is already a separate CRD, a
// confidence score, and the reasoning.
var subresourceOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "fields": {
      "type": "array",
      "description": "One entry per nested CRD field judged to represent a concept that AWS models as a first-class resource.",
      "items": {
        "type": "object",
        "properties": {
          "ack_field_path": {
            "type": "string",
            "description": "The embedded field path in the parent CRD, in dot notation."
          },
          "concept": {
            "type": "string",
            "description": "A short PascalCase name for the resource concept the field represents."
          },
          "terraform_resource": {
            "type": "string",
            "description": "The standalone Terraform resource slug for the concept (without the aws_ prefix), if one exists. Empty if none."
          },
          "has_own_id": {
            "type": "boolean",
            "description": "Whether the concept has its own identifier separate from the parent's."
          },
          "has_own_arn": {
            "type": "boolean",
            "description": "Whether the concept has its own ARN separate from the parent's."
          },
          "has_own_tagging": {
            "type": "boolean",
            "description": "Whether the concept supports its own tags independent of the parent."
          },
          "already_separate_crd": {
            "type": "boolean",
            "description": "Whether this controller already manages the concept as its own CRD (in which case it is not an anti-pattern)."
          },
          "confidence": {
            "type": "number",
            "minimum": 0,
            "maximum": 1,
            "description": "Confidence, 0.0-1.0, that the embedded field should be its own CRD."
          },
          "reasoning": {
            "type": "string",
            "description": "Why the field is (or is not) an embedded sub-resource anti-pattern and the basis for the confidence."
          }
        },
        "required": ["ack_field_path", "concept", "terraform_resource", "has_own_id", "has_own_arn", "has_own_tagging", "already_separate_crd", "confidence", "reasoning"],
        "additionalProperties": false
      }
    },
    "summary": {
      "type": "string",
      "description": "Short overall summary, including any embedded fields that should be promoted to their own CRD."
    }
  },
  "required": ["fields", "summary"],
  "additionalProperties": false
}`)

// newSubresourceIssue builds the embedded-subresource issue bound to the given
// docs fetcher. It detects concepts embedded as a nested field of a parent CRD
// that AWS models as first-class resources (their own id, arn, and tagging) and
// that therefore should be their own CRD rather than a managed field.
func newSubresourceIssue(fetcher DocsFetcher) Issue {
	return Issue{
		Number: issueSubresourceNumber,
		Name:   "embedded-subresources",
		Description: "Identify concepts embedded as a nested field of a parent CRD that AWS models as " +
			"first-class resources (own id, arn, and tagging) and that should be their own CRD.",
		Tools:        subresourceTools(fetcher),
		OutputSchema: subresourceOutputSchema,
		System:       subresourceSystemPrompt,
		Prompt:       subresourceUserPrompt,
		Evaluate:     evaluateSubresource,
		Summarize:    summarizeSubresource,
	}
}

// subresourceFindings is the decoded shape of the sub-resource issue's report.
type subresourceFindings struct {
	Fields  []subresourceField `json:"fields"`
	Summary string             `json:"summary"`
}

// subresourceField is one reported embedded field in the sub-resource findings.
type subresourceField struct {
	ACKFieldPath       string  `json:"ack_field_path"`
	Concept            string  `json:"concept"`
	TerraformResource  string  `json:"terraform_resource"`
	HasOwnID           bool    `json:"has_own_id"`
	HasOwnARN          bool    `json:"has_own_arn"`
	HasOwnTagging      bool    `json:"has_own_tagging"`
	AlreadySeparateCRD bool    `json:"already_separate_crd"`
	Confidence         float64 `json:"confidence"`
	Reasoning          string  `json:"reasoning"`
}

// isAntiPattern reports whether an embedded field is the anti-pattern this issue
// looks for: a concept that is not already a separate CRD yet has its own
// identity (id or arn) and its own tagging — the marks of a first-class AWS
// resource that was folded into its parent instead of standing on its own.
func (f subresourceField) isAntiPattern() bool {
	return !f.AlreadySeparateCRD && (f.HasOwnID || f.HasOwnARN) && f.HasOwnTagging
}

// evaluateSubresource fails when any embedded field is judged to be a first-class
// resource folded into its parent (its own id/arn and tagging) that is not
// already a separate CRD.
func evaluateSubresource(raw json.RawMessage) (Verdict, error) {
	var sf subresourceFindings
	if err := json.Unmarshal(raw, &sf); err != nil {
		return VerdictError, fmt.Errorf("parsing sub-resource findings: %w", err)
	}
	for _, f := range sf.Fields {
		if f.isAntiPattern() {
			return VerdictFail, nil
		}
	}
	return VerdictPass, nil
}

// summarizeSubresource renders the reduced result: the embedded fields that
// should be promoted to their own CRD (with the concept and standalone Terraform
// resource), and any concepts that are already separate CRDs.
func summarizeSubresource(raw json.RawMessage) string {
	var sf subresourceFindings
	if err := json.Unmarshal(raw, &sf); err != nil {
		return "unparseable findings"
	}

	var promote, separate []string
	for _, f := range sf.Fields {
		if f.AlreadySeparateCRD {
			separate = append(separate, f.Concept)
			continue
		}
		if f.isAntiPattern() {
			promote = append(promote, fmt.Sprintf("%s -> %s%s", f.ACKFieldPath, f.Concept, terraformSuffix(f.TerraformResource)))
		}
	}

	var b strings.Builder
	if len(promote) > 0 {
		fmt.Fprintf(&b, "should be separate CRDs: %s\n", strings.Join(promote, ", "))
	}
	if len(separate) > 0 {
		fmt.Fprintf(&b, "already separate CRDs: %s\n", strings.Join(separate, ", "))
	}
	if b.Len() == 0 {
		return "no embedded sub-resource anti-patterns found"
	}
	return strings.TrimRight(b.String(), "\n")
}

// terraformSuffix renders the standalone Terraform resource in parentheses when
// one was identified, for the summary line.
func terraformSuffix(slug string) string {
	if slug == "" {
		return ""
	}
	return " (aws_" + slug + ")"
}

// subresourceSystemPrompt establishes the agent's role and method for the
// sub-resource issue: what the anti-pattern is, how to recognize it, which tools
// to use, and that it must finish by calling the report tool.
func subresourceSystemPrompt(target Target) string {
	return fmt.Sprintf(`You are an expert reviewer of AWS Controllers for Kubernetes (ACK) service controllers.

Your task: for the %q resource of the %q controller, identify concepts that are embedded as a nested field of this CRD but that AWS models as first-class resources — with their own id, ARN, and tagging — and that therefore should have been their own CRD rather than a managed field of this one.

What the anti-pattern is:
- A parent CRD embeds a concept as a nested object or array-of-objects field, but AWS treats that concept as an independent resource: it has its own identifier and/or ARN, its own tags, and its own create/read/update/delete lifecycle distinct from the parent's. Folding it into the parent means users cannot manage its identity, tags, or lifecycle on their own.
- By contrast, an ordinary configuration sub-object (settings or parameters that only exist as part of the parent, with no independent identifier, ARN, tags, or lifecycle) is NOT this anti-pattern. Do not report those.

Signals to rely on (do not assume a specific resource — investigate with the tools):
- Terraform: if Terraform models the concept as its own standalone resource while this CRD embeds it as a field, that is strong evidence the concept deserves its own CRD. A standalone Terraform resource that documents its own arn, id, and tags attributes confirms an independent identity and lifecycle.
- A concept this controller already manages as its own CRD is NOT the anti-pattern; only embedded fields count.

You have a single tool, %s, which searches a fixed set of sources with a regular expression:
- %q: every spec field of this CRD as JSON, one field per line (path, type, description). Nested object and array-of-object fields are the candidate embedded sub-resources.
- %q: the Kinds this controller already manages as CRDs, one per line.
- %q: the list of all Terraform AWS provider resource slugs.
- %q: a Terraform resource doc, selected with the "ref" slug.
These are the only things you can read.

Method — perform your research in this order:
1. grep %q to enumerate the nested object/array fields of this CRD (for example grep '"type":"array"' and '"type":"object"'). These are the candidate embedded concepts. Note any that carry their own id/arn/tags sub-fields.
2. For each candidate, grep %q for a standalone resource matching the concept. Terraform uses snake_case, and a standalone resource for an embedded concept is often named by combining the parent and the concept, so try several forms derived from the field name and the parent resource. Record the slug if found. The terraform_index depends on an external service that may be unavailable; if it errors, do not retry it — derive the likely slug yourself and verify it directly in the next step.
3. For each likely slug, grep %q with ref set to that slug for "arn", "^id", and "tags". A successful read confirms the concept is a standalone resource with its own identity and tagging; a not-found error means that slug is wrong, so try another.
4. grep %q to check whether this controller already manages the concept as its own CRD. If it does, it is not the anti-pattern.
5. Report your findings.

An embedded field whose concept is a standalone AWS resource with its own id/ARN and tagging, and that is NOT already a separate CRD in this controller, is the anti-pattern this issue looks for; give those the highest confidence. Ordinary configuration sub-objects that have no independent identity or lifecycle are NOT anti-patterns — do not report them.

When you have gathered enough evidence, call the %s tool exactly once with your complete findings. Do not answer in prose; report only through that tool.`,
		target.Resource, target.Controller,
		toolGrep, sourceFields, sourceControllerResources, sourceTerraformIndex, sourceTerraformDoc,
		sourceFields, sourceTerraformIndex, sourceTerraformDoc, sourceControllerResources, reportToolName)
}

// subresourceUserPrompt is the initial user turn that starts the investigation.
func subresourceUserPrompt(target Target) string {
	return fmt.Sprintf("Scan the %q resource of the %q controller for concepts embedded as a nested field "+
		"that should be their own CRD (they have their own id, ARN, and tagging in AWS).",
		target.Resource, target.Controller)
}
