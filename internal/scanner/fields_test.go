package scanner

import (
	"encoding/json"
	"strings"
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

	// A document is never immutable nor the resource's own primary key, so fields
	// carrying those markings are filtered out of the document index.
	if _, ok := byPath["domainName"]; ok {
		t.Errorf("immutable field domainName should be filtered from the document index; got %v", byPath)
	}
	if _, ok := byPath["name"]; ok {
		t.Errorf("primary-key field name should be filtered from the document index; got %v", byPath)
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

func TestBuildFieldIndexKeepsOnlyStringFields(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	out, err := buildFieldIndex(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	var records []fieldRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("field index is not valid JSON: %v\n%s", err, out)
	}
	byPath := map[string]fieldRecord{}
	for _, r := range records {
		byPath[r.Path] = r
	}

	// The object/array-of-object container "tags" is a struct, not a candidate
	// for a document, so it is dropped from the index.
	if _, ok := byPath["tags"]; ok {
		t.Errorf("struct field tags should be filtered out of the document index; got %v", byPath)
	}
	// Its string leaves are kept — nested strings survive; only the container is
	// dropped.
	for _, leaf := range []string{"tags.key", "tags.value"} {
		if _, ok := byPath[leaf]; !ok {
			t.Errorf("string leaf %q should be kept; got %v", leaf, byPath)
		}
	}
	// Every remaining record is string-valued.
	for _, r := range records {
		if r.Type != "string" && r.Type != "array" {
			t.Errorf("field %q has non-string type %q; should have been filtered", r.Path, r.Type)
		}
	}
}

// TestBuildFieldIndexOmitsIrrelevantMarkings locks in that the document index
// carries only the markings the document issue needs (is_document,
// is_iam_policy) and never the reference/immutability markings that belong to
// other issues — so the agent is sent only relevant data.
func TestBuildFieldIndexOmitsIrrelevantMarkings(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	out, err := buildFieldIndex(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("document index:\n%s", out)

	for _, irrelevant := range []string{"is_reference", "is_immutable", "is_primary_key"} {
		if strings.Contains(out, irrelevant) {
			t.Errorf("document index should not carry %q:\n%s", irrelevant, out)
		}
	}
	// The relevant document markings are present.
	for _, relevant := range []string{"is_document", "is_iam_policy"} {
		if !strings.Contains(out, relevant) {
			t.Errorf("document index should carry %q:\n%s", relevant, out)
		}
	}
}

func TestFilterKeyAndImmutableFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "policyDocument"},
		{Path: "domainName"},
		{Path: "name"},
		{Path: "region"},
	}
	immutable := map[string]bool{"domainname": true}
	primaryKey := map[string]bool{"name": true}

	got := map[string]bool{}
	for _, r := range filterKeyAndImmutableFields(in, immutable, primaryKey) {
		got[r.Path] = true
	}
	if !got["policyDocument"] || !got["region"] {
		t.Errorf("mutable non-key fields were dropped: %v", got)
	}
	for _, dropped := range []string{"domainName", "name"} {
		if got[dropped] {
			t.Errorf("field %q should have been filtered (immutable/primary key)", dropped)
		}
	}

	// With no markings the slice is returned unchanged.
	if out := filterKeyAndImmutableFields(in, nil, nil); len(out) != len(in) {
		t.Errorf("with no markings all %d fields should be kept, got %d", len(in), len(out))
	}
}

func TestStringValuedPaths(t *testing.T) {
	spec := crdSchemaNode{
		Type: "object",
		Properties: map[string]crdSchemaNode{
			"name":    {Type: "string"},
			"enabled": {Type: "boolean"},
			"count":   {Type: "integer"},
			"aliases": {Type: "array", Items: &crdSchemaNode{Type: "string"}},
			"config": {Type: "object", Properties: map[string]crdSchemaNode{
				"policy": {Type: "string"},
			}},
			"rules": {Type: "array", Items: &crdSchemaNode{Type: "object", Properties: map[string]crdSchemaNode{
				"target": {Type: "string"},
			}}},
		},
	}

	paths := stringValuedPaths(spec)

	// String leaves, arrays of strings, and nested string leaves are string-valued.
	for _, want := range []string{"name", "aliases", "config.policy", "rules.target"} {
		if !paths[want] {
			t.Errorf("expected %q to be string-valued; got %v", want, paths)
		}
	}
	// Non-string scalars, object containers, and arrays of objects are not.
	for _, notWant := range []string{"enabled", "count", "config", "rules"} {
		if paths[notWant] {
			t.Errorf("%q should not be string-valued; got %v", notWant, paths)
		}
	}
}

func TestFilterNonStringFields(t *testing.T) {
	in := []fieldRecord{
		{Path: "name", Type: "string"},
		{Path: "config", Type: "object"},
		{Path: "config.policy", Type: "string"},
		{Path: "count", Type: "integer"},
		{Path: "aliases", Type: "array"},
	}
	stringPaths := map[string]bool{"name": true, "config.policy": true, "aliases": true}

	got := map[string]bool{}
	for _, r := range filterNonStringFields(in, stringPaths) {
		got[r.Path] = true
	}
	if !got["name"] || !got["config.policy"] || !got["aliases"] {
		t.Errorf("string-valued fields were dropped: %v", got)
	}
	for _, dropped := range []string{"config", "count"} {
		if got[dropped] {
			t.Errorf("non-string field %q should have been filtered", dropped)
		}
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
