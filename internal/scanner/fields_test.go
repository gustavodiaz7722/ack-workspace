package scanner

import (
	"encoding/json"
	"testing"
)

func TestBuildFieldIndex(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	out, err := buildFieldIndex(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}

	// The index is a valid JSON array of field records.
	var records []fieldRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("field index is not valid JSON: %v\n%s", err, out)
	}
	byPath := map[string]fieldRecord{}
	for _, r := range records {
		byPath[r.Path] = r
	}

	// A generator marking (PascalCase "PolicyDocument") resolves onto the CRD's
	// camelCase field via case-insensitive path matching.
	pd, ok := byPath["policyDocument"]
	if !ok {
		t.Fatalf("missing policyDocument; got paths %v", byPath)
	}
	if pd.Type != "string" || !pd.IsDocument {
		t.Errorf("policyDocument = %+v, want type string + is_document true", pd)
	}

	// An unmarked field is present and not flagged.
	if dn, ok := byPath["domainName"]; !ok || dn.IsDocument || dn.IsIAMPolicy {
		t.Errorf("domainName = %+v (present=%v), want present and unmarked", dn, ok)
	}

	// Array element fields descend with a dotted path (no index segment).
	if _, ok := byPath["tags.key"]; !ok {
		t.Errorf("expected array element field tags.key; got paths %v", byPath)
	}

	// Generated reference fields are filtered out entirely — the container, its
	// "from" object, and the name/namespace leaves.
	for _, refPath := range []string{"roleRef", "roleRef.from", "roleRef.from.name", "roleRef.from.namespace"} {
		if _, ok := byPath[refPath]; ok {
			t.Errorf("reference field %q should have been filtered out", refPath)
		}
	}

	// A field configured as a cross-resource reference in generator.yaml (RoleARN)
	// holds an ARN, never a document, so it is filtered out of the index too.
	if _, ok := byPath["roleARN"]; ok {
		t.Errorf("reference-configured field roleARN should have been filtered out; got paths %v", byPath)
	}
}

func TestFilterReferenceFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "deliveryPolicy"},
		{Path: "roleRef"},
		{Path: "roleRef.from"},
		{Path: "roleRef.from.name"},
		{Path: "roleRef.from.namespace"},
		{Path: "securityGroupRefs"},
		{Path: "securityGroupRefs.from.name"},
		{Path: "notAReference"}, // ends in nothing special; kept
	}
	got := map[string]bool{}
	for _, r := range filterReferenceFields(in) {
		got[r.Path] = true
	}
	if !got["deliveryPolicy"] || !got["notAReference"] {
		t.Errorf("non-reference fields were dropped: %v", got)
	}
	for _, dropped := range []string{"roleRef", "roleRef.from", "roleRef.from.name", "securityGroupRefs", "securityGroupRefs.from.name"} {
		if got[dropped] {
			t.Errorf("reference field %q should have been filtered", dropped)
		}
	}
}

func TestFilterConfiguredReferenceFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "policyDocument"},
		{Path: "roleARN"},
		// A nested reference (generator.yaml keys with dotted paths) and any
		// children beneath it are dropped; matching is case-insensitive.
		{Path: "lambdaConfig.customMessage"},
		{Path: "lambdaConfig.customMessage.child"},
		{Path: "lambdaConfig.postConfirmation"},
		{Path: "domainName"}, // not a reference; kept
	}
	// Reference paths as they appear in generator.yaml (PascalCase), lowercased.
	refPaths := map[string]bool{
		"rolearn":                       true,
		"lambdaconfig.custommessage":    true,
		"lambdaconfig.postconfirmation": true,
	}

	got := map[string]bool{}
	for _, r := range filterConfiguredReferenceFields(in, refPaths) {
		got[r.Path] = true
	}
	if !got["policyDocument"] || !got["domainName"] {
		t.Errorf("non-reference fields were dropped: %v", got)
	}
	for _, dropped := range []string{"roleARN", "lambdaConfig.customMessage", "lambdaConfig.customMessage.child", "lambdaConfig.postConfirmation"} {
		if got[dropped] {
			t.Errorf("reference field %q should have been filtered", dropped)
		}
	}
}

func TestFilterConfiguredReferenceFieldsNoReferences(t *testing.T) {
	in := []fieldRecord{{Path: "policyDocument"}, {Path: "domainName"}}
	out := filterConfiguredReferenceFields(in, nil)
	if len(out) != 2 {
		t.Errorf("with no references, all %d fields should be kept, got %d", len(in), len(out))
	}
}

func TestBuildFieldIndexUnknownResource(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	if _, err := buildFieldIndex(repo, "Nonexistent"); err == nil {
		t.Error("expected error building index for a resource with no CRD")
	}
}
