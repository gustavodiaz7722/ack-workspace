package scanner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// fakeConverseAPI captures the request and returns a canned response, so the
// neutral<->Converse translation can be verified without AWS.
type fakeConverseAPI struct {
	in  *bedrockruntime.ConverseInput
	out *bedrockruntime.ConverseOutput
}

func (f *fakeConverseAPI) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.in = in
	return f.out, nil
}

func TestBedrockConverseTranslation(t *testing.T) {
	fake := &fakeConverseAPI{
		out: &bedrockruntime.ConverseOutput{
			StopReason: types.StopReasonToolUse,
			Output: &types.ConverseOutputMemberMessage{Value: types.Message{
				Role: types.ConversationRoleAssistant,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
						ToolUseId: aws.String("r1"),
						Name:      aws.String(reportToolName),
						Input:     document.NewLazyDocument(map[string]any{"summary": "ok"}),
					}},
				},
			}},
		},
	}
	c := &BedrockClient{api: fake, modelID: "model-x", maxTokens: 128}

	req := ConverseRequest{
		System: "system prompt",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{{Text: "hello"}}},
			{Role: RoleAssistant, Blocks: []Block{{ToolUse: &ToolUse{ID: "1", Name: "t", Input: json.RawMessage(`{"a":1}`)}}}},
			{Role: RoleUser, Blocks: []Block{{ToolResult: &ToolResult{ToolUseID: "1", Text: "evidence"}}}},
		},
		Tools: []ToolSpec{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}

	resp, err := c.Converse(context.Background(), req)
	if err != nil {
		t.Fatalf("Converse error: %v", err)
	}

	// --- request translation ---
	if aws.ToString(fake.in.ModelId) != "model-x" {
		t.Errorf("ModelId = %q, want model-x", aws.ToString(fake.in.ModelId))
	}
	if len(fake.in.System) != 1 {
		t.Fatalf("System blocks = %d, want 1", len(fake.in.System))
	}
	if sys, ok := fake.in.System[0].(*types.SystemContentBlockMemberText); !ok || sys.Value != "system prompt" {
		t.Errorf("system block not translated: %#v", fake.in.System[0])
	}
	if fake.in.ToolConfig == nil || len(fake.in.ToolConfig.Tools) != 1 {
		t.Fatalf("tool config not translated: %#v", fake.in.ToolConfig)
	}
	if len(fake.in.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(fake.in.Messages))
	}
	// The assistant tool-use input JSON must round-trip through the document.
	toolUse, ok := fake.in.Messages[1].Content[0].(*types.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("message 1 content is %T, want tool use", fake.in.Messages[1].Content[0])
	}
	if got := string(documentToRaw(toolUse.Value.Input)); got != `{"a":1}` {
		t.Errorf("tool-use input round-trip = %s, want {\"a\":1}", got)
	}

	// --- response translation ---
	if resp.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Message.Blocks) != 1 || resp.Message.Blocks[0].ToolUse == nil {
		t.Fatalf("response message not translated: %#v", resp.Message)
	}
	tu := resp.Message.Blocks[0].ToolUse
	if tu.Name != reportToolName {
		t.Errorf("tool name = %q, want %s", tu.Name, reportToolName)
	}
	var out struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(tu.Input, &out); err != nil || out.Summary != "ok" {
		t.Errorf("tool-use output input = %s (err %v)", tu.Input, err)
	}
}
