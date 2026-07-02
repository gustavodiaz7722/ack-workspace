package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// scriptedClient returns a pre-recorded response (or error) per call, in order.
// It is used to drive the agent loop deterministically in a single conversation.
type scriptedClient struct {
	responses []ConverseResponse
	err       error
	calls     int
	lastReq   ConverseRequest
}

func (c *scriptedClient) Converse(_ context.Context, req ConverseRequest) (ConverseResponse, error) {
	c.lastReq = req
	if c.err != nil {
		return ConverseResponse{}, c.err
	}
	r := c.responses[c.calls]
	c.calls++
	return r, nil
}

// assistantToolUse builds an assistant turn that calls one tool.
func assistantToolUse(id, name string, input json.RawMessage) ConverseResponse {
	return ConverseResponse{
		StopReason: StopToolUse,
		Message: Message{Role: RoleAssistant, Blocks: []Block{
			{ToolUse: &ToolUse{ID: id, Name: name, Input: input}},
		}},
	}
}

func reportTool(name string) Tool {
	return Tool{Name: name, InputSchema: emptyObjectSchema, Run: nil}
}

func TestAgentReportsFindingsAfterToolUse(t *testing.T) {
	findings := json.RawMessage(`{"summary":"done"}`)
	var ran bool
	investigate := Tool{
		Name:        "investigate",
		InputSchema: emptyObjectSchema,
		Run: func(_ context.Context, target Target, _ json.RawMessage) (string, error) {
			ran = true
			if target.Resource != "Certificate" {
				t.Errorf("tool got resource %q, want Certificate", target.Resource)
			}
			return "evidence", nil
		},
	}
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("t1", "investigate", json.RawMessage(`{}`)),
		assistantToolUse("r1", reportToolName, findings),
	}}

	agent := NewAgent(client)
	tools := []Tool{investigate, reportTool(reportToolName)}
	got, err := agent.Run(context.Background(), Target{Resource: "Certificate"}, "sys", "go", tools, reportToolName)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !ran {
		t.Error("investigation tool was never executed")
	}
	if string(got) != string(findings) {
		t.Errorf("findings = %s, want %s", got, findings)
	}
	if client.calls != 2 {
		t.Errorf("model called %d times, want 2", client.calls)
	}
}

func TestAgentReportsOnFirstTurn(t *testing.T) {
	findings := json.RawMessage(`{"summary":"immediate"}`)
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("r1", reportToolName, findings),
	}}
	got, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{reportTool(reportToolName)}, reportToolName)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(got) != string(findings) {
		t.Errorf("findings = %s, want %s", got, findings)
	}
}

func TestAgentNoFindingsOnPlainTurn(t *testing.T) {
	client := &scriptedClient{responses: []ConverseResponse{
		{StopReason: StopEndTurn, Message: Message{Role: RoleAssistant, Blocks: []Block{{Text: "I think..."}}}},
	}}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{reportTool(reportToolName)}, reportToolName)
	if !errors.Is(err, ErrNoFindings) {
		t.Fatalf("err = %v, want ErrNoFindings", err)
	}
}

func TestAgentMaxTurns(t *testing.T) {
	// A client that always asks for a tool never converges on a report.
	client := &loopingClient{}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{{Name: "noop", InputSchema: emptyObjectSchema, Run: func(context.Context, Target, json.RawMessage) (string, error) { return "x", nil }}, reportTool(reportToolName)},
		reportToolName)
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v, want ErrMaxTurns", err)
	}
	if client.calls != defaultMaxTurns {
		t.Errorf("model called %d times, want %d", client.calls, defaultMaxTurns)
	}
}

type loopingClient struct{ calls int }

func (c *loopingClient) Converse(_ context.Context, _ ConverseRequest) (ConverseResponse, error) {
	c.calls++
	return assistantToolUse("t", "noop", json.RawMessage(`{}`)), nil
}

func TestAgentPropagatesModelError(t *testing.T) {
	client := &scriptedClient{err: errors.New("boom")}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{reportTool(reportToolName)}, reportToolName)
	if err == nil {
		t.Fatal("expected error from model failure")
	}
}

func TestRunToolUnknownAndError(t *testing.T) {
	byName := map[string]Tool{
		"boom": {Name: "boom", Run: func(context.Context, Target, json.RawMessage) (string, error) {
			return "", errors.New("kaboom")
		}},
	}

	unknown := runTool(context.Background(), byName, ToolUse{ID: "1", Name: "missing"}, Target{})
	if unknown.ToolResult == nil || !unknown.ToolResult.IsError {
		t.Fatal("unknown tool should yield an error tool-result")
	}

	failed := runTool(context.Background(), byName, ToolUse{ID: "2", Name: "boom"}, Target{})
	if failed.ToolResult == nil || !failed.ToolResult.IsError {
		t.Fatal("failing tool should yield an error tool-result")
	}
}
