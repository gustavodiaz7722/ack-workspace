// Package scanner implements the `scan` feature: it drives a Bedrock,
// tool-using agent that investigates a single known issue against a single
// resource of a single ACK service controller, and reports structured findings.
//
// The package is layered so the model provider (Amazon Bedrock) is isolated
// behind the ModelClient interface. Everything except bedrock.go is free of any
// AWS SDK dependency, which keeps the agent loop, tool execution, issue
// registry, and orchestration unit-testable with an in-memory fake client.
//
// Design note (context management): each agent conversation investigates
// exactly one (controller, resource, issue) triple. Conversations are therefore
// short-lived and single-purpose, so the sliding-window/summarizing context
// management a framework like Strands provides is unnecessary. Instead, the
// tools return pre-filtered, scoped data (only the fields relevant to the
// issue) so a single turn never overflows the model's context window.
package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Role identifies the author of a conversation Message. Only the two roles the
// Converse tool-use loop needs are modeled: the assistant (the model) and the
// user (this program, which relays the initial prompt and every tool result).
type Role string

const (
	// RoleUser marks a message authored by this program: the initial prompt and
	// the tool-result turns fed back to the model.
	RoleUser Role = "user"
	// RoleAssistant marks a message authored by the model.
	RoleAssistant Role = "assistant"
)

// StopReason is why the model stopped generating a turn. Only the values the
// agent loop reacts to are modeled; any other reason is treated as a terminal
// (non-tool) turn.
type StopReason string

const (
	// StopToolUse indicates the model wants one or more tools invoked before it
	// can continue. The agent runs them and feeds the results back.
	StopToolUse StopReason = "tool_use"
	// StopEndTurn indicates the model produced a normal terminal turn.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTokens indicates the model hit its output token limit mid-turn.
	StopMaxTokens StopReason = "max_tokens"
)

// ToolUse is the model's request to invoke a named tool with JSON arguments.
type ToolUse struct {
	// ID correlates this request with the ToolResult the agent sends back; the
	// Converse contract requires the result to echo this identifier.
	ID string
	// Name is the tool the model wants to run.
	Name string
	// Input is the JSON argument object the model produced for the tool.
	Input json.RawMessage
}

// ToolResult is the outcome of running a tool, relayed back to the model as the
// content of a follow-up user turn.
type ToolResult struct {
	// ToolUseID echoes the ToolUse.ID this result answers.
	ToolUseID string
	// Text is the (pre-filtered) tool output presented to the model.
	Text string
	// IsError reports that the tool failed; the model is told so it can adapt
	// rather than treating the text as valid data.
	IsError bool
}

// Block is one content element of a Message. Exactly one of the pointer fields
// is set (or Text is non-empty for a plain text block); the others are nil.
// Modeling content as a tagged union mirrors the Converse content-block shape
// while staying free of any AWS SDK types.
type Block struct {
	// Text is a plain text content block when non-empty and the tool pointers
	// are nil.
	Text string
	// ToolUse is set when this block is the model asking to run a tool.
	ToolUse *ToolUse
	// ToolResult is set when this block carries a tool's output back to the model.
	ToolResult *ToolResult
}

// Message is a single conversation turn: a role and its ordered content blocks.
type Message struct {
	Role   Role
	Blocks []Block
}

// ToolSpec advertises a tool to the model: its name, a natural-language
// description of when to use it, and the JSON Schema of its input object.
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema document describing the tool's argument
	// object. It is passed through to the model provider verbatim.
	InputSchema json.RawMessage
}

// ConverseRequest is a single, provider-neutral request to the model: a system
// prompt, the running message history, and the tools the model may call.
type ConverseRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
}

// ConverseResponse is the model's reply: the assistant Message it produced and
// why it stopped.
type ConverseResponse struct {
	Message    Message
	StopReason StopReason
}

// ModelClient is the seam between the agent loop and the model provider. The
// production implementation (bedrock.go) calls the Amazon Bedrock Converse API;
// tests substitute an in-memory fake so the loop, tool dispatch, and reporting
// can be exercised without network access or AWS credentials.
type ModelClient interface {
	// Converse sends one request and returns the model's reply. Implementations
	// translate the neutral request/response to and from the provider's wire
	// types.
	Converse(ctx context.Context, req ConverseRequest) (ConverseResponse, error)
}

// defaultMaxTurns bounds a single conversation so a misbehaving model cannot
// loop forever calling tools without ever reporting findings. A thorough
// investigation can legitimately take many turns: a wide CRD may have dozens of
// candidate nested fields, and confirming each against external documentation
// (search, then read several ranges) costs multiple turns apiece. The ceiling
// is set high enough to let those complete while still guaranteeing
// termination.
const defaultMaxTurns = 40

// ErrNoFindings indicates the model ended the conversation with a normal turn
// without ever calling the report tool, so no structured findings were
// produced. The scanner records this as a failed job.
var ErrNoFindings = errors.New("agent ended the conversation without reporting findings")

// ErrMaxTurns indicates the conversation reached the turn ceiling without the
// model calling the report tool. It usually means the model kept requesting
// tools without converging on an answer.
var ErrMaxTurns = errors.New("agent exceeded the maximum number of conversation turns")

// Agent runs the Converse tool-use loop for one investigation. It is stateless
// between runs; a fresh conversation is started for every (controller,
// resource, issue) triple.
type Agent struct {
	client   ModelClient
	maxTurns int
	tr       tracer
}

// NewAgent returns an Agent backed by client using the default turn ceiling and
// no tracing. Callers enable a transcript by setting the agent's tracer.
func NewAgent(client ModelClient) *Agent {
	return &Agent{client: client, maxTurns: defaultMaxTurns, tr: nopTracer{}}
}

// Run drives one investigation to completion and returns the structured
// findings the model submitted through the report tool.
//
// The loop is the standard Converse tool-use cycle: send the system prompt and
// the running history, and whenever the model asks for tools, run them and feed
// the results back as the next user turn. The conversation terminates when the
// model calls the report tool named reportTool, whose raw JSON input (validated
// by the caller against the issue's output schema) is returned as the findings.
//
// tools must include the report tool spec; its Run may be nil because the agent
// intercepts a call to it rather than executing a function. A normal terminal
// turn without a report-tool call yields ErrNoFindings, and exhausting maxTurns
// yields ErrMaxTurns, so every non-reporting outcome is a typed error the
// scanner can record as a failed job.
func (a *Agent) Run(ctx context.Context, target Target, system, prompt string, tools []Tool, reportTool string) (json.RawMessage, error) {
	specs := make([]ToolSpec, 0, len(tools))
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		specs = append(specs, ToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		byName[t.Name] = t
	}

	messages := []Message{{Role: RoleUser, Blocks: []Block{{Text: prompt}}}}
	a.tr.start(system, prompt)

	for turn := 0; turn < a.maxTurns; turn++ {
		resp, err := a.client.Converse(ctx, ConverseRequest{System: system, Messages: messages, Tools: specs})
		if err != nil {
			a.tr.finish(nil, err)
			return nil, fmt.Errorf("model conversation failed: %w", err)
		}
		messages = append(messages, resp.Message)
		a.tr.modelResponse(turn+1, resp)

		toolUses := collectToolUses(resp.Message)
		if len(toolUses) == 0 {
			// A terminal turn with no tool calls: the model chose to answer in
			// prose instead of reporting through the report tool, so there are no
			// structured findings to return.
			a.tr.finish(nil, ErrNoFindings)
			return nil, ErrNoFindings
		}

		// If the model called the report tool, capture its structured input and
		// finish. It is checked before running any sibling tools because the
		// report tool signals the conversation is complete.
		if findings, ok := findReport(toolUses, reportTool); ok {
			a.tr.finish(findings, nil)
			return findings, nil
		}

		// Otherwise run each requested tool and relay the results back as a
		// single user turn, preserving request order.
		results := make([]Block, 0, len(toolUses))
		for _, use := range toolUses {
			block := runTool(ctx, byName, use, target)
			if block.ToolResult != nil {
				a.tr.toolResult(turn+1, use.Name, *block.ToolResult)
			}
			results = append(results, block)
		}
		messages = append(messages, Message{Role: RoleUser, Blocks: results})
	}

	a.tr.finish(nil, ErrMaxTurns)
	return nil, ErrMaxTurns
}

// collectToolUses returns the ToolUse requests in a message, in order.
func collectToolUses(m Message) []ToolUse {
	var uses []ToolUse
	for _, b := range m.Blocks {
		if b.ToolUse != nil {
			uses = append(uses, *b.ToolUse)
		}
	}
	return uses
}

// findReport returns the input of the first call to the report tool, if present.
func findReport(uses []ToolUse, reportTool string) (json.RawMessage, bool) {
	for _, use := range uses {
		if use.Name == reportTool {
			return use.Input, true
		}
	}
	return nil, false
}

// runTool executes one requested tool and packages its outcome as a ToolResult
// block. An unknown tool name or a tool error is reported back to the model as
// an error result (rather than aborting the conversation) so the model can
// adapt; the agent only fails the whole job on a model/transport error or when
// no findings are ever reported.
func runTool(ctx context.Context, byName map[string]Tool, use ToolUse, target Target) Block {
	tool, ok := byName[use.Name]
	if !ok || tool.Run == nil {
		return Block{ToolResult: &ToolResult{
			ToolUseID: use.ID,
			Text:      fmt.Sprintf("unknown tool %q", use.Name),
			IsError:   true,
		}}
	}
	out, err := tool.Run(ctx, target, use.Input)
	if err != nil {
		return Block{ToolResult: &ToolResult{
			ToolUseID: use.ID,
			Text:      fmt.Sprintf("tool %q failed: %v", use.Name, err),
			IsError:   true,
		}}
	}
	return Block{ToolResult: &ToolResult{ToolUseID: use.ID, Text: out}}
}
