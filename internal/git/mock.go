package git

import (
	"context"
	"fmt"
)

// Call records a single Runner.Run invocation: the directory it ran in and the
// exact git argument vector it was given.
type Call struct {
	// Dir is the working directory passed to Run.
	Dir string
	// Args is the git argument vector (the values after "git").
	Args []string
}

// String renders the call as the command line it represents, e.g.
// `git status --porcelain (in /work/repo)`.
func (c Call) String() string {
	return fmt.Sprintf("git %v (in %s)", c.Args, c.Dir)
}

// Response is a scripted result returned by MockRunner for one Run call.
type Response struct {
	// Output is the combined stdout+stderr the mock returns.
	Output string
	// Err is the error the mock returns.
	Err error
}

// MockRunner is a recording, scriptable Runner for tests. It captures every
// invocation in order and returns scripted output/errors, so component tests
// can assert the exact git argument vectors issued (Requirements 1.1, 8.3)
// without spawning a real git process.
//
// Response selection precedence per call:
//  1. ResponseFunc, when set, computes the response from the call arguments.
//  2. Otherwise the next entry from the Responses queue is consumed in order.
//  3. Otherwise a zero Response ("", nil) is returned.
//
// The zero value is a ready-to-use mock that records calls and returns empty
// output with no error.
type MockRunner struct {
	// Calls records every Run invocation in the order they occurred.
	Calls []Call

	// Responses is a FIFO queue of scripted responses. Each Run call consumes
	// the next entry unless ResponseFunc is set.
	Responses []Response

	// ResponseFunc, when non-nil, takes precedence over Responses and computes
	// a response from the call's dir and args. It is the ergonomic choice for
	// tests that vary output based on the git subcommand (e.g. returning porcelain
	// status for `status --porcelain` and a branch name for `symbolic-ref`).
	ResponseFunc func(dir string, args []string) (string, error)
}

// Ensure MockRunner satisfies the interface at compile time.
var _ Runner = (*MockRunner)(nil)

// Queue appends a scripted response to the Responses queue and returns the
// receiver for fluent setup, e.g. m.Queue("", nil).Queue("", errBoom).
func (m *MockRunner) Queue(output string, err error) *MockRunner {
	m.Responses = append(m.Responses, Response{Output: output, Err: err})
	return m
}

// Run records the invocation and returns the next scripted response. A defensive
// copy of args is stored so later mutation by the caller cannot corrupt the
// recorded history.
func (m *MockRunner) Run(_ context.Context, dir string, args ...string) (string, error) {
	argsCopy := append([]string(nil), args...)
	m.Calls = append(m.Calls, Call{Dir: dir, Args: argsCopy})

	if m.ResponseFunc != nil {
		return m.ResponseFunc(dir, argsCopy)
	}

	if len(m.Responses) > 0 {
		resp := m.Responses[0]
		m.Responses = m.Responses[1:]
		return resp.Output, resp.Err
	}

	return "", nil
}

// ArgVectors returns the recorded argument vectors in call order, which is
// convenient for asserting the exact sequence of git commands a component
// issued.
func (m *MockRunner) ArgVectors() [][]string {
	out := make([][]string, len(m.Calls))
	for i, c := range m.Calls {
		out[i] = c.Args
	}
	return out
}

// Last returns the most recent recorded call and true, or a zero Call and false
// when no calls have been recorded.
func (m *MockRunner) Last() (Call, bool) {
	if len(m.Calls) == 0 {
		return Call{}, false
	}
	return m.Calls[len(m.Calls)-1], true
}
