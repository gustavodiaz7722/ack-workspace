package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// emptyObjectSchema is a no-argument tool input schema used by test spy tools.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// requireGrep skips a test when no grep binary is available.
func requireGrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(grepBinary); err != nil {
		t.Skipf("grep not available: %v", err)
	}
}

// fakeFetcher is a DocsFetcher backed by in-memory data. It records the slug it
// was last asked to fetch so tests can assert routing.
type fakeFetcher struct {
	resources []string
	docs      map[string]string
	gotSlug   string
}

func (f *fakeFetcher) ListResources(_ context.Context) ([]string, error) {
	return f.resources, nil
}

func (f *fakeFetcher) FetchDoc(_ context.Context, slug string) (string, error) {
	f.gotSlug = slug
	doc, ok := f.docs[slug]
	if !ok {
		return "", fmt.Errorf("no doc for %q", slug)
	}
	return doc, nil
}

const testTerraformDoc = "# acm_certificate\n\nThe policy argument must contain a valid JSON document.\nname - (Required) The name.\ntags - key/value map.\n"

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		resources: []string{"acm_certificate", "acmpca_policy", "s3_bucket"},
		docs:      map[string]string{"acm_certificate": testTerraformDoc},
	}
}

// grepArgs marshals grep tool input.
func grepArgs(t *testing.T, source, ref, pattern string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"source": source, "ref": ref, "pattern": pattern})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGrepToolFields(t *testing.T) {
	requireGrep(t)
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	tool := grepTool(documentSources(newFakeFetcher()))
	target := Target{Controller: "acm", Resource: "Certificate", RepoPath: repo}

	// Grep for a field name → returns its combined record (type + markings).
	out, err := tool.Run(context.Background(), target, grepArgs(t, sourceFields, "", "policyDocument"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source: fields") || !strings.Contains(out, `"path":"policyDocument"`) {
		t.Errorf("fields grep missing record:\n%s", out)
	}
	if !strings.Contains(out, `"is_document":true`) {
		t.Errorf("policyDocument should be marked is_document:\n%s", out)
	}

	// Grep the marking directly → returns the marked field(s).
	out, err = tool.Run(context.Background(), target, grepArgs(t, sourceFields, "", `"is_document":true`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "policyDocument") {
		t.Errorf("is_document filter should return policyDocument:\n%s", out)
	}
}

func TestGrepToolTerraformIndexAndDoc(t *testing.T) {
	requireGrep(t)
	fetcher := newFakeFetcher()
	tool := grepTool(documentSources(fetcher))
	target := Target{Controller: "acm", Resource: "Certificate"}

	// index source: grep the slug list (context 0 → matching lines only).
	out, err := tool.Run(context.Background(), target,
		json.RawMessage(`{"source":"terraform_index","pattern":"^acm","context":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "acm_certificate") || strings.Contains(out, "s3_bucket") {
		t.Errorf("terraform_index grep = %s", out)
	}

	// doc source: default slug is acm_certificate.
	out, err = tool.Run(context.Background(), target, grepArgs(t, sourceTerraformDoc, "", "JSON"))
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.gotSlug != "acm_certificate" {
		t.Errorf("slug = %q, want acm_certificate", fetcher.gotSlug)
	}
	if !strings.Contains(out, "JSON document") {
		t.Errorf("terraform_doc grep missing match:\n%s", out)
	}

	// doc source with an explicit ref slug.
	if _, err := tool.Run(context.Background(), target, grepArgs(t, sourceTerraformDoc, "acmpca_policy", "x")); err == nil {
		if fetcher.gotSlug != "acmpca_policy" {
			t.Errorf("ref slug = %q, want acmpca_policy", fetcher.gotSlug)
		}
	}
}

func TestGrepToolUnknownSource(t *testing.T) {
	requireGrep(t)
	tool := grepTool(documentSources(newFakeFetcher()))
	if _, err := tool.Run(context.Background(), Target{Controller: "acm", Resource: "Certificate"},
		grepArgs(t, "bogus", "", "x")); err == nil {
		t.Error("expected error for an unknown source")
	}
}

func TestGrepToolInvalidPattern(t *testing.T) {
	requireGrep(t)
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	tool := grepTool(documentSources(newFakeFetcher()))
	if _, err := tool.Run(context.Background(), Target{Controller: "acm", Resource: "Certificate", RepoPath: repo},
		grepArgs(t, sourceFields, "", "(")); err == nil {
		t.Error("expected error for an invalid regular expression")
	}
}

func TestToSnakeCase(t *testing.T) {
	cases := map[string]string{
		"Certificate":  "certificate",
		"LoadBalancer": "load_balancer",
		"already":      "already",
	}
	for in, want := range cases {
		if got := toSnakeCase(in); got != want {
			t.Errorf("toSnakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripDocExtension(t *testing.T) {
	cases := map[string]string{
		"acm_certificate.html.markdown": "acm_certificate",
		"s3_bucket.markdown":            "s3_bucket",
		"notes.txt":                     "",
	}
	for in, want := range cases {
		if got := stripDocExtension(in); got != want {
			t.Errorf("stripDocExtension(%q) = %q, want %q", in, got, want)
		}
	}
}
