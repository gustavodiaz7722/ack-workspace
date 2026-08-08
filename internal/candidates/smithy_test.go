package candidates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDocIndex resolves the model documentation for one resource of one
// controller in a single call: decode the model, then index it for the
// resource.
//
// The Indexer separates those two steps so a controller's model is decoded once
// and indexed per resource, so this convenience exists only for tests, which
// assert on one resource at a time.
func buildDocIndex(modelJSON, repoPath, resource string) (docIndex, error) {
	md, err := newModelDocs(modelJSON)
	if err != nil {
		return docIndex{}, err
	}
	return md.indexFor(repoPath, resource)
}

// lookup returns the model's documentation for a CRD field path, discarding the
// join provenance. Production code reads the provenance (it is recorded on
// every candidate record), so only these tests, which assert on the
// documentation alone, have a use for the narrower form.
func (d docIndex) lookup(path string) (smithyDoc, bool) {
	doc, _, ok := d.lookupOrigin(path)
	return doc, ok
}

// writeControllerRepoWithGenerator creates a controller checkout whose
// generator.yaml is caller-supplied, for the cases (renames, custom field
// source operations) that need configuration the shared fixture does not carry.
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
	// field would mislead a reader more than leaving the description empty.
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

// customFieldModel exercises the two shapes a custom field can name: an
// operation input published by the model under the "Request" suffix, and a
// plain struct. generator.yaml uses the older SDK's "Input" spelling for the
// former.
const customFieldModel = `{
  "smithy": "2.0",
  "shapes": {
    "com.amazonaws.acm#CreateCertificateRequest": {
      "type": "structure",
      "members": {
        "CertificateName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The certificate name.</p>" }
        }
      }
    },
    "com.amazonaws.acm#PutSubscriptionFilterRequest": {
      "type": "structure",
      "members": {
        "RoleArn": {
          "target": "com.amazonaws.acm#RoleArnType",
          "traits": { "smithy.api#documentation": "<p>The ARN of an IAM role that grants permission.</p>" }
        },
        "FilterName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>A name for the subscription filter.</p>" }
        }
      }
    },
    "com.amazonaws.acm#RoleArnType": {
      "type": "string",
      "traits": { "smithy.api#pattern": "^arn:aws:iam::[0-9]{12}:role/.+$" }
    },
    "com.amazonaws.acm#DomainName": { "type": "string" }
  }
}`

// A custom field declares a shape, not an operation, and its members become a
// nested subtree of the spec. Those subtrees carry no CRD description at all,
// so if the walk does not mount them every field in them is judgeable by name
// alone.
func TestBuildDocIndexMountsCustomFieldShape(t *testing.T) {
	root := t.TempDir()
	generator := `resources:
  Certificate:
    fields:
      SubscriptionFilters:
        custom_field:
          list_of: PutSubscriptionFilterInput
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	idx, err := buildDocIndex(customFieldModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}

	doc, origin, ok := idx.lookupOrigin("subscriptionFilters.roleARN")
	if !ok {
		t.Fatalf("subscriptionFilters.roleARN not resolved; paths present: %v", sortedPaths(idx))
	}
	if origin != joinOriginPath {
		t.Errorf("origin = %q, want %q", origin, joinOriginPath)
	}
	if !strings.Contains(doc.Description, "ARN of an IAM role") {
		t.Errorf("description = %q", doc.Description)
	}
	// The target shape's pattern comes along, which is the strongest signal that a
	// field is a reference.
	if !strings.Contains(doc.Pattern, "arn:aws:iam::") {
		t.Errorf("pattern = %q, want the IAM role ARN pattern", doc.Pattern)
	}
	if _, ok := idx.lookup("subscriptionFilters.filterName"); !ok {
		t.Errorf("subscriptionFilters.filterName not resolved; paths present: %v", sortedPaths(idx))
	}
}

// generator.yaml names the shape "<Operation>Input" (the older SDK convention)
// while the models publish "<Operation>Request", so the lookup has to reconcile
// the two. A plain struct name must still match directly.
func TestFindShapeReconcilesInputAndRequestSuffixes(t *testing.T) {
	m, err := newModelDocs(customFieldModel)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m.model.findShape("PutSubscriptionFilterInput"); !ok ||
		shortShapeName(got) != "PutSubscriptionFilterRequest" {
		t.Errorf("findShape(Input form) = %q (found=%v), want PutSubscriptionFilterRequest", got, ok)
	}
	if got, ok := m.model.findShape("PutSubscriptionFilterRequest"); !ok ||
		shortShapeName(got) != "PutSubscriptionFilterRequest" {
		t.Errorf("findShape(exact) = %q (found=%v)", got, ok)
	}
	if got, ok := m.model.findShape("RoleArnType"); !ok || shortShapeName(got) != "RoleArnType" {
		t.Errorf("findShape(plain struct name) = %q (found=%v)", got, ok)
	}
	if _, ok := m.model.findShape("NoSuchShape"); ok {
		t.Error("findShape resolved a shape that does not exist")
	}
}

// renamesPerOperationModel has two operations whose inputs rename *different*
// members onto the same ACK field, which is what cloudwatchlogs does with
// CreateLogGroup.LogGroupName and DescribeLogGroups.LogGroupNamePrefix.
const renamesPerOperationModel = `{
  "smithy": "2.0",
  "shapes": {
    "com.amazonaws.acm#CreateCertificateRequest": {
      "type": "structure",
      "members": {
        "CertificateName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>A name for the certificate.</p>" }
        }
      }
    },
    "com.amazonaws.acm#DescribeCertificatesRequest": {
      "type": "structure",
      "members": {
        "CertificateNamePrefix": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>The prefix to match.</p>" }
        }
      }
    },
    "com.amazonaws.acm#DomainName": { "type": "string" }
  }
}`

// A rename must stay scoped to the operation that declares it. Flattening them
// applies one operation's rename while walking another's input shape, which
// attributes the wrong member's documentation to the field — and because the
// Create input is what defines the spec, it has to win the tie.
func TestBuildDocIndexScopesRenamesPerOperation(t *testing.T) {
	root := t.TempDir()
	generator := `resources:
  Certificate:
    fields:
      CreationTime:
        from:
          operation: DescribeCertificates
    renames:
      operations:
        CreateCertificate:
          input_fields:
            CertificateName: Name
        DescribeCertificates:
          input_fields:
            CertificateNamePrefix: Name
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	idx, err := buildDocIndex(renamesPerOperationModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := idx.lookup("name")
	if !ok {
		t.Fatalf("name not resolved; paths present: %v", sortedPaths(idx))
	}
	if !strings.Contains(doc.Description, "A name for the certificate") {
		t.Errorf("name description = %q, want the Create input's documentation", doc.Description)
	}
	if strings.Contains(doc.Description, "prefix") {
		t.Errorf("name took the Describe input's documentation: %q", doc.Description)
	}
}

// Model member casing is inconsistent across services — cloudwatchlogs declares
// "logGroupName" while sagemaker declares "ExecutionRoleArn" — so a rename
// keyed on generator.yaml's PascalCase must match case-insensitively or the
// field lands at its pre-rename path where nothing looks for it.
func TestBuildDocIndexRenamesMatchMemberCaseInsensitively(t *testing.T) {
	root := t.TempDir()
	model := `{
  "smithy": "2.0",
  "shapes": {
    "com.amazonaws.acm#CreateCertificateRequest": {
      "type": "structure",
      "members": {
        "certificateName": {
          "target": "com.amazonaws.acm#DomainName",
          "traits": { "smithy.api#documentation": "<p>A name for the certificate.</p>" }
        }
      }
    },
    "com.amazonaws.acm#DomainName": { "type": "string" }
  }
}`
	generator := `resources:
  Certificate:
    renames:
      operations:
        CreateCertificate:
          input_fields:
            CertificateName: Name
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	idx, err := buildDocIndex(model, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	if doc, ok := idx.lookup("name"); !ok || !strings.Contains(doc.Description, "A name for the certificate") {
		t.Errorf("name = %+v (found=%v); paths present: %v", doc, ok, sortedPaths(idx))
	}
}

// Several roots can contribute the same field path, and walkShape resolves that
// by keeping whichever arrived first with documentation. Iterating the shape
// map directly would make the winner depend on Go's map order, so two runs over
// an unchanged repo could disagree — which defeats the point of a reproducible
// index. Create must sort first because it is the operation that defines the
// spec.
func TestRootShapesOrderIsDeterministicCreateFirst(t *testing.T) {
	md, err := newModelDocs(renamesPerOperationModel)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := 0; i < 20; i++ {
		roots := md.model.rootShapes("Certificate", []string{"DescribeCertificates"})
		if len(roots) != 2 {
			t.Fatalf("got %d roots, want 2: %v", len(roots), roots)
		}
		if shortShapeName(roots[0]) != "CreateCertificateRequest" {
			t.Fatalf("roots[0] = %q, want CreateCertificateRequest first", roots[0])
		}
		joined := strings.Join(roots, ",")
		if i == 0 {
			first = joined
			continue
		}
		if joined != first {
			t.Fatalf("root order varies between runs: %q vs %q", joined, first)
		}
	}
}

func TestOperationForShape(t *testing.T) {
	cases := map[string]string{
		"com.amazonaws.logs#CreateLogGroupRequest": "createloggroup",
		"CreateLogGroupInput":                      "createloggroup",
		"PutSubscriptionFilterMessage":             "putsubscriptionfilter",
		"IpPermission":                             "ippermission",
	}
	for in, want := range cases {
		if got := operationForShape(in); got != want {
			t.Errorf("operationForShape(%q) = %q, want %q", in, got, want)
		}
	}
}
