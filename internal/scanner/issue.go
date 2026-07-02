package scanner

import (
	"encoding/json"
	"sort"
)

// reportToolName is the name of the synthetic tool every issue exposes for
// submitting its final, structured findings. The agent intercepts a call to it
// to end the conversation (see Agent.Run); the tool's input schema is the
// issue's OutputSchema, so the model is guided to produce schema-shaped output.
const reportToolName = "report_findings"

// Issue is one entry in the static issue database: a known, well-understood
// class of ACK controller problem the agent knows how to investigate.
//
// An Issue is self-describing. It carries everything the agent needs to run:
// the tools it may call to gather evidence, a prompt builder that frames the
// investigation for a specific Target, and the JSON Schema the findings must
// conform to. New issues are added by defining an Issue value and registering
// it (see registry).
type Issue struct {
	// Number is the stable, human-facing issue identifier used on the command
	// line (`--issue 1`) and as the registry key.
	Number int
	// Name is a short slug describing the issue, shown in output.
	Name string
	// Description is a one-line summary of what the issue detects.
	Description string
	// Tools are the investigation tools the agent may call for this issue. The
	// report tool is added automatically and need not be listed here.
	Tools []Tool
	// OutputSchema is the JSON Schema the findings must conform to. It becomes
	// the input schema of the report tool, so the model is steered to emit
	// output in this shape.
	OutputSchema json.RawMessage
	// System returns the system prompt for an investigation of target. It
	// establishes the agent's role, the definition of the issue, and how to use
	// the tools and the report tool.
	System func(target Target) string
	// Prompt returns the initial user prompt that kicks off the investigation of
	// target.
	Prompt func(target Target) string
	// Evaluate inspects the structured findings the agent reported and decides
	// whether the target passes the issue. It returns VerdictPass or VerdictFail,
	// or an error if the findings cannot be interpreted (which the scanner
	// records as VerdictError).
	Evaluate func(findings json.RawMessage) (Verdict, error)
	// Summarize renders a reduced, human-readable summary of the findings for the
	// result output (for the document issue, the correctly- and incorrectly-marked
	// field paths).
	Summarize func(findings json.RawMessage) string
}

// Verdict is the pass/fail outcome of evaluating an issue's findings.
type Verdict string

const (
	// VerdictPass means the target satisfies the issue (nothing to fix).
	VerdictPass Verdict = "pass"
	// VerdictFail means the target has at least one problem the issue detects.
	VerdictFail Verdict = "fail"
	// VerdictError means the findings could not be evaluated (for example
	// malformed output). It is distinct from an agent/transport failure.
	VerdictError Verdict = "error"
)

// reportTool returns the synthetic report tool for the issue: a tool whose
// input schema is the issue's OutputSchema and whose Run is nil (the agent
// captures its input rather than executing it).
func (i Issue) reportTool() Tool {
	return Tool{
		Name: reportToolName,
		Description: "Submit your final, structured findings for this issue. Call this " +
			"exactly once, after you have gathered enough evidence, with an argument " +
			"object matching the required schema. Calling this tool ends the investigation.",
		InputSchema: i.OutputSchema,
	}
}

// agentTools returns the full tool set advertised to the model for this issue:
// the issue's investigation tools plus the synthetic report tool.
func (i Issue) agentTools() []Tool {
	return append(append([]Tool(nil), i.Tools...), i.reportTool())
}

// Registry is the static issue database: a lookup of known issues by number.
type Registry struct {
	issues map[int]Issue
}

// NewRegistry returns the default registry populated with every issue the
// scanner knows how to investigate, using an unauthenticated docs fetcher.
func NewRegistry() *Registry {
	return newRegistry(newHTTPDocsFetcher(""))
}

// NewRegistryWithToken is NewRegistry with a GitHub token used to authenticate
// documentation-listing requests, avoiding the low unauthenticated rate limit.
func NewRegistryWithToken(githubToken string) *Registry {
	return newRegistry(newHTTPDocsFetcher(githubToken))
}

// newRegistry builds the default registry with the given docs fetcher injected
// into the issues that consult external documentation.
func newRegistry(fetcher DocsFetcher) *Registry {
	r := &Registry{issues: map[int]Issue{}}
	for _, i := range defaultIssues(fetcher) {
		r.register(i)
	}
	return r
}

// register adds an issue to the registry keyed by its Number. A later
// registration for the same number replaces the earlier one.
func (r *Registry) register(i Issue) {
	r.issues[i.Number] = i
}

// Get returns the issue with the given number and whether it was found.
func (r *Registry) Get(number int) (Issue, bool) {
	i, ok := r.issues[number]
	return i, ok
}

// All returns every registered issue, ordered by Number, so "scan all issues"
// runs them in a stable, predictable sequence.
func (r *Registry) All() []Issue {
	out := make([]Issue, 0, len(r.issues))
	for _, i := range r.issues {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}
