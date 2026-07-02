package scanner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// tracer receives agent-loop events so a conversation can be inspected while
// debugging. The Agent calls it at every step; the default implementation
// (nopTracer) does nothing, so tracing has no cost when disabled.
//
// A tracer is scoped to one conversation (one (controller, resource, issue)
// triple); the scanner constructs one per job with the target baked into its
// output label.
type tracer interface {
	// start records the system and initial user prompt that frame the
	// conversation.
	start(system, prompt string)
	// modelResponse records an assistant turn: its stop reason, any text, and the
	// tools it asked to call. turn is 1-based.
	modelResponse(turn int, resp ConverseResponse)
	// toolResult records the outcome of executing one tool the model requested.
	toolResult(turn int, name string, result ToolResult)
	// finish records how the conversation ended: the reported findings, or the
	// terminal error when no findings were produced.
	finish(findings json.RawMessage, err error)
}

// nopTracer is the default tracer: it discards every event.
type nopTracer struct{}

func (nopTracer) start(string, string)                {}
func (nopTracer) modelResponse(int, ConverseResponse) {}
func (nopTracer) toolResult(int, string, ToolResult)  {}
func (nopTracer) finish(json.RawMessage, error)       {}

// writerTracer writes a human-readable transcript of the conversation to a
// writer. The mutex is shared across all of a scan's tracers so lines from
// concurrent conversations are not interleaved mid-write; the scanner also
// serializes jobs while tracing, so transcripts stay grouped per conversation.
type writerTracer struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string // "controller/resource#issue"
}

func (t *writerTracer) start(system, prompt string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.headerf("conversation start")
	t.section("system prompt", system)
	t.section("user prompt", prompt)
}

func (t *writerTracer) modelResponse(turn int, resp ConverseResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.headerf("turn %d ▸ assistant (stop=%s)", turn, resp.StopReason)
	for _, b := range resp.Message.Blocks {
		switch {
		case b.ToolUse != nil:
			t.line("  tool_use %s(%s)", b.ToolUse.Name, compactJSON(b.ToolUse.Input))
		case strings.TrimSpace(b.Text) != "":
			t.section("text", b.Text)
		}
	}
}

func (t *writerTracer) toolResult(turn int, name string, result ToolResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	status := "ok"
	if result.IsError {
		status = "error"
	}
	t.headerf("turn %d ▸ tool %s → %s", turn, name, status)
	t.indent(result.Text)
}

func (t *writerTracer) finish(findings json.RawMessage, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.headerf("finished with error: %v", err)
		return
	}
	t.headerf("findings reported")
	t.indent(prettyJSON(findings))
}

// headerf writes a labeled header line, e.g. "[acm/Certificate#1] turn 1 ...".
func (t *writerTracer) headerf(format string, args ...any) {
	fmt.Fprintf(t.w, "[%s] %s\n", t.prefix, fmt.Sprintf(format, args...))
}

// line writes a single label-prefixed line.
func (t *writerTracer) line(format string, args ...any) {
	fmt.Fprintf(t.w, "[%s] %s\n", t.prefix, fmt.Sprintf(format, args...))
}

// section writes a titled block with its (possibly multi-line) body indented.
func (t *writerTracer) section(title, body string) {
	t.line("  %s:", title)
	t.indent(body)
}

// indent writes body with each line indented under the current event.
func (t *writerTracer) indent(body string) {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fmt.Fprintf(t.w, "    %s\n", sc.Text())
	}
}

// compactJSON renders raw JSON on a single line, falling back to the raw bytes
// when it is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// prettyJSON renders raw JSON indented, falling back to the raw bytes when it is
// not valid JSON.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}
