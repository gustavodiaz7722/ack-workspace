package scanner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// issueReferenceNumber is the stable identifier of the missing-references issue
// on the command line and in the registry.
const issueReferenceNumber = 2

// referenceOutputSchema is the JSON Schema the agent's findings must conform to
// for the reference issue. It becomes the report tool's input schema, so the
// model is steered to emit exactly this shape: per candidate field, the model
// field that flagged it, the matching ACK CRD field (if any), what it references,
// the signal that identified it, whether generator.yaml already configures the
// reference, a confidence score, and the reasoning.
var referenceOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "fields": {
      "type": "array",
      "description": "One entry per field judged to be a cross-resource reference (holding an ARN, ID, or Name that points at another AWS resource).",
      "items": {
        "type": "object",
        "properties": {
          "model_field": {
            "type": "string",
            "description": "The CRD field name that identified this as a reference, e.g. \"roleARN\" or \"kmsKeyID\". Usually the last segment of ack_field_path."
          },
          "ack_field_path": {
            "type": "string",
            "description": "Full ACK CRD field path in dot notation, e.g. \"lambdaConfig.preSignUp\". Empty if there is no corresponding CRD field."
          },
          "target_resource": {
            "type": "string",
            "description": "The AWS resource type the field points at, e.g. \"Role\", \"Function\", \"Key\". Empty if not identifiable."
          },
          "target_service": {
            "type": "string",
            "description": "The AWS service that owns the target resource, e.g. \"iam\", \"lambda\", \"kms\". Empty if not identifiable."
          },
          "signal": {
            "type": "string",
            "enum": ["arn_pattern", "arn_suffix", "id_suffix", "name_suffix", "doc_mention"],
            "description": "The strongest signal that identified this as a reference."
          },
          "has_reference": {
            "type": "boolean",
            "description": "Whether generator.yaml already configures this field with a references block (is_reference in the fields source)."
          },
          "confidence": {
            "type": "number",
            "minimum": 0,
            "maximum": 1,
            "description": "Confidence, 0.0-1.0, that this field is a cross-resource reference."
          },
          "reasoning": {
            "type": "string",
            "description": "Why this field is (or is not) a cross-resource reference and the basis for the confidence."
          }
        },
        "required": ["model_field", "ack_field_path", "target_resource", "signal", "has_reference", "confidence", "reasoning"],
        "additionalProperties": false
      }
    },
    "summary": {
      "type": "string",
      "description": "Short overall summary, including any reference fields that need a references block added in generator.yaml."
    }
  },
  "required": ["fields", "summary"],
  "additionalProperties": false
}`)

// newReferenceIssue builds the missing-references issue bound to the given model
// fetcher. It detects CRD fields that hold a cross-resource reference (an ARN,
// ID, or Name pointing at another AWS resource) but are not configured with a
// references block in generator.yaml.
func newReferenceIssue(fetcher ModelFetcher) Issue {
	return Issue{
		Number: issueReferenceNumber,
		Name:   "missing-references",
		Description: "Identify cross-resource reference fields (ARN/ID/Name pointing at another AWS " +
			"resource) in an ACK controller CRD that are not configured with a references block in generator.yaml.",
		Tools:        referenceTools(fetcher),
		OutputSchema: referenceOutputSchema,
		System:       referenceSystemPrompt,
		Prompt:       referenceUserPrompt,
		Evaluate:     evaluateReference,
		Summarize:    summarizeReference,
	}
}

// referenceFindings is the decoded shape of the reference issue's report.
type referenceFindings struct {
	Fields  []referenceFieldFinding `json:"fields"`
	Summary string                  `json:"summary"`
}

// referenceFieldFinding is one reported field in the reference findings.
type referenceFieldFinding struct {
	ModelField     string  `json:"model_field"`
	ACKFieldPath   string  `json:"ack_field_path"`
	TargetResource string  `json:"target_resource"`
	TargetService  string  `json:"target_service"`
	Signal         string  `json:"signal"`
	HasReference   bool    `json:"has_reference"`
	Confidence     float64 `json:"confidence"`
	Reasoning      string  `json:"reasoning"`
}

// evaluateReference passes only when every cross-resource reference field that
// maps to an ACK CRD field is already configured with a references block.
// Model-only fields (no ACK field path) cannot be configured, so they do not
// affect the verdict.
func evaluateReference(raw json.RawMessage) (Verdict, error) {
	var rf referenceFindings
	if err := json.Unmarshal(raw, &rf); err != nil {
		return VerdictError, fmt.Errorf("parsing reference findings: %w", err)
	}
	for _, f := range rf.Fields {
		if f.ACKFieldPath == "" {
			continue
		}
		if !f.HasReference {
			return VerdictFail, nil
		}
	}
	return VerdictPass, nil
}

// summarizeReference renders the reduced result: the reference fields missing a
// references block (with what they point at), the ones already configured, and
// any model reference fields absent from the CRD.
func summarizeReference(raw json.RawMessage) string {
	var rf referenceFindings
	if err := json.Unmarshal(raw, &rf); err != nil {
		return "unparseable findings"
	}

	var missing, configured, unmapped []string
	for _, f := range rf.Fields {
		switch {
		case f.ACKFieldPath == "":
			unmapped = append(unmapped, f.ModelField)
		case f.HasReference:
			configured = append(configured, f.ACKFieldPath)
		default:
			missing = append(missing, fmt.Sprintf("%s -> %s (%s)", f.ACKFieldPath, referenceTarget(f), f.Signal))
		}
	}

	var b strings.Builder
	if len(missing) > 0 {
		fmt.Fprintf(&b, "missing references: %s\n", strings.Join(missing, ", "))
	}
	if len(configured) > 0 {
		fmt.Fprintf(&b, "already configured: %s\n", strings.Join(configured, ", "))
	}
	if len(unmapped) > 0 {
		fmt.Fprintf(&b, "model-only (no CRD field): %s\n", strings.Join(unmapped, ", "))
	}
	if b.Len() == 0 {
		return "no reference fields identified"
	}
	return strings.TrimRight(b.String(), "\n")
}

// referenceTarget renders the referenced resource for the summary, qualifying it
// with the service when known (for example "iam Role") and falling back to a
// placeholder when neither is identified.
func referenceTarget(f referenceFieldFinding) string {
	switch {
	case f.TargetService != "" && f.TargetResource != "":
		return f.TargetService + " " + f.TargetResource
	case f.TargetResource != "":
		return f.TargetResource
	case f.TargetService != "":
		return f.TargetService
	default:
		return "another resource"
	}
}

// referenceSystemPrompt establishes the agent's role and method for the
// reference issue: what a cross-resource reference is, how ACK configures one,
// which tools to use, and that it must finish by calling the report tool.
func referenceSystemPrompt(target Target) string {
	return fmt.Sprintf(`You are an expert reviewer of AWS Controllers for Kubernetes (ACK) service controllers.

Your task: for the %q resource of the %q controller, identify fields that are cross-resource references (they hold an ARN, ID, or Name that points at another AWS resource) but are NOT configured with a references block in generator.yaml.

Background:
- ACK generator.yaml can configure a field with a "references" block, which lets a user point at another Kubernetes resource instead of hardcoding an ARN/ID/Name. A field that holds an identifier of another AWS resource but lacks this block is the problem this issue looks for.
- References are mutually exclusive with document fields: a reference holds an identifier, not a JSON/policy document.

You have a single tool, %s, which searches one source with a regular expression:
- %q: every spec field of the resource's CRD as JSON, one field per line. Each record has:
  - "path": the full CRD field path in dot notation, e.g. "lambdaConfig.preSignUp".
  - "type": the field's type. Only string-valued fields are listed, because an identifier is always a string.
  - "description" and "description_source": what the field holds. "crd" means the description came from the CRD itself; "model" means it was resolved from the service's AWS API model, which is the only place nested fields are documented. Weigh both equally.
  - "pattern": the API model's validation pattern, when the field has one. An ARN pattern such as "^arn:aws[a-z\-]*:iam::\d{12}:role/?...$" names the referenced service and resource type outright and is the strongest signal available.
  - "is_reference": whether generator.yaml already configures the field as a cross-resource reference.
  - "is_immutable": a supporting signal, since a reference is frequently set once (a KMS key, IAM role, parent ID, or subnet).
  - "is_primary_key": the resource's own primary key. Exclude it when it is the resource's own identifier, but note a sub-resource's primary key can itself be a reference to its parent.
This is the only thing you can read. Every field you can report on is in it, so every finding must carry a non-empty ack_field_path.

Signals for identifying a reference, in order of confidence:
1. A "pattern" that is an ARN template naming another service and resource type (signal "arn_pattern", highest confidence).
2. Field name ending in "ARN" with a description naming another service/resource (signal "arn_suffix").
3. Field name ending in "ID" with a description like "The ID of the ..." (signal "id_suffix").
4. A description explicitly saying to use another API/service to obtain the value (signal "doc_mention").
5. Field name ending in "Name" with a description naming a specific resource type (signal "name_suffix", lower confidence).

EXCLUDE (do not report): JSON/policy document fields; tags; enum fields; the resource's own primary key (its own name/ID/ARN); and free-form strings such as descriptions.

Method — perform your research in this order:
1. grep %q for "arn:aws" to find the fields whose pattern is an ARN template. These are your highest-confidence candidates.
2. grep %q for "(ARN|ID|Name)\"" to find the remaining candidates by name, and read each one's description to judge whether it points at another AWS resource or is just local data.
3. grep %q for descriptions that name another service or tell the caller to call another API (for example "iam|kms|lambda|s3|the ID of the|use the .* API"), which catches references whose names give nothing away.
4. For each candidate, read its "is_reference" marking. A reference field with is_reference=false is the problem this issue looks for; give those the highest confidence consistent with the signal.
5. Report your findings.

Nested fields are the ones most likely to be misconfigured — top-level ARN fields are usually already wired up — so do not stop at the top level of the path tree.

When you have gathered enough evidence, call the %s tool exactly once with your complete findings. Do not answer in prose; report only through that tool.`,
		target.Resource, target.Controller,
		toolGrep, sourceFields,
		sourceFields, sourceFields, sourceFields, reportToolName)
}

// referenceUserPrompt is the initial user turn that starts the investigation.
func referenceUserPrompt(target Target) string {
	return fmt.Sprintf("Scan the %q resource of the %q controller for cross-resource reference fields "+
		"(ARN/ID/Name pointing at another AWS resource) that are missing a references block in generator.yaml.",
		target.Resource, target.Controller)
}
