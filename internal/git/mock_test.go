package git

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMockRunner_RecordsCallsInOrder(t *testing.T) {
	ctx := context.Background()
	m := &MockRunner{}

	if _, err := m.Run(ctx, "/work/runtime", "clone", "https://example/repo.git", "."); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := m.Run(ctx, "/work/runtime", "remote", "add", "upstream", "https://example/up.git"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := [][]string{
		{"clone", "https://example/repo.git", "."},
		{"remote", "add", "upstream", "https://example/up.git"},
	}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}

	if m.Calls[0].Dir != "/work/runtime" {
		t.Errorf("expected dir to be recorded, got %q", m.Calls[0].Dir)
	}
}

func TestMockRunner_ScriptedResponsesQueueConsumedInOrder(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	m := &MockRunner{}
	m.Queue("first-output", nil).Queue("", boom)

	out, err := m.Run(ctx, "dir", "status", "--porcelain")
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}
	if out != "first-output" {
		t.Errorf("expected scripted output %q, got %q", "first-output", out)
	}

	out, err = m.Run(ctx, "dir", "fetch", "upstream")
	if !errors.Is(err, boom) {
		t.Fatalf("expected scripted error %v, got %v", boom, err)
	}
	if out != "" {
		t.Errorf("expected empty output for errored call, got %q", out)
	}

	// Queue exhausted: subsequent calls return the zero Response.
	out, err = m.Run(ctx, "dir", "rev-parse", "HEAD")
	if err != nil || out != "" {
		t.Errorf("expected zero response once queue is exhausted, got out=%q err=%v", out, err)
	}
}

func TestMockRunner_ResponseFuncTakesPrecedence(t *testing.T) {
	ctx := context.Background()
	m := &MockRunner{
		// Queue is present but must be ignored while ResponseFunc is set.
		Responses: []Response{{Output: "queued", Err: nil}},
		ResponseFunc: func(_ string, args []string) (string, error) {
			if len(args) > 0 && args[0] == "symbolic-ref" {
				return "main\n", nil
			}
			return "", nil
		},
	}

	out, err := m.Run(ctx, "dir", "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "main\n" {
		t.Errorf("expected ResponseFunc output %q, got %q", "main\n", out)
	}

	// The queue must remain untouched because ResponseFunc handled the call.
	if len(m.Responses) != 1 {
		t.Errorf("expected Responses queue untouched when ResponseFunc set, got %d entries", len(m.Responses))
	}
}

func TestMockRunner_RecordedArgsAreDefensivelyCopied(t *testing.T) {
	ctx := context.Background()
	m := &MockRunner{}

	args := []string{"checkout", "main"}
	if _, err := m.Run(ctx, "dir", args...); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Mutating the caller's slice after the call must not change history.
	args[1] = "mutated"

	if got := m.Calls[0].Args[1]; got != "main" {
		t.Errorf("recorded args should be a defensive copy; got %q after caller mutation", got)
	}
}

func TestMockRunner_Last(t *testing.T) {
	ctx := context.Background()
	m := &MockRunner{}

	if _, ok := m.Last(); ok {
		t.Fatal("expected Last to report no calls on an empty mock")
	}

	_, _ = m.Run(ctx, "dir", "fetch", "upstream")
	_, _ = m.Run(ctx, "dir", "merge", "--ff-only", "upstream/main")

	last, ok := m.Last()
	if !ok {
		t.Fatal("expected Last to report a recorded call")
	}
	want := []string{"merge", "--ff-only", "upstream/main"}
	if !reflect.DeepEqual(last.Args, want) {
		t.Errorf("expected last call %v, got %v", want, last.Args)
	}
}

func TestCall_String(t *testing.T) {
	c := Call{Dir: "/work/repo", Args: []string{"status", "--porcelain"}}
	got := c.String()
	if got == "" {
		t.Fatal("expected non-empty Call.String()")
	}
	// Sanity: it should mention the directory and the subcommand.
	for _, want := range []string{"/work/repo", "status"} {
		if !contains(got, want) {
			t.Errorf("Call.String() = %q; expected it to contain %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
