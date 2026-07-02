package scanner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// DefaultModelID is the Bedrock model the scanner uses when none is configured.
// It is the US cross-region inference profile for Claude Sonnet 4.5, which is
// optimized for agentic tool use — a good fit for this scanner. Override it with
// the scan command's --model flag if your account enables a different model or
// region (for example a "global." or "eu." inference profile, or a newer model).
const DefaultModelID = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

// defaultMaxTokens bounds the model's output per turn. Findings are compact, so
// a few thousand tokens is ample headroom.
const defaultMaxTokens = 4096

// converseAPI is the slice of the Bedrock Runtime client the adapter uses. It is
// an interface so the adapter's translation can be unit-tested with a fake, and
// so the concrete client is constructed only in NewBedrockClient.
type converseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// BedrockClient is the production ModelClient: it translates the scanner's
// provider-neutral request/response to and from the Amazon Bedrock Converse API.
// It is the only place in the package that depends on the AWS SDK.
type BedrockClient struct {
	api         converseAPI
	modelID     string
	maxTokens   int32
	temperature float32
}

// NewBedrockClient builds a BedrockClient from the default AWS configuration
// chain (environment, shared config, and so on). An empty region defers to the
// resolved default; an empty modelID uses DefaultModelID. Constructing the
// client performs no network request; credentials are exercised only when
// Converse runs.
func NewBedrockClient(ctx context.Context, region, modelID string) (*BedrockClient, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	if modelID == "" {
		modelID = DefaultModelID
	}
	return &BedrockClient{
		api:         bedrockruntime.NewFromConfig(cfg),
		modelID:     modelID,
		maxTokens:   defaultMaxTokens,
		temperature: 0,
	}, nil
}

// Converse implements ModelClient by issuing one Bedrock Converse call. It maps
// the neutral messages/tools onto the Converse wire types, sends the request,
// and maps the reply back to a neutral ConverseResponse.
func (c *BedrockClient) Converse(ctx context.Context, req ConverseRequest) (ConverseResponse, error) {
	in := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(c.modelID),
		Messages: toBedrockMessages(req.Messages),
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens:   aws.Int32(c.maxTokens),
			Temperature: aws.Float32(c.temperature),
		},
	}
	if req.System != "" {
		in.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: req.System},
		}
	}
	if len(req.Tools) > 0 {
		cfg, err := toToolConfiguration(req.Tools)
		if err != nil {
			return ConverseResponse{}, err
		}
		in.ToolConfig = cfg
	}

	out, err := c.api.Converse(ctx, in)
	if err != nil {
		return ConverseResponse{}, fmt.Errorf("bedrock converse: %w", err)
	}

	msg, err := fromBedrockOutput(out.Output)
	if err != nil {
		return ConverseResponse{}, err
	}
	return ConverseResponse{Message: msg, StopReason: StopReason(out.StopReason)}, nil
}

// toBedrockMessages maps neutral messages onto Converse messages.
func toBedrockMessages(msgs []Message) []types.Message {
	out := make([]types.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, types.Message{
			Role:    toConversationRole(m.Role),
			Content: toContentBlocks(m.Blocks),
		})
	}
	return out
}

// toConversationRole maps a neutral Role to the Converse role enum.
func toConversationRole(r Role) types.ConversationRole {
	if r == RoleAssistant {
		return types.ConversationRoleAssistant
	}
	return types.ConversationRoleUser
}

// toContentBlocks maps neutral content blocks onto Converse content blocks.
func toContentBlocks(blocks []Block) []types.ContentBlock {
	out := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.ToolUse != nil:
			out = append(out, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
				ToolUseId: aws.String(b.ToolUse.ID),
				Name:      aws.String(b.ToolUse.Name),
				Input:     rawToDocument(b.ToolUse.Input),
			}})
		case b.ToolResult != nil:
			status := types.ToolResultStatusSuccess
			if b.ToolResult.IsError {
				status = types.ToolResultStatusError
			}
			out = append(out, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(b.ToolResult.ToolUseID),
				Status:    status,
				Content: []types.ToolResultContentBlock{
					&types.ToolResultContentBlockMemberText{Value: b.ToolResult.Text},
				},
			}})
		default:
			out = append(out, &types.ContentBlockMemberText{Value: b.Text})
		}
	}
	return out
}

// toToolConfiguration maps neutral tool specs onto a Converse ToolConfiguration.
func toToolConfiguration(specs []ToolSpec) (*types.ToolConfiguration, error) {
	tools := make([]types.Tool, 0, len(specs))
	for _, s := range specs {
		tools = append(tools, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
			Name:        aws.String(s.Name),
			Description: aws.String(s.Description),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: rawToDocument(s.InputSchema)},
		}})
	}
	return &types.ToolConfiguration{Tools: tools}, nil
}

// fromBedrockOutput extracts the assistant message from a Converse output union.
func fromBedrockOutput(out types.ConverseOutput) (Message, error) {
	msg, ok := out.(*types.ConverseOutputMemberMessage)
	if !ok {
		return Message{}, fmt.Errorf("unexpected converse output of type %T", out)
	}
	return fromBedrockMessage(msg.Value), nil
}

// fromBedrockMessage maps a Converse message back to a neutral Message. Only the
// content the agent loop reacts to (text and tool-use blocks) is translated.
func fromBedrockMessage(m types.Message) Message {
	blocks := make([]Block, 0, len(m.Content))
	for _, cb := range m.Content {
		switch v := cb.(type) {
		case *types.ContentBlockMemberText:
			blocks = append(blocks, Block{Text: v.Value})
		case *types.ContentBlockMemberToolUse:
			blocks = append(blocks, Block{ToolUse: &ToolUse{
				ID:    aws.ToString(v.Value.ToolUseId),
				Name:  aws.ToString(v.Value.Name),
				Input: documentToRaw(v.Value.Input),
			}})
		}
	}
	return Message{Role: fromConversationRole(m.Role), Blocks: blocks}
}

// fromConversationRole maps the Converse role enum back to a neutral Role.
func fromConversationRole(r types.ConversationRole) Role {
	if r == types.ConversationRoleAssistant {
		return RoleAssistant
	}
	return RoleUser
}

// rawToDocument wraps a JSON payload as a Smithy document for the Converse wire
// types. Empty input becomes an empty object; malformed JSON also degrades to an
// empty object rather than panicking, since tool inputs and schemas are produced
// by the program and validated elsewhere.
func rawToDocument(raw json.RawMessage) document.Interface {
	var v any = map[string]any{}
	if len(raw) > 0 {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			v = parsed
		}
	}
	return document.NewLazyDocument(v)
}

// documentToRaw serializes a Smithy document (a tool-use input) back to JSON.
func documentToRaw(d document.Interface) json.RawMessage {
	if d == nil {
		return nil
	}
	b, err := d.MarshalSmithyDocument()
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}
