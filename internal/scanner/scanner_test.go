package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// smartClient reports canned findings immediately, without calling any
// investigation tool. It is stateless and safe under the scanner's concurrent
// job execution.
type smartClient struct {
	findings json.RawMessage
}

func (c *smartClient) Converse(_ context.Context, _ ConverseRequest) (ConverseResponse, error) {
	// Report immediately: the scanner tests exercise job fan-out, rendering, and
	// aggregation, not tool execution (which is covered by the agent and tool
	// tests and would otherwise require bash/network here).
	return assistantToolUse("r1", reportToolName, c.findings), nil
}

func testApp(root string) app.App {
	return app.App{Config: config.Config{WorkspaceRoot: root, Concurrency: 4}}
}

func TestScanEndToEndTable(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	// One correctly-marked field and one unmarked field → the resource FAILs.
	findings := json.RawMessage(`{"fields":[
		{"terraform_field":"delivery_policy","ack_field_path":"deliveryPolicy","document_kind":"json","marked_in_generator":true,"current_marking":"is_document","confidence":1,"reasoning":"x"},
		{"terraform_field":"filter_policy","ack_field_path":"filterPolicy","document_kind":"json","marked_in_generator":false,"current_marking":"none","confidence":1,"reasoning":"x"}
	],"summary":"one unmarked"}`)
	var buf bytes.Buffer
	s := NewWithWriter(&smartClient{findings: findings}, &buf)

	if _, err := s.Scan(context.Background(), testApp(root), Options{
		Controller: "acm", Resource: All, Issue: "1", Concurrency: 4,
	}); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"acm/Certificate", "json-document-fields", "FAIL",
		"incorrectly marked: filterPolicy", "correctly marked: deliveryPolicy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestScanEndToEndJSON(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	// All mapped fields correctly marked → PASS.
	findings := json.RawMessage(`{"fields":[
		{"terraform_field":"delivery_policy","ack_field_path":"deliveryPolicy","document_kind":"json","marked_in_generator":true,"current_marking":"is_document","confidence":1,"reasoning":"x"}
	],"summary":"all marked"}`)
	var buf bytes.Buffer
	s := NewWithWriter(&smartClient{findings: findings}, &buf)

	if _, err := s.Scan(context.Background(), testApp(root), Options{
		Controller: "acm", Resource: "Certificate", Issue: "1", JSON: true, Concurrency: 2,
	}); err != nil {
		t.Fatalf("Scan error: %v", err)
	}

	var got []Finding
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("JSON output did not parse: %v\n%s", err, buf.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Controller != "acm" || f.Resource != "Certificate" || f.Issue != 1 || f.Status != statusOK {
		t.Errorf("unexpected finding: %+v", f)
	}
	if f.Verdict != string(VerdictPass) {
		t.Errorf("verdict = %q, want pass", f.Verdict)
	}
	if string(f.Findings) == "" {
		t.Error("finding is missing structured findings")
	}
}

func TestScanFailedConversationIsReportedNotFatal(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	var buf bytes.Buffer
	// A client that always errors makes every conversation fail; the batch must
	// still complete and report the failure per job.
	s := NewWithWriter(&scriptedClient{err: errors.New("no credentials")}, &buf)
	if _, err := s.Scan(context.Background(), testApp(root), Options{
		Controller: "acm", Resource: "Certificate", Issue: "1", JSON: true, Concurrency: 1,
	}); err != nil {
		t.Fatalf("Scan should not return a fatal error for a per-job failure: %v", err)
	}
	var got []Finding
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != statusFailed || got[0].Error == "" {
		t.Errorf("expected one failed finding with an error, got %+v", got)
	}
}

func TestScanNoControllers(t *testing.T) {
	var buf bytes.Buffer
	s := NewWithWriter(&smartClient{}, &buf)
	if _, err := s.Scan(context.Background(), testApp(t.TempDir()), Options{
		Controller: All, Resource: All, Issue: "1",
	}); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if !strings.Contains(buf.String(), "No controllers found") {
		t.Errorf("expected friendly no-controllers message, got: %s", buf.String())
	}
}

func TestScanUnknownController(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	s := NewWithWriter(&smartClient{}, &bytes.Buffer{})
	if _, err := s.Scan(context.Background(), testApp(root), Options{
		Controller: "s3", Resource: All, Issue: "1",
	}); err == nil {
		t.Error("expected error for unknown controller")
	}
}

func TestResolveIssues(t *testing.T) {
	s := New(&smartClient{})

	all, err := s.resolveIssues(All)
	if err != nil || len(all) != 1 {
		t.Fatalf("resolveIssues(all) = %v, %v; want 1 issue", all, err)
	}
	one, err := s.resolveIssues("1")
	if err != nil || len(one) != 1 || one[0].Number != 1 {
		t.Fatalf("resolveIssues(1) = %v, %v", one, err)
	}

	var usage *UsageError
	if _, err := s.resolveIssues("99"); !errors.As(err, &usage) {
		t.Errorf("resolveIssues(99) err = %v, want UsageError", err)
	}
	if _, err := s.resolveIssues("abc"); !errors.As(err, &usage) {
		t.Errorf("resolveIssues(abc) err = %v, want UsageError", err)
	}
}

func TestBuildJobsFanOut(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")
	writeControllerRepo(t, root, "s3-controller")

	controllers, err := discoverControllers(root)
	if err != nil {
		t.Fatal(err)
	}
	issues := NewRegistry().All()

	// 2 controllers x 1 resource each (Certificate) x 1 issue = 2 jobs.
	jobs, err := buildJobs(controllers, All, issues)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
}
