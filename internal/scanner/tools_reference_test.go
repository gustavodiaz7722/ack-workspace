package scanner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeModelFetcher is a ModelFetcher backed by in-memory data. It records the
// model name it was last asked to fetch so tests can assert name resolution.
type fakeModelFetcher struct {
	models   map[string]string
	gotModel string
}

func (f *fakeModelFetcher) FetchModel(_ context.Context, modelName string) (string, error) {
	f.gotModel = modelName
	m, ok := f.models[modelName]
	if !ok {
		return "", &fetchError{name: modelName}
	}
	return m, nil
}

type fetchError struct{ name string }

func (e *fetchError) Error() string { return "no model for " + e.name }

func newFakeModelFetcher() *fakeModelFetcher {
	return &fakeModelFetcher{models: map[string]string{"acm": testSmithyModel}}
}

func TestBuildReferenceFieldIndex(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	out, err := buildReferenceFieldIndex(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	var records []referenceFieldRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("reference index is not valid JSON: %v\n%s", err, out)
	}
	byPath := map[string]referenceFieldRecord{}
	for _, r := range records {
		byPath[r.Path] = r
	}

	// The reference-configured field (RoleARN in generator.yaml) is KEPT and
	// marked is_reference — unlike the document index, which filters it out.
	rec, ok := byPath["roleARN"]
	if !ok {
		t.Fatalf("roleARN should be present in the reference index; got %v", byPath)
	}
	if !rec.IsReference {
		t.Errorf("roleARN should be marked is_reference: %+v", rec)
	}

	// A plain field is present and not marked.
	if dn, ok := byPath["domainName"]; !ok || dn.IsReference {
		t.Errorf("domainName = %+v (present=%v), want present and not a reference", dn, ok)
	}

	// Unlike the document index, immutable and primary-key fields are KEPT in the
	// reference index and surfaced as signal — a reference is often immutable or a
	// sub-resource's primary key.
	if dn, ok := byPath["domainName"]; !ok || !dn.IsImmutable {
		t.Errorf("domainName = %+v (present=%v), want present and is_immutable true", dn, ok)
	}
	if n, ok := byPath["name"]; !ok || !n.IsPrimaryKey {
		t.Errorf("name = %+v (present=%v), want present and is_primary_key true", n, ok)
	}

	// The generated companion Ref structure is still dropped.
	if _, ok := byPath["roleRef"]; ok {
		t.Error("generated roleRef companion should be filtered out of the reference index")
	}
}

func TestResolveModelName(t *testing.T) {
	root := t.TempDir()

	// No sdk_names → falls back to the controller alias.
	acm := writeControllerRepo(t, root, "acm-controller")
	if got := resolveModelName(acm, "acm"); got != "acm" {
		t.Errorf("resolveModelName(acm) = %q, want acm", got)
	}

	// sdk_names.model_name overrides the alias.
	cognito := filepath.Join(root, "cognitoidentityprovider-controller")
	crds := filepath.Join(cognito, "helm", "crds")
	if err := os.MkdirAll(crds, 0o755); err != nil {
		t.Fatal(err)
	}
	gen := "sdk_names:\n  model_name: cognito-identity-provider\nresources:\n  UserPool: {}\n"
	if err := os.WriteFile(filepath.Join(cognito, generatorFileName), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveModelName(cognito, "cognitoidentityprovider"); got != "cognito-identity-provider" {
		t.Errorf("resolveModelName(cognito) = %q, want cognito-identity-provider", got)
	}

	// An unreadable generator.yaml degrades to the fallback.
	if got := resolveModelName(filepath.Join(root, "nope"), "fallback"); got != "fallback" {
		t.Errorf("resolveModelName(missing) = %q, want fallback", got)
	}
}

func TestGrepToolReferenceSources(t *testing.T) {
	requireGrep(t)
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	fetcher := newFakeModelFetcher()
	tool := grepTool(referenceSources(fetcher))
	target := Target{Controller: "acm", Resource: "Certificate", RepoPath: repo}

	// The fields source exposes the is_reference marking.
	out, err := tool.Run(context.Background(), target, grepArgs(t, sourceFields, "", "roleARN"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source: fields") || !strings.Contains(out, `"is_reference":true`) {
		t.Errorf("fields grep missing reference marking:\n%s", out)
	}

	// The model source is filtered to the resource's shapes and greppable for
	// the arnReference trait; the model name defaults to the controller alias.
	out, err = tool.Run(context.Background(), target, grepArgs(t, sourceModel, "", "arnReference"))
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.gotModel != "acm" {
		t.Errorf("model name = %q, want acm", fetcher.gotModel)
	}
	if !strings.Contains(out, "arnReference") {
		t.Errorf("model grep missing arnReference:\n%s", out)
	}
}
