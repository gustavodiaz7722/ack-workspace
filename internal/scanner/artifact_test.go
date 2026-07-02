package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGeneratorYAML = `resources:
  Certificate:
    fields:
      CertificateAuthorityArn:
        is_iam_policy: true
      PolicyDocument:
        is_document: true
      DomainName:
        is_immutable: true
`

const testCRDYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  names:
    kind: Certificate
  versions:
    - name: v1alpha1
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                domainName:
                  type: string
                  description: The fully qualified domain name.
                policyDocument:
                  type: string
                  description: A JSON policy document.
                tags:
                  type: array
                  items:
                    type: object
                    properties:
                      key:
                        type: string
                      value:
                        type: string
                roleRef:
                  type: object
                  properties:
                    from:
                      type: object
                      properties:
                        name:
                          type: string
                        namespace:
                          type: string
            status:
              type: object
              properties:
                state:
                  type: string
`

// writeControllerRepo creates a minimal controller checkout under root and
// returns its path.
func writeControllerRepo(t *testing.T, root, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	crds := filepath.Join(repo, "helm", "crds")
	if err := os.MkdirAll(crds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, generatorFileName), []byte(testGeneratorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crds, "acm_certificate.yaml"), []byte(testCRDYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestDiscoverControllers(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	writeControllerRepo(t, root, "s3-controller")
	// A plain directory without generator.yaml must be ignored.
	if err := os.MkdirAll(filepath.Join(root, "not-a-controller"), 0o755); err != nil {
		t.Fatal(err)
	}

	refs, err := discoverControllers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("found %d controllers, want 2", len(refs))
	}
	// Sorted by alias.
	if refs[0].Alias != "acm" || refs[1].Alias != "s3" {
		t.Errorf("aliases = %q, %q; want acm, s3", refs[0].Alias, refs[1].Alias)
	}
}

func TestDiscoverControllersMissingRoot(t *testing.T) {
	refs, err := discoverControllers(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("want no controllers, got %d", len(refs))
	}
}

func TestFindController(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	// Both the bare alias and the full form must resolve.
	for _, id := range []string{"acm", "acm-controller"} {
		ref, err := findController(root, id)
		if err != nil {
			t.Fatalf("findController(%q): %v", id, err)
		}
		if ref.Alias != "acm" {
			t.Errorf("alias = %q, want acm", ref.Alias)
		}
	}
	if _, err := findController(root, "missing"); err == nil {
		t.Error("expected error for unknown controller")
	}
}

func TestDiscoverResources(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	res, err := discoverResources(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0] != "Certificate" {
		t.Errorf("resources = %v, want [Certificate]", res)
	}
}

func TestFindResourceCRD(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	content, err := findResourceCRD(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "kind: CustomResourceDefinition") || !strings.Contains(content, "policyDocument") {
		t.Errorf("CRD content unexpected:\n%s", content)
	}
	if _, err := findResourceCRD(repo, "Nonexistent"); err == nil {
		t.Error("expected error for a resource with no matching CRD")
	}
}
