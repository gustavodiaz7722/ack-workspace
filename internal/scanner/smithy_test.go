package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeControllerRepoWithGenerator creates a controller checkout whose
// generator.yaml is caller-supplied, for the cases (renames, custom field source
// operations) that need configuration the shared fixture does not carry.
func writeControllerRepoWithGenerator(t *testing.T, root, name, generator string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	crds := filepath.Join(repo, "helm", "crds")
	if err := os.MkdirAll(crds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, generatorFileName), []byte(generator), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crds, "acm_certificate.yaml"), []byte(testCRDYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestBuildDocIndexPaths(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	idx, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}

	// Member names are transformed with ACK's own names.New(...).CamelLower, the
	// same function the code-generator uses for the CRD's JSON tag, so RoleArn
	// must land on the CRD's actual "roleARN" path rather than "roleArn".
	doc, ok := idx.lookup("roleARN")
	if !ok {
		t.Fatalf("roleARN not resolved; paths present: %v", sortedPaths(idx))
	}
	if !strings.Contains(doc.Description, "ARN of the IAM role") {
		t.Errorf("roleARN description = %q", doc.Description)
	}
	// The target shape's pattern comes along, which is the strongest reference
	// signal available.
	if !strings.Contains(doc.Pattern, "arn:aws:iam::") {
		t.Errorf("roleARN pattern = %q, want the IAM role ARN pattern", doc.Pattern)
	}

	// A list of structures is unwrapped onto the parent path without an index
	// segment, matching how ACK flattens arrays into the CRD.
	if doc, ok := idx.lookup("tags.key"); !ok || !strings.Contains(doc.Description, "tag key") {
		t.Errorf("tags.key = %+v (found=%v), want the tag key documentation", doc, ok)
	}

	// Unions carry members just like structures and must be descended into.
	if doc, ok := idx.lookup("options.kmsKeyID"); !ok || !strings.Contains(doc.Description, "KMS key") {
		t.Errorf("options.kmsKeyID = %+v (found=%v); paths present: %v", doc, ok, sortedPaths(idx))
	}
}

func TestBuildDocIndexAppliesRenames(t *testing.T) {
	root := t.TempDir()
	generator := `resources:
  Certificate:
    renames:
      operations:
        CreateCertificate:
          input_fields:
            CertificateName: Name
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	idx, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	// The renamed field must resolve under the name ACK exposes, not the model's.
	if doc, ok := idx.byPath["name"]; !ok || !strings.Contains(doc.Description, "certificate name") {
		t.Errorf("name = %+v (found=%v), want the renamed CertificateName documentation", doc, ok)
	}
	if _, ok := idx.byPath["certificateName"]; ok {
		t.Error("the original model name should not remain once renamed")
	}
}

func TestBuildDocIndexIncludesCustomFieldOperations(t *testing.T) {
	root := t.TempDir()
	// A custom field sourced from another operation pulls that operation's input
	// shape into the walk. These synthesized subtrees are entirely nested, so they
	// are exactly the fields with no CRD description.
	generator := `resources:
  Certificate:
    fields:
      Permission:
        from:
          operation: AddPermission
          path: Permission
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	idx, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	if doc, ok := idx.byPath["sourceARN"]; !ok || !strings.Contains(doc.Description, "calling service") {
		t.Errorf("sourceARN = %+v (found=%v), want documentation from AddPermissionRequest", doc, ok)
	}
}

func TestUnambiguousMemberDocsRejectsAmbiguity(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	idx, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	// "Description" is declared twice with different text, so the member-name
	// fallback must refuse it: attaching an arbitrary one of several meanings to a
	// field would mislead the agent more than leaving the description empty.
	if _, ok := idx.byMember["description"]; ok {
		t.Error("an ambiguous member name must not enter the fallback index")
	}
	// An unambiguous name is available to the fallback.
	if _, ok := idx.byMember["key"]; !ok {
		t.Error("an unambiguous member name should be in the fallback index")
	}
}

func TestDocIndexLookupToleratesInitialismSkew(t *testing.T) {
	// A controller generated against a different names table can spell an
	// initialism differently (a CRD's "argoCD" against a freshly computed
	// "argoCd"), so the join falls back to a case-insensitive match.
	idx := docIndex{
		byPath:    map[string]smithyDoc{"configuration.argoCd.role": {Description: "the role"}},
		byMember:  map[string]smithyDoc{},
		lowerPath: map[string]string{"configuration.argocd.role": "configuration.argoCd.role"},
	}
	if doc, ok := idx.lookup("configuration.argoCD.role"); !ok || doc.Description != "the role" {
		t.Errorf("case-insensitive lookup failed: %+v (found=%v)", doc, ok)
	}
}

func TestBuildDocIndexRejectsBadModel(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	if _, err := buildDocIndex("not json", repo, "Certificate"); err == nil {
		t.Error("expected an error for an unparseable model")
	}
	if _, err := buildDocIndex(`{"smithy":"2.0"}`, repo, "Certificate"); err == nil {
		t.Error("expected an error for a model with no shapes")
	}
}

func sortedPaths(idx docIndex) []string {
	out := make([]string, 0, len(idx.byPath))
	for p := range idx.byPath {
		out = append(out, p)
	}
	return out
}
