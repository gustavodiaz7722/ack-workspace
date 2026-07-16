package scanner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// issueDocumentNumber is the stable identifier of the JSON/IAM policy document
// issue on the command line and in the registry.
const issueDocumentNumber = 1

// documentOutputSchema is the JSON Schema the agent's findings must conform to
// for the document issue. It is the report tool's input schema, so the model is
// steered to emit exactly this shape: per candidate field, whether it holds a
// document, whether generator.yaml already marks it, a confidence score, and the
// reasoning behind that confidence.
var documentOutputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "fields": {
      "type": "array",
      "description": "One entry per field judged to hold a JSON/YAML or IAM policy document.",
      "items": {
        "type": "object",
        "properties": {
          "terraform_field": {
            "type": "string",
            "description": "The Terraform doc field/argument that identified this as a document, e.g. \"delivery_policy\". Empty if there is no corresponding Terraform field."
          },
          "ack_field_path": {
            "type": "string",
            "description": "Full ACK CRD field path in dot notation, e.g. \"spec.deliveryPolicy\" or \"domainValidationOptions.validationDomain\"."
          },
          "document_kind": {
            "type": "string",
            "enum": ["json", "iam_policy"],
            "description": "Whether the field holds a general JSON/YAML document or specifically an IAM policy document."
          },
          "marked_in_generator": {
            "type": "boolean",
            "description": "Whether generator.yaml already marks this field is_document or is_iam_policy."
          },
          "current_marking": {
            "type": "string",
            "enum": ["is_document", "is_iam_policy", "none"],
            "description": "The field's current generator.yaml marking, or \"none\"."
          },
          "confidence": {
            "type": "number",
            "minimum": 0,
            "maximum": 1,
            "description": "Confidence, 0.0-1.0, that this field holds a document."
          },
          "reasoning": {
            "type": "string",
            "description": "Why this field is (or is not) a document field and the basis for the confidence."
          }
        },
        "required": ["terraform_field", "ack_field_path", "document_kind", "marked_in_generator", "current_marking", "confidence", "reasoning"],
        "additionalProperties": false
      }
    },
    "summary": {
      "type": "string",
      "description": "Short overall summary, including any unmarked document fields that need is_document/is_iam_policy added."
    }
  },
  "required": ["fields", "summary"],
  "additionalProperties": false
}`)

// newDocumentIssue builds the JSON/IAM policy document issue bound to the given
// docs fetcher. It detects CRD string fields that hold a JSON or IAM policy
// document but are not correctly marked is_document / is_iam_policy in
// generator.yaml.
func newDocumentIssue(fetcher DocsFetcher) Issue {
	return Issue{
		Number: issueDocumentNumber,
		Name:   "json-document-fields",
		Description: "Identify JSON/IAM policy document fields in an ACK controller CRD that " +
			"are not properly marked is_document/is_iam_policy in generator.yaml.",
		Tools:        documentTools(fetcher),
		OutputSchema: documentOutputSchema,
		System:       documentSystemPrompt,
		Prompt:       documentUserPrompt,
		Evaluate:     evaluateDocument,
		Summarize:    summarizeDocument,
	}
}

// documentFindings is the decoded shape of the document issue's report.
type documentFindings struct {
	Fields  []documentField `json:"fields"`
	Summary string          `json:"summary"`
}

// documentField is one reported field in the document findings.
type documentField struct {
	TerraformField    string  `json:"terraform_field"`
	ACKFieldPath      string  `json:"ack_field_path"`
	DocumentKind      string  `json:"document_kind"`
	MarkedInGenerator bool    `json:"marked_in_generator"`
	CurrentMarking    string  `json:"current_marking"`
	Confidence        float64 `json:"confidence"`
	Reasoning         string  `json:"reasoning"`
}

// expectedMarking is the generator.yaml marking the field should carry given the
// kind of document it holds.
func (f documentField) expectedMarking() string {
	if f.DocumentKind == "iam_policy" {
		return "is_iam_policy"
	}
	return "is_document"
}

// correctlyMarked reports whether the field's current marking matches the
// marking expected for its document kind.
func (f documentField) correctlyMarked() bool {
	return f.CurrentMarking == f.expectedMarking()
}

// evaluateDocument passes only when every document field that maps to an ACK CRD
// field is correctly marked. Terraform-only fields (no ACK field path) cannot be
// marked, so they do not affect the verdict.
func evaluateDocument(raw json.RawMessage) (Verdict, error) {
	var df documentFindings
	if err := json.Unmarshal(raw, &df); err != nil {
		return VerdictError, fmt.Errorf("parsing document findings: %w", err)
	}
	for _, f := range df.Fields {
		if f.ACKFieldPath == "" {
			continue
		}
		if !f.correctlyMarked() {
			return VerdictFail, nil
		}
	}
	return VerdictPass, nil
}

// summarizeDocument renders the reduced result: the correctly-marked field
// paths, the incorrectly-marked ones (with what they are vs. what they should
// be), and any Terraform document fields absent from the CRD.
func summarizeDocument(raw json.RawMessage) string {
	var df documentFindings
	if err := json.Unmarshal(raw, &df); err != nil {
		return "unparseable findings"
	}

	var correct, incorrect, unmapped []string
	for _, f := range df.Fields {
		switch {
		case f.ACKFieldPath == "":
			unmapped = append(unmapped, f.TerraformField)
		case f.correctlyMarked():
			correct = append(correct, f.ACKFieldPath)
		default:
			marking := f.CurrentMarking
			if marking == "" {
				marking = "none"
			}
			incorrect = append(incorrect, fmt.Sprintf("%s (is %s, expected %s)", f.ACKFieldPath, marking, f.expectedMarking()))
		}
	}

	var b strings.Builder
	if len(incorrect) > 0 {
		fmt.Fprintf(&b, "incorrectly marked: %s\n", strings.Join(incorrect, ", "))
	}
	if len(correct) > 0 {
		fmt.Fprintf(&b, "correctly marked: %s\n", strings.Join(correct, ", "))
	}
	if len(unmapped) > 0 {
		fmt.Fprintf(&b, "terraform-only (no CRD field): %s\n", strings.Join(unmapped, ", "))
	}
	if b.Len() == 0 {
		return "no document fields identified"
	}
	return strings.TrimRight(b.String(), "\n")
}

// documentSystemPrompt establishes the agent's role and method for the document
// issue: what the issue is, how ACK marks document fields, which tools to use,
// and that it must finish by calling the report tool.
func documentSystemPrompt(target Target) string {
	return fmt.Sprintf(`You are an expert reviewer of AWS Controllers for Kubernetes (ACK) service controllers.

Your task: for the %q resource of the %q controller, identify fields that hold a JSON/YAML document or an IAM policy document but are NOT correctly marked in generator.yaml.

Background:
- ACK generator.yaml can mark a field "is_document: true" (free-form JSON/YAML document) or "is_iam_policy: true" (an IAM policy document). Marking a field ensures the controller compares it as a structured document instead of as a raw string.
- String-typed CRD fields are the candidates that might hold a document.
- The Terraform AWS provider documentation often states when a field must contain a JSON document or a policy, which is strong external evidence.

You have a single tool, %s, which searches a fixed set of sources with a regular expression:
- %q: every spec field of the resource's CRD as JSON, one field per line, each with its path, type, description, and its is_document / is_iam_policy markings from generator.yaml. This is your primary source — it already fuses the CRD and generator.yaml.
- %q: the list of Terraform doc slugs (grep to find this resource's slug).
- %q: a Terraform doc, selected with the "ref" slug.
These are the only things you can read.

Method — perform your research in this order:
1. Identify the JSON/policy document fields in the Terraform docs. grep %q (for example for the controller name) to find this resource's slug, then grep %q with ref set to that slug to find every Terraform argument that holds a JSON or policy document. Look for indicators such as:
   - A description mentioning "JSON", "policy document", "JSON-encoded", or "jsonencode"
   - Example values showing JSON content or use of jsonencode()
   - Field names containing "policy", "document", "json", or "configuration"
   - References to aws_iam_policy_document data sources
   Good starting patterns are "json|policy|document|jsonencode" and "iam_policy_document".
2. Independently, search the ACK CRD fields themselves for JSON references. grep %q with a pattern like "json|policy|document" — the field descriptions often state directly that a field must hold JSON (for example "The policy must be in JSON string format"), which identifies a document field even when the Terraform match is indirect. Treat any such field as a candidate.
3. Cross-reference each candidate (from steps 1 and 2) to the matching ACK CRD field. grep %q to find the CRD field whose name corresponds to the Terraform field (Terraform uses snake_case, ACK uses camelCase, e.g. delivery_policy -> deliveryPolicy). Record the full ACK field path in dot notation.
4. Determine which of those ACK CRD fields are correctly marked. The %q record for each field shows is_document and is_iam_policy; a document field with both false is NOT correctly marked.
5. Report your findings.

A field that clearly holds a JSON document or IAM policy but has is_document=false and is_iam_policy=false is the problem this issue looks for; give those the highest confidence. If a Terraform document field has no corresponding ACK CRD field, note it (with an empty ack_field_path). Report as soon as you have completed steps 1-4.

When you have gathered enough evidence, call the %s tool exactly once with your complete findings. Do not answer in prose; report only through that tool.`,
		target.Resource, target.Controller,
		toolGrep, sourceFields, sourceTerraformIndex, sourceTerraformDoc,
		sourceTerraformIndex, sourceTerraformDoc, sourceFields, sourceFields, sourceFields, reportToolName)
}

// documentUserPrompt is the initial user turn that starts the investigation.
func documentUserPrompt(target Target) string {
	return fmt.Sprintf("Scan the %q resource of the %q controller for JSON/IAM policy document "+
		"fields that are missing an is_document or is_iam_policy marking in generator.yaml.",
		target.Resource, target.Controller)
}

// defaultIssues returns the issues registered by default, wiring the given
// fetchers into the issues that consult external sources: the docs fetcher into
// the document issue (Terraform provider docs) and the model fetcher into the
// reference issue (AWS Smithy API models). New issues are added here as they are
// implemented.
func defaultIssues(docs DocsFetcher, models ModelFetcher) []Issue {
	return []Issue{
		newDocumentIssue(docs),
		newReferenceIssue(models),
	}
}
