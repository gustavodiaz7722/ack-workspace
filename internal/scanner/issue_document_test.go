package scanner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateDocument(t *testing.T) {
	cases := []struct {
		name     string
		findings string
		want     Verdict
	}{
		{
			name:     "all correctly marked",
			findings: `{"fields":[{"ack_field_path":"deliveryPolicy","document_kind":"json","current_marking":"is_document"}]}`,
			want:     VerdictPass,
		},
		{
			name:     "unmarked document field fails",
			findings: `{"fields":[{"ack_field_path":"filterPolicy","document_kind":"json","current_marking":"none"}]}`,
			want:     VerdictFail,
		},
		{
			name:     "wrong marking kind fails",
			findings: `{"fields":[{"ack_field_path":"policy","document_kind":"iam_policy","current_marking":"is_document"}]}`,
			want:     VerdictFail,
		},
		{
			name:     "terraform-only field is ignored",
			findings: `{"fields":[{"terraform_field":"replay_policy","ack_field_path":"","document_kind":"json","current_marking":"none"}]}`,
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
			got, err := evaluateDocument(json.RawMessage(tc.findings))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvaluateDocumentMalformed(t *testing.T) {
	got, err := evaluateDocument(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for malformed findings")
	}
	if got != VerdictError {
		t.Errorf("verdict = %q, want error", got)
	}
}

func TestSummarizeDocument(t *testing.T) {
	findings := `{"fields":[
		{"ack_field_path":"deliveryPolicy","document_kind":"json","current_marking":"is_document"},
		{"ack_field_path":"filterPolicy","document_kind":"json","current_marking":"none"},
		{"terraform_field":"replay_policy","ack_field_path":"","document_kind":"json","current_marking":"none"}
	]}`
	out := summarizeDocument(json.RawMessage(findings))
	if !strings.Contains(out, "correctly marked: deliveryPolicy") {
		t.Errorf("missing correctly-marked line:\n%s", out)
	}
	if !strings.Contains(out, "incorrectly marked: filterPolicy (is none, expected is_document)") {
		t.Errorf("missing incorrectly-marked line:\n%s", out)
	}
	if !strings.Contains(out, "terraform-only (no CRD field): replay_policy") {
		t.Errorf("missing terraform-only line:\n%s", out)
	}
}
