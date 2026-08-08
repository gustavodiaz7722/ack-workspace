package prereq

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// allFound stubs LookPath to report that every executable resolves.
func allFound(file string) (string, error) {
	return "/usr/bin/" + file, nil
}

// noneFound stubs LookPath to report that no executable resolves.
func noneFound(string) (string, error) {
	return "", exec.ErrNotFound
}

// only stubs LookPath to resolve just the named executables, so a test can
// script a partially-equipped machine.
func only(names ...string) func(string) (string, error) {
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	return func(file string) (string, error) {
		if present[file] {
			return "/usr/bin/" + file, nil
		}
		return "", exec.ErrNotFound
	}
}

// missing asserts the error is a *MissingError and returns its items.
func missing(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a missing-prerequisite error, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("error type = %T, want *MissingError: %v", err, err)
	}
	return me.Missing
}

func TestCheck_AllPresent(t *testing.T) {
	c := NewCheckerWithLookPath(allFound)
	cfg := config.Config{GitHubUser: "octocat", Token: "tok"}

	if err := c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg); err != nil {
		t.Fatalf("expected no error when all prerequisites present, got: %v", err)
	}
}

func TestCheck_NoNeedsAlwaysPasses(t *testing.T) {
	// Even with nothing on the PATH and an empty config, a Need with nothing
	// requested must pass (the config and candidates commands require nothing).
	c := NewCheckerWithLookPath(noneFound)
	if err := c.Check(Need{}, config.Config{}); err != nil {
		t.Fatalf("expected no error for empty Need, got: %v", err)
	}
}

func TestCheck_GitMissing(t *testing.T) {
	c := NewCheckerWithLookPath(noneFound)
	cfg := config.Config{GitHubUser: "octocat", Token: "tok"}

	items := missing(t, c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg))
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(items), items)
	}
	if !strings.Contains(items[0], "git") {
		t.Errorf("missing item should name git: %q", items[0])
	}
}

// TestCheck_EveryDeclaredToolIsChecked is the point of the whole design: a
// command that names a toolchain has every part of it verified up front, and one
// error names all of them. Before this, a missing helm or eksctl surfaced
// minutes into a deploy.
func TestCheck_EveryDeclaredToolIsChecked(t *testing.T) {
	c := NewCheckerWithLookPath(noneFound)

	need := Need{Tools: Git | Docker | AWS | Kubectl | Helm | Eksctl}
	items := missing(t, c.Check(need, config.Config{}))
	if len(items) != 6 {
		t.Fatalf("expected 6 missing tools, got %d: %v", len(items), items)
	}
	joined := strings.Join(items, "\n")
	for _, want := range []string{"git", "docker", "aws", "kubectl", "helm", "eksctl"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report must name %q; got:\n%s", want, joined)
		}
	}
}

// TestCheck_ReportsOnlyTheToolsActuallyAbsent covers the partially-equipped
// machine: the two tools that are present must not appear in the report.
func TestCheck_ReportsOnlyTheToolsActuallyAbsent(t *testing.T) {
	c := NewCheckerWithLookPath(only("git", "docker"))

	need := Need{Tools: Git | Docker | AWS | Kubectl | Helm | Eksctl}
	items := missing(t, c.Check(need, config.Config{}))
	if len(items) != 4 {
		t.Fatalf("expected 4 missing tools, got %d: %v", len(items), items)
	}
	joined := strings.Join(items, "\n")
	for _, absent := range []string{"aws", "kubectl", "helm", "eksctl"} {
		if !strings.Contains(joined, absent) {
			t.Errorf("report must name absent tool %q; got:\n%s", absent, joined)
		}
	}
	for _, present := range []string{"git:", "docker:"} {
		if strings.Contains(joined, present) {
			t.Errorf("report must not name present tool %q; got:\n%s", present, joined)
		}
	}
}

// TestCheck_UndeclaredToolsAreNotChecked pins that declaring one tool does not
// drag in the rest: status needs git and must not require helm.
func TestCheck_UndeclaredToolsAreNotChecked(t *testing.T) {
	c := NewCheckerWithLookPath(only("git"))

	if err := c.Check(Need{Tools: Git}, config.Config{}); err != nil {
		t.Fatalf("expected no error when the only declared tool is present, got: %v", err)
	}
}

// TestCheck_ToolsReportedInBitOrder pins the ordering so the report a user sees
// is stable between runs rather than dependent on iteration order.
func TestCheck_ToolsReportedInBitOrder(t *testing.T) {
	c := NewCheckerWithLookPath(noneFound)

	items := missing(t, c.Check(Need{Tools: Eksctl | Git | Helm | Make}, config.Config{}))
	want := []string{"git", "make", "helm", "eksctl"}
	if len(items) != len(want) {
		t.Fatalf("expected %d items, got %d: %v", len(want), len(items), items)
	}
	for i, w := range want {
		if !strings.HasPrefix(items[i], w+":") {
			t.Errorf("item %d = %q, want it to start with %q", i, items[i], w+":")
		}
	}
}

func TestCheck_TokenMissing(t *testing.T) {
	c := NewCheckerWithLookPath(allFound)
	cfg := config.Config{GitHubUser: "octocat", Token: ""}

	items := missing(t, c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg))
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(items), items)
	}
	if !strings.Contains(items[0], "token") {
		t.Errorf("missing item should instruct the user to supply a token: %q", items[0])
	}
}

func TestCheck_TokenWhitespaceOnlyIsMissing(t *testing.T) {
	c := NewCheckerWithLookPath(allFound)
	cfg := config.Config{GitHubUser: "octocat", Token: "   "}

	if err := c.Check(Need{Token: true}, cfg); err == nil {
		t.Fatal("expected error when token is whitespace-only, got nil")
	}
}

func TestCheck_IdentityMissing(t *testing.T) {
	c := NewCheckerWithLookPath(allFound)
	cfg := config.Config{GitHubUser: "", Token: "tok"}

	items := missing(t, c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg))
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(items), items)
	}
	lower := strings.ToLower(items[0])
	if !strings.Contains(lower, "identity") && !strings.Contains(lower, "username") {
		t.Errorf("missing item should instruct the user to configure an identity: %q", items[0])
	}
}

func TestCheck_MultipleMissingAggregated(t *testing.T) {
	// A tool, the token, and the identity are all absent: one error, all three.
	c := NewCheckerWithLookPath(noneFound)
	cfg := config.Config{GitHubUser: "", Token: ""}

	err := c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg)
	items := missing(t, err)
	if len(items) != 3 {
		t.Fatalf("expected 3 missing items, got %d: %v", len(items), items)
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"git", "token", "identity"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error must name every missing item; missing %q in: %q", want, err.Error())
		}
	}
	// Tools come before the configuration values.
	if !strings.HasPrefix(items[0], "git:") {
		t.Errorf("first item = %q, want the tool first", items[0])
	}
}

func TestCheck_TwoMissingGitAndToken(t *testing.T) {
	c := NewCheckerWithLookPath(noneFound)
	cfg := config.Config{GitHubUser: "octocat", Token: ""}

	err := c.Check(Need{Tools: Git, Token: true, Identity: true}, cfg)
	items := missing(t, err)
	if len(items) != 2 {
		t.Fatalf("expected 2 missing items (git, token), got %d: %v", len(items), items)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "git") || !strings.Contains(msg, "token") {
		t.Errorf("error must name git and token: %q", err.Error())
	}
	if strings.Contains(msg, "identity") || strings.Contains(msg, "username") {
		t.Errorf("error must not name identity when identity is present: %q", err.Error())
	}
}

// TestMissingError_SingleVsMultiple pins both renderings, since the single-item
// form is the common case and reads differently.
func TestMissingError_SingleVsMultiple(t *testing.T) {
	one := (&MissingError{Missing: []string{"git: nope"}}).Error()
	if !strings.HasPrefix(one, "missing prerequisite: ") {
		t.Errorf("single-item error = %q", one)
	}
	two := (&MissingError{Missing: []string{"git: nope", "helm: nope"}}).Error()
	if !strings.HasPrefix(two, "missing 2 prerequisites:") {
		t.Errorf("multi-item error = %q", two)
	}
	if strings.Count(two, "\n  - ") != 2 {
		t.Errorf("multi-item error should list each item on its own line: %q", two)
	}
}

func TestNewChecker_DefaultLookPath(t *testing.T) {
	// The production constructor must wire a non-nil LookPath, and a request with
	// no needs passes regardless of environment.
	c := NewChecker()
	if err := c.Check(Need{}, config.Config{}); err != nil {
		t.Fatalf("expected no error for empty Need with default checker, got: %v", err)
	}
}
