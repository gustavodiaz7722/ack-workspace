package scanner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateSubresource(t *testing.T) {
	cases := []struct {
		name     string
		findings string
		want     Verdict
	}{
		{
			name:     "own id + arn + tagging, embedded, not a CRD fails",
			findings: `{"fields":[{"ack_field_path":"ipPermissions","has_own_id":true,"has_own_arn":true,"has_own_tagging":true,"already_separate_crd":false}]}`,
			want:     VerdictFail,
		},
		{
			name:     "own id + tagging (no arn) still fails",
			findings: `{"fields":[{"ack_field_path":"ingressRules","has_own_id":true,"has_own_arn":false,"has_own_tagging":true,"already_separate_crd":false}]}`,
			want:     VerdictFail,
		},
		{
			name:     "already a separate CRD passes",
			findings: `{"fields":[{"ack_field_path":"rules","has_own_id":true,"has_own_arn":true,"has_own_tagging":true,"already_separate_crd":true}]}`,
			want:     VerdictPass,
		},
		{
			name:     "identity but no independent tagging passes",
			findings: `{"fields":[{"ack_field_path":"listeners","has_own_id":true,"has_own_arn":false,"has_own_tagging":false,"already_separate_crd":false}]}`,
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
			got, err := evaluateSubresource(json.RawMessage(tc.findings))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvaluateSubresourceMalformed(t *testing.T) {
	got, err := evaluateSubresource(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for malformed findings")
	}
	if got != VerdictError {
		t.Errorf("verdict = %q, want error", got)
	}
}

func TestSummarizeSubresource(t *testing.T) {
	findings := `{"fields":[
		{"ack_field_path":"ipPermissions","concept":"SecurityGroupRule","terraform_resource":"vpc_security_group_ingress_rule","has_own_id":true,"has_own_arn":true,"has_own_tagging":true,"already_separate_crd":false},
		{"ack_field_path":"tags","concept":"Tag","has_own_id":false,"has_own_arn":false,"has_own_tagging":false,"already_separate_crd":false},
		{"ack_field_path":"","concept":"Listener","has_own_id":true,"has_own_arn":true,"has_own_tagging":true,"already_separate_crd":true}
	]}`
	out := summarizeSubresource(json.RawMessage(findings))
	if !strings.Contains(out, "should be separate CRDs: ipPermissions -> SecurityGroupRule (aws_vpc_security_group_ingress_rule)") {
		t.Errorf("missing the promote line:\n%s", out)
	}
	if !strings.Contains(out, "already separate CRDs: Listener") {
		t.Errorf("missing the already-separate line:\n%s", out)
	}
	// A plain config sub-object (no identity) is neither promoted nor listed.
	if strings.Contains(out, "Tag") {
		t.Errorf("a non-identity sub-object should not appear:\n%s", out)
	}
}

func TestSummarizeSubresourceNoFindings(t *testing.T) {
	if out := summarizeSubresource(json.RawMessage(`{"fields":[]}`)); out != "no embedded sub-resource anti-patterns found" {
		t.Errorf("summary = %q, want the empty message", out)
	}
}
