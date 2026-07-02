package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// recordingTracer captures the agent's trace events for assertions.
type recordingTracer struct {
	started        bool
	responses      int
	toolCalls      []string
	finishFindings json.RawMessage
	finishErr      error
}

func (r *recordingTracer) start(string, string)                { r.started = true }
func (r *recordingTracer) modelResponse(int, ConverseResponse) { r.responses++ }
func (r *recordingTracer) toolResult(_ int, name string, _ ToolResult) {
	r.toolCalls = append(r.toolCalls, name)
}
func (r *recordingTracer) finish(f json.RawMessage, err error) {
	r.finishFindings = f
	r.finishErr = err
}

func TestAgentEmitsTraceEvents(t *testing.T) {
	findings := json.RawMessage(`{"summary":"done"}`)
	investigate := Tool{
		Name:        "investigate",
		InputSchema: emptyObjectSchema,
		Run:         func(context.Context, Target, json.RawMessage) (string, error) { return "evidence", nil },
	}
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("t1", "investigate", json.RawMessage(`{}`)),
		assistantToolUse("r1", reportToolName, findings),
	}}

	agent := NewAgent(client)
	rec := &recordingTracer{}
	agent.tr = rec

	if _, err := agent.Run(context.Background(), Target{Resource: "Certificate"}, "sys", "go",
		[]Tool{investigate, reportTool(reportToolName)}, reportToolName); err != nil {
		t.Fatal(err)
	}

	if !rec.started {
		t.Error("start was not traced")
	}
	if rec.responses != 2 {
		t.Errorf("model responses traced = %d, want 2", rec.responses)
	}
	if len(rec.toolCalls) != 1 || rec.toolCalls[0] != "investigate" {
		t.Errorf("tool calls traced = %v, want [investigate]", rec.toolCalls)
	}
	if rec.finishErr != nil || string(rec.finishFindings) != string(findings) {
		t.Errorf("finish traced findings=%s err=%v", rec.finishFindings, rec.finishErr)
	}
}

func TestScanTraceTranscript(t *testing.T) {
	root := t.TempDir()
	writeControllerRepo(t, root, "acm-controller")

	findings := json.RawMessage(`{"summary":"policyDocument is unmarked","fields":[]}`)
	var out, trace bytes.Buffer
	s := NewWithWriter(&smartClient{findings: findings}, &out)
	s.SetTraceWriter(&trace)

	if _, err := s.Scan(context.Background(), testApp(root), Options{
		Controller: "acm", Resource: "Certificate", Issue: "1", JSON: true, Concurrency: 4,
	}); err != nil {
		t.Fatal(err)
	}

	transcript := trace.String()
	for _, want := range []string{
		"[acm/Certificate#1]",
		"conversation start",
		"system prompt",
		"turn 1",
		"findings reported",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}

	// The transcript must not pollute the findings written to stdout.
	if strings.Contains(out.String(), "conversation start") {
		t.Error("trace output leaked into stdout")
	}
	// stdout must still be valid findings JSON.
	var got []Finding
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid findings JSON: %v", err)
	}
}
