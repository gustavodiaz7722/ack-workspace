package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// stubFetcher serves a fixed model (or a fixed error) so the Indexer can be
// exercised without network access.
type stubFetcher struct {
	model string
	err   error
	calls int
}

func (f *stubFetcher) FetchModel(context.Context, string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.model, nil
}

// decodeLines parses a JSON Lines body into records, failing the test if any line
// is not a self-contained JSON object. That property is what lets a consumer grep
// the index and read it incrementally.
func decodeLines(t *testing.T, body string) []CandidateRecord {
	t.Helper()
	var out []CandidateRecord
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var r CandidateRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line is not a JSON object: %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func recordByPath(records []CandidateRecord, path string) (CandidateRecord, bool) {
	for _, r := range records {
		if r.Path == path {
			return r, true
		}
	}
	return CandidateRecord{}, false
}

func TestCandidateRecordsFiltersAndMarkings(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	docs, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	records, err := candidateRecords(repo, "Certificate", docs)
	if err != nil {
		t.Fatal(err)
	}

	paths := make([]string, len(records))
	for i, r := range records {
		paths[i] = r.Path
	}

	// The generated "<name>Ref" companion is controller plumbing, not an API
	// field, so it must never appear as a candidate.
	for _, p := range paths {
		if strings.HasPrefix(p, "roleRef") {
			t.Errorf("roleRef companion leaked into the index: %v", paths)
		}
	}
	// A list of structs contributes its string leaves, not the object itself.
	if _, ok := recordByPath(records, "tags.key"); !ok {
		t.Errorf("tags.key missing from %v", paths)
	}
	if _, ok := recordByPath(records, "tags"); ok {
		t.Errorf("the tags array itself must not be a candidate: %v", paths)
	}

	// Every record carries its resource so a merged multi-resource stream stays
	// interpretable.
	for _, r := range records {
		if r.Resource != "Certificate" {
			t.Errorf("record %s has resource %q, want Certificate", r.Path, r.Resource)
		}
	}

	// generator.yaml is authoritative for is_reference, and the configured target
	// is surfaced so a reader does not have to open generator.yaml to see the
	// controller's own convention.
	roleARN, ok := recordByPath(records, "roleARN")
	if !ok {
		t.Fatalf("roleARN missing from %v", paths)
	}
	if !roleARN.IsReference {
		t.Error("roleARN: is_reference = false, want true")
	}
	if want := "iam Role -> Status.ACKResourceMetadata.ARN"; roleARN.ReferenceTarget != want {
		t.Errorf("roleARN reference_target = %q, want %q", roleARN.ReferenceTarget, want)
	}

	// Immutable and primary-key markings are surfaced as signal, not used to
	// exclude: a reference is frequently immutable, and a sub-resource's primary
	// key can itself point at its parent.
	if name, ok := recordByPath(records, "name"); !ok || !name.IsPrimaryKey {
		t.Errorf("name record = %+v (found=%v), want is_primary_key", name, ok)
	}
	if dn, ok := recordByPath(records, "domainName"); !ok || !dn.IsImmutable {
		t.Errorf("domainName record = %+v (found=%v), want is_immutable", dn, ok)
	}

	// An unconfigured field carries no target.
	if dn, _ := recordByPath(records, "domainName"); dn.ReferenceTarget != "" {
		t.Errorf("domainName reference_target = %q, want empty", dn.ReferenceTarget)
	}
}

func TestCandidateRecordsModelJoinOrigin(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	docs, err := buildDocIndex(testSmithyModel, repo, "Certificate")
	if err != nil {
		t.Fatal(err)
	}
	records, err := candidateRecords(repo, "Certificate", docs)
	if err != nil {
		t.Fatal(err)
	}

	// A field the structural walk reached is marked as a path join, so a reader
	// can tell a description that is right by construction from one recovered via
	// the member-name fallback.
	roleARN, _ := recordByPath(records, "roleARN")
	if roleARN.ModelJoin != joinOriginPath {
		t.Errorf("roleARN model_join = %q, want %q", roleARN.ModelJoin, joinOriginPath)
	}
	if !strings.Contains(roleARN.Pattern, "arn:aws:iam::") {
		t.Errorf("roleARN pattern = %q, want the IAM role ARN pattern", roleARN.Pattern)
	}
}

func TestCandidateRecordsWithoutModel(t *testing.T) {
	root := t.TempDir()
	repo := writeControllerRepo(t, root, "acm-controller")

	// A zero docIndex stands for "the model could not be fetched". The index must
	// still build — degraded, not absent — because a model outage should not stop
	// an audit, only lower its confidence on nested fields.
	records, err := candidateRecords(repo, "Certificate", docIndex{})
	if err != nil {
		t.Fatalf("candidateRecords without a model failed: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("no records built without a model")
	}
	for _, r := range records {
		if r.ModelJoin != "" {
			t.Errorf("%s: model_join = %q, want empty without a model", r.Path, r.ModelJoin)
		}
		if r.DescriptionSource == descriptionSourceModel {
			t.Errorf("%s: description_source = model without a model", r.Path)
		}
	}
	// CRD-sourced descriptions still come through.
	if name, _ := recordByPath(records, "name"); name.DescriptionSource != descriptionSourceCRD {
		t.Errorf("name description_source = %q, want crd", name.DescriptionSource)
	}
}

func TestSuppressedIdentifierFields(t *testing.T) {
	root := t.TempDir()
	generator := `ignore:
  field_paths:
    - CreateBrokerInput.DataReplicationPrimaryBrokerArn
    - CreateCertificateInput.ClientToken
    - PutThingInput.ThingName
    - CreateCertificateInput.Description
    - CreateEventDataStoreInput.KmsKeyId
resources:
  Certificate:
    fields: {}
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	got, err := suppressedIdentifierFields(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CreateBrokerInput.DataReplicationPrimaryBrokerArn",
		"CreateEventDataStoreInput.KmsKeyId",
		"PutThingInput.ThingName",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("suppressedIdentifierFields = %v, want %v", got, want)
	}
}

func TestLooksLikeIdentifier(t *testing.T) {
	cases := map[string]bool{
		"CreateBrokerInput.DataReplicationPrimaryBrokerArn": true,
		"CreateEventDataStoreInput.KmsKeyId":                true,
		// ACK's own initialism casing must match too.
		"Foo.KMSKeyID":  true,
		"Foo.SubnetIDs": true,
		"PutSubscriptionFilterInput.LogGroupName": true,
		"Foo.Identifier":                     true,
		"CreateCertificateInput.ClientToken": false,
		"CreateCertificateInput.Description": false,
		"Foo.Policy":                         false,
	}
	for path, want := range cases {
		if got := looksLikeIdentifier(path); got != want {
			t.Errorf("looksLikeIdentifier(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCleanDoc(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<p>The ARN of an IAM role.</p>", "The ARN of an IAM role."},
		// A tag boundary separates words that would otherwise run together.
		{"<p>First.</p><p>Second.</p>", "First. Second."},
		{"<p>Use <code>arn:aws:iam</code> here.</p>", "Use arn:aws:iam here."},
		{"<p>A &amp; B &lt;x&gt;</p>", "A & B <x>"},
		{"  plain\n  text  ", "plain text"},
		{"", ""},
	}
	for _, c := range cases {
		if got := cleanDoc(c.in); got != c.want {
			t.Errorf("cleanDoc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCandidatesStreamsJSONLines(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	summary, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   "Certificate",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A clean run returns no Results, so the entrypoint prints no summary block
	// and stdout stays a pure record stream.
	if len(summary.Results) != 0 {
		t.Errorf("clean run returned %d Results, want 0 so stdout is not polluted", len(summary.Results))
	}
	records := decodeLines(t, out.String())
	if len(records) == 0 {
		t.Fatal("no records emitted")
	}
	// Progress goes to the note writer, never to the record stream.
	if !strings.Contains(errOut.String(), "acm/Certificate") {
		t.Errorf("progress line missing from notes: %q", errOut.String())
	}
}

func TestCandidatesWritesPerResourceFiles(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	outDir := filepath.Join(t.TempDir(), "idx")

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	if _, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   All,
		OutDir:     outDir,
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(outDir, "acm", "Certificate.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected an index at %s: %v", path, err)
	}
	if len(decodeLines(t, string(body))) == 0 {
		t.Errorf("index at %s is empty", path)
	}
	// With --out-dir nothing goes to the record stream.
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty when writing to --out-dir", out.String())
	}
}

func TestCandidatesSkipsResourceWithoutCRD(t *testing.T) {
	root := t.TempDir()
	// The generator declares a resource the controller does not generate a CRD
	// for. That resource is not indexable, and the caller must be able to tell it
	// apart from a resource that indexed clean — reporting it as a success would
	// be a silent false clean on a resource nobody examined.
	generator := `resources:
  Certificate:
    fields: {}
  Ghost:
    fields: {}
`
	writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	summary, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   All,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A missing CRD is skipped, not failed: it must not make the run exit non-zero.
	if summary.HasFailures() {
		t.Errorf("a resource without a CRD must not be a failure: %+v", summary.Results)
	}
	if !strings.Contains(errOut.String(), "SKIP") || !strings.Contains(errOut.String(), "Ghost") {
		t.Errorf("notes should record the skipped resource, got %q", errOut.String())
	}
}

func TestCandidatesDegradesWhenModelUnavailable(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{err: context.DeadlineExceeded}, &out, &errOut)
	summary, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   "Certificate",
	})
	if err != nil {
		t.Fatalf("an unavailable model must degrade the index, not fail the run: %v", err)
	}
	if summary.HasFailures() {
		t.Errorf("unavailable model reported as a failure: %+v", summary.Results)
	}
	if len(decodeLines(t, out.String())) == 0 {
		t.Error("no records emitted without a model")
	}
	// The degradation must be stated, so an empty gap list is not mistaken for a
	// thorough one.
	if !strings.Contains(errOut.String(), "unavailable") {
		t.Errorf("notes should report the unavailable model, got %q", errOut.String())
	}
}

func TestCandidatesFetchesModelOncePerController(t *testing.T) {
	root := t.TempDir()
	// Two resources of one controller share a model. Decoding is a large JSON
	// unmarshal for the bigger services, so it must not be repeated per resource.
	generator := `resources:
  Certificate:
    fields: {}
  Other:
    fields: {}
`
	repo := writeControllerRepoWithGenerator(t, root, "acm-controller", generator)
	crd := strings.Replace(testCRDYAML, "kind: Certificate", "kind: Other", 1)
	if err := os.WriteFile(filepath.Join(repo, "helm", "crds", "acm_other.yaml"), []byte(crd), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &stubFetcher{model: testSmithyModel}
	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(f, &out, &errOut)
	if _, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   All,
	}); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Errorf("FetchModel called %d times, want 1 per controller", f.calls)
	}
}

func TestCandidatesReportsSuppressedFields(t *testing.T) {
	root := t.TempDir()
	generator := `ignore:
  field_paths:
    - CreateCertificateInput.KmsKeyId
resources:
  Certificate:
    fields: {}
`
	writeControllerRepoWithGenerator(t, root, "acm-controller", generator)

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	if _, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   "Certificate",
	}); err != nil {
		t.Fatal(err)
	}

	// A suppressed field can never reach the index, and a suppression can hide a
	// reference, so it has to be reported or an empty gap list reads as clean.
	notes := errOut.String()
	if !strings.Contains(notes, "CreateCertificateInput.KmsKeyId") {
		t.Errorf("suppressed identifier field not reported: %q", notes)
	}
	if !strings.Contains(notes, "suppressed") {
		t.Errorf("suppression note missing its explanation: %q", notes)
	}
}

func TestCandidatesDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	outDir := filepath.Join(t.TempDir(), "idx")

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	a := testApp(root)
	a.DryRun = true
	summary, err := ix.Candidates(context.Background(), a, CandidatesOptions{
		Controller: "acm",
		Resource:   All,
		OutDir:     outDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.HasFailures() {
		t.Errorf("dry run reported failures: %+v", summary.Results)
	}
	if out.Len() != 0 {
		t.Errorf("dry run wrote records: %q", out.String())
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Errorf("dry run created %s", outDir)
	}
}

func TestCandidatesUnknownController(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	if _, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "nope",
		Resource:   All,
	}); err == nil {
		t.Error("expected an error for an unknown controller")
	}
}

func TestMarshalCandidateLinesOneObjectPerLine(t *testing.T) {
	body, err := marshalCandidateLines([]CandidateRecord{
		{Resource: "Certificate", Path: "a", Type: "string"},
		{Resource: "Certificate", Path: "b", Type: "array"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), body)
	}
	for _, l := range lines {
		var r CandidateRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Errorf("line %q is not a JSON object: %v", l, err)
		}
	}
}

// Guard the Summary contract the command depends on: only failures are returned,
// so main.go renders nothing on a clean run but a real failure still reaches the
// exit code.
func TestCandidatesSummaryCarriesOnlyFailures(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	// A read-only out dir makes the write fail without any other error.
	outDir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o700) })

	var out, errOut bytes.Buffer
	ix := NewIndexerWithFetcher(&stubFetcher{model: testSmithyModel}, &out, &errOut)
	summary, err := ix.Candidates(context.Background(), testApp(root), CandidatesOptions{
		Controller: "acm",
		Resource:   "Certificate",
		OutDir:     filepath.Join(outDir, "sub"),
	})
	if err != nil {
		t.Fatalf("a write failure belongs in the Summary, not the error: %v", err)
	}
	if !summary.HasFailures() {
		t.Fatalf("write failure not reported: %+v", summary.Results)
	}
	for _, r := range summary.Results {
		if r.Outcome != workspace.OutcomeFailed {
			t.Errorf("Summary carries a non-failure Result: %+v", r)
		}
	}
}
