package scanner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateReference(t *testing.T) {
	cases := []struct {
		name     string
		findings string
		want     Verdict
	}{
		{
			name:     "configured reference passes",
			findings: `{"fields":[{"ack_field_path":"roleARN","has_reference":true}]}`,
			want:     VerdictPass,
		},
		{
			name:     "unconfigured reference fails",
			findings: `{"fields":[{"ack_field_path":"lambdaConfig.preSignUp","has_reference":false}]}`,
			want:     VerdictFail,
		},
		{
			name:     "model-only field is ignored",
			findings: `{"fields":[{"model_field":"KmsKeyId","ack_field_path":"","has_reference":false}]}`,
			want:     VerdictPass,
		},
		{
			name:     "no fields passes",
			findings: `{"fields":[]}`,
			want:     VerdictPass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateReference(json.RawMessage(tc.findings))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvaluateReferenceMalformed(t *testing.T) {
	got, err := evaluateReference(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for malformed findings")
	}
	if got != VerdictError {
		t.Errorf("verdict = %q, want error", got)
	}
}

func TestSummarizeReference(t *testing.T) {
	findings := `{"fields":[
		{"model_field":"RoleArn","ack_field_path":"roleARN","target_service":"iam","target_resource":"Role","signal":"arn_trait","has_reference":true},
		{"model_field":"PreSignUp","ack_field_path":"lambdaConfig.preSignUp","target_service":"lambda","target_resource":"Function","signal":"arn_suffix","has_reference":false},
		{"model_field":"KmsKeyId","ack_field_path":"","target_service":"kms","target_resource":"Key","signal":"id_suffix","has_reference":false}
	]}`
	out := summarizeReference(json.RawMessage(findings))
	if !strings.Contains(out, "missing references: lambdaConfig.preSignUp -> lambda Function (arn_suffix)") {
		t.Errorf("missing the missing-references line:\n%s", out)
	}
	if !strings.Contains(out, "already configured: roleARN") {
		t.Errorf("missing the configured line:\n%s", out)
	}
	if !strings.Contains(out, "model-only (no CRD field): KmsKeyId") {
		t.Errorf("missing the model-only line:\n%s", out)
	}
}

func TestSummarizeReferenceNoFields(t *testing.T) {
	if out := summarizeReference(json.RawMessage(`{"fields":[]}`)); out != "no reference fields identified" {
		t.Errorf("summary = %q, want the empty message", out)
	}
}
