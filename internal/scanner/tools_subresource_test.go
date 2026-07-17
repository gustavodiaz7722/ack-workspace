package scanner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildFieldTypeIndex(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	out, err := buildFieldTypeIndex(repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	var records []fieldTypeRecord
	if err := json.Unmarshal([]byte(out), &records); err != nil {
		t.Fatalf("field-type index is not valid JSON: %v\n%s", err, out)
	}
	byPath := map[string]fieldTypeRecord{}
	for _, r := range records {
		byPath[r.Path] = r
	}

	// The nested array field and its element sub-field are both present, so the
	// agent can see the shape of a candidate embedded sub-resource.
	if tags, ok := byPath["tags"]; !ok || tags.Type != "array" {
		t.Errorf("tags = %+v (present=%v), want an array field", tags, ok)
	}
	if _, ok := byPath["tags.key"]; !ok {
		t.Errorf("expected nested element field tags.key; got %v", byPath)
	}

	// The index carries no markings (raw JSON has no is_document/is_reference).
	if strings.Contains(out, "is_document") || strings.Contains(out, "is_reference") {
		t.Errorf("field-type index should carry no markings:\n%s", out)
	}
}

func TestGrepToolSubresourceSources(t *testing.T) {
	requireGrep(t)
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")
	tool := grepTool(subresourceSources(newFakeFetcher()))
	target := Target{Controller: "acm", Resource: "Certificate", RepoPath: repo}

	// The controller_resources source lists the Kinds this controller manages.
	out, err := tool.Run(context.Background(), target,
		grepArgs(t, sourceControllerResources, "", "Certificate"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source: controller_resources") || !strings.Contains(out, "Certificate") {
		t.Errorf("controller_resources grep unexpected:\n%s", out)
	}

	// The fields source is the structural (path/type) index.
	out, err = tool.Run(context.Background(), target, grepArgs(t, sourceFields, "", `"type":"array"`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source: fields") || !strings.Contains(out, "tags") {
		t.Errorf("fields grep unexpected:\n%s", out)
	}

	// The Terraform sources are shared with the document issue and still work.
	out, err = tool.Run(context.Background(), target,
		json.RawMessage(`{"source":"terraform_index","pattern":"^acm","context":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "acm_certificate") {
		t.Errorf("terraform_index grep unexpected:\n%s", out)
	}
}
