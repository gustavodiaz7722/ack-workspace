package scanner

import (
	"context"
	"encoding/json"
)

// Target is the atomic subject of one agent conversation: a single resource of
// a single controller. Every scan job investigates exactly one issue against
// exactly one Target, so a Target uniquely scopes the artifacts a tool reads.
type Target struct {
	// Controller is the service controller alias, for example "acm" (derived
	// from the repository directory name with any "-controller" suffix removed).
	Controller string
	// Resource is the resource Kind within the controller, for example
	// "Certificate", matching a key under generator.yaml's `resources:` map.
	Resource string
	// RepoPath is the absolute path to the controller repository checkout under
	// the workspace root. Tools resolve generator.yaml, the CRDs, and generated
	// code relative to it.
	RepoPath string
}

// ToolFunc executes a tool against a Target using the JSON arguments the model
// supplied, and returns the (pre-filtered) text to relay back to the model.
//
// A returned error is surfaced to the model as an error tool-result so it can
// adapt; it does not abort the conversation. Implementations must therefore
// return scoped, compact output: returning whole manifests or documents risks
// overflowing the model's context window in a single turn.
type ToolFunc func(ctx context.Context, target Target, input json.RawMessage) (string, error)

// Source is one grep-able document an issue exposes to the agent. It is the
// sandbox boundary: the grep tool can only read the sources an issue declares,
// so the model cannot wander into unrelated files. Load resolves the source to
// its text content for a given Target; ref is an optional, source-specific
// selector (for example a Terraform resource slug) that most sources ignore.
type Source struct {
	Name        string
	Description string
	Load        func(ctx context.Context, target Target, ref string) (string, error)
}

// Tool is one capability exposed to the agent: a name, a description telling the
// model when to use it, the JSON Schema of its arguments, and the function that
// runs it.
//
// The report tool is modeled as a Tool with a nil Run: the agent intercepts a
// call to it to capture the final structured findings instead of executing a
// function (see Agent.Run).
type Tool struct {
	// Name is the identifier the model calls and that a ToolUse references.
	Name string
	// Description tells the model what the tool does and when to call it.
	Description string
	// InputSchema is the JSON Schema of the tool's argument object.
	InputSchema json.RawMessage
	// Run executes the tool. It is nil for the report tool, which the agent
	// handles specially.
	Run ToolFunc
}
