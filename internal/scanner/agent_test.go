package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// requiredSchema mirrors the shape of a real issue's output schema: an object
// with properties the model must supply.
var requiredSchema = json.RawMessage(
	`{"type":"object","properties":{"fields":{"type":"array"},"summary":{"type":"string"}},` +
		`"required":["fields","summary"],"additionalProperties":false}`)

func requiredReportTool(name string) Tool {
	return Tool{Name: name, InputSchema: requiredSchema, Run: nil}
}

func TestAgentRejectsIncompleteReportThenAcceptsResubmission(t *testing.T) {
	// The failure seen in practice: the model calls the report tool with a bare
	// object. It must be refused rather than accepted as "nothing found", and the
	// model given a chance to resubmit.
	valid := json.RawMessage(`{"fields":[],"summary":"nothing found"}`)
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("r1", reportToolName, json.RawMessage(`{}`)),
		assistantToolUse("r2", reportToolName, valid),
	}}

	got, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(got) != string(valid) {
		t.Errorf("findings = %s, want %s", got, valid)
	}

	// The rejection must reach the model as an error tool-result naming what was
	// missing, otherwise it has nothing to correct.
	var found bool
	for _, m := range client.lastReq.Messages {
		for _, b := range m.Blocks {
			if b.ToolResult != nil && b.ToolResult.IsError &&
				strings.Contains(b.ToolResult.Text, "missing required properties") {
				found = true
				if b.ToolResult.ToolUseID != "r1" {
					t.Errorf("rejection echoed tool use %q, want r1", b.ToolResult.ToolUseID)
				}
			}
		}
	}
	if !found {
		t.Error("the rejected report was not relayed back to the model")
	}
}

func TestAgentWellFormedEmptyReportIsAccepted(t *testing.T) {
	// A report with an empty findings list is legitimate: the issue was assessed
	// and nothing was found. Only a report missing required properties is refused.
	valid := json.RawMessage(`{"fields":[],"summary":"no candidates"}`)
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("r1", reportToolName, valid),
	}}
	got, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(got) != string(valid) {
		t.Errorf("findings = %s, want %s", got, valid)
	}
}

func TestAgentInvalidFindingsWhenModelNeverComplies(t *testing.T) {
	// A model that keeps submitting an incomplete report must fail with the
	// specific reason, not the generic turn-ceiling error.
	client := &alwaysEmptyReportClient{}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if !errors.Is(err, ErrInvalidFindings) {
		t.Fatalf("err = %v, want ErrInvalidFindings", err)
	}
	if !strings.Contains(err.Error(), "fields") {
		t.Errorf("err = %v, want it to name the missing properties", err)
	}
}

type alwaysEmptyReportClient struct{ calls int }

func (c *alwaysEmptyReportClient) Converse(_ context.Context, _ ConverseRequest) (ConverseResponse, error) {
	c.calls++
	return assistantToolUse("r", reportToolName, json.RawMessage(`{}`)), nil
}

func TestAgentInvalidFindingsWhenModelGivesUpInProse(t *testing.T) {
	// An incomplete report followed by a prose turn must still surface the
	// rejection, which is more actionable than "ended without reporting".
	client := &scriptedClient{responses: []ConverseResponse{
		assistantToolUse("r1", reportToolName, json.RawMessage(`{}`)),
		{StopReason: StopEndTurn, Message: Message{Role: RoleAssistant, Blocks: []Block{{Text: "sorry"}}}},
	}}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if !errors.Is(err, ErrInvalidFindings) {
		t.Fatalf("err = %v, want ErrInvalidFindings", err)
	}
}

func TestValidateReport(t *testing.T) {
	cases := []struct {
		name    string
		schema  json.RawMessage
		input   json.RawMessage
		wantErr string
	}{
		{"complete", requiredSchema, json.RawMessage(`{"fields":[],"summary":"s"}`), ""},
		{"empty object", requiredSchema, json.RawMessage(`{}`), "missing required properties: fields, summary"},
		{"partial", requiredSchema, json.RawMessage(`{"fields":[]}`), "missing required properties: summary"},
		{"null value", requiredSchema, json.RawMessage(`{"fields":null,"summary":"s"}`), "missing required properties: fields"},
		{"not an object", requiredSchema, json.RawMessage(`"just text"`), "not a JSON object"},
		{"empty input", requiredSchema, json.RawMessage(``), "not a JSON object"},
		{"no required in schema", emptyObjectSchema, json.RawMessage(`{}`), ""},
		{"absent schema", nil, json.RawMessage(`{}`), ""},
		{"unparseable schema", json.RawMessage(`not json`), json.RawMessage(`{}`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReport(tc.schema, tc.input)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("err = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestAgentTruncatedReportIsDiagnosedAsTruncation(t *testing.T) {
	// Observed against sagemaker/Domain: the model begins a real report, is cut
	// off at the output token limit, and the partial tool-use arrives with an
	// empty input. That must be reported as truncation, and the guidance sent back
	// must ask for a shorter report rather than for the properties it "forgot" —
	// otherwise the model retries identically and is truncated identically.
	truncated := ConverseResponse{
		StopReason: StopMaxTokens,
		Message: Message{Role: RoleAssistant, Blocks: []Block{
			{Text: "Let me now compile my findings:"},
			{ToolUse: &ToolUse{ID: "r1", Name: reportToolName, Input: json.RawMessage(`{}`)}},
		}},
	}
	valid := json.RawMessage(`{"fields":[],"summary":"short"}`)
	client := &scriptedClient{responses: []ConverseResponse{
		truncated,
		assistantToolUse("r2", reportToolName, valid),
	}}

	got, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(got) != string(valid) {
		t.Errorf("findings = %s, want %s", got, valid)
	}

	var guidance string
	for _, m := range client.lastReq.Messages {
		for _, b := range m.Blocks {
			if b.ToolResult != nil && b.ToolResult.IsError {
				guidance = b.ToolResult.Text
			}
		}
	}
	if !strings.Contains(guidance, "output token limit") {
		t.Errorf("guidance = %q, want it to name the output token limit", guidance)
	}
	if !strings.Contains(guidance, "shorter") {
		t.Errorf("guidance = %q, want it to ask for a shorter report", guidance)
	}
}

func TestAgentTruncationSurvivesAsTerminalError(t *testing.T) {
	// A model truncated every time must fail as a truncation, not as a generic
	// schema violation: the actionable fix is a larger output token ceiling.
	client := &alwaysTruncatedClient{}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

type alwaysTruncatedClient struct{}

func (c *alwaysTruncatedClient) Converse(_ context.Context, _ ConverseRequest) (ConverseResponse, error) {
	return ConverseResponse{
		StopReason: StopMaxTokens,
		Message: Message{Role: RoleAssistant, Blocks: []Block{
			{ToolUse: &ToolUse{ID: "r", Name: reportToolName, Input: json.RawMessage(`{}`)}},
		}},
	}, nil
}

func TestAgentTruncatedProseTurnIsNotReportedAsDeclining(t *testing.T) {
	// A turn cut off before it reached any tool call must be distinguished from
	// the model choosing to answer in prose.
	client := &scriptedClient{responses: []ConverseResponse{
		{StopReason: StopMaxTokens, Message: Message{Role: RoleAssistant, Blocks: []Block{{Text: "I was still thinking"}}}},
	}}
	_, err := NewAgent(client).Run(context.Background(), Target{}, "sys", "go",
		[]Tool{requiredReportTool(reportToolName)}, reportToolName)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if errors.Is(err, ErrNoFindings) {
		t.Error("a truncated turn must not be reported as the model declining to report")
	}
}
