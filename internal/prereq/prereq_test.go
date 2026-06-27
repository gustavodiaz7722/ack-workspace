package prereq

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
)

// gitFound stubs LookPath to report that `git` resolves on the PATH.
func gitFound(file string) (string, error) {
	return "/usr/bin/" + file, nil
}

// gitMissing stubs LookPath to report that no executable resolves on the PATH.
func gitMissing(file string) (string, error) {
	return "", exec.ErrNotFound
}

func TestCheck_AllPresent(t *testing.T) {
	c := NewCheckerWithLookPath(gitFound)
	cfg := config.Config{GitHubUser: "octocat", Token: "tok"}

	if err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg); err != nil {
		t.Fatalf("expected no error when all prerequisites present, got: %v", err)
	}
}

func TestCheck_NoNeedsAlwaysPasses(t *testing.T) {
	// Even with git missing and empty config, a Need with nothing requested
	// must pass (e.g. the `config` command requires no prerequisites).
	c := NewCheckerWithLookPath(gitMissing)
	if err := c.Check(Need{}, config.Config{}); err != nil {
		t.Fatalf("expected no error for empty Need, got: %v", err)
	}
}

func TestCheck_GitMissing(t *testing.T) {
	c := NewCheckerWithLookPath(gitMissing)
	cfg := config.Config{GitHubUser: "octocat", Token: "tok"}

	err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg)
	if err == nil {
		t.Fatal("expected error when git is missing, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingError, got %T: %v", err, err)
	}
	if len(me.Missing) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(me.Missing), me.Missing)
	}
	if !strings.Contains(me.Error(), "git") {
		t.Errorf("error should identify git as missing: %q", me.Error())
	}
}

func TestCheck_TokenMissing(t *testing.T) {
	c := NewCheckerWithLookPath(gitFound)
	cfg := config.Config{GitHubUser: "octocat", Token: ""}

	err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg)
	if err == nil {
		t.Fatal("expected error when token is missing, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingError, got %T: %v", err, err)
	}
	if len(me.Missing) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(me.Missing), me.Missing)
	}
	if !strings.Contains(me.Error(), "token") {
		t.Errorf("error should instruct the user to supply a token: %q", me.Error())
	}
}

func TestCheck_TokenWhitespaceOnlyIsMissing(t *testing.T) {
	c := NewCheckerWithLookPath(gitFound)
	cfg := config.Config{GitHubUser: "octocat", Token: "   "}

	err := c.Check(Need{Token: true}, cfg)
	if err == nil {
		t.Fatal("expected error when token is whitespace-only, got nil")
	}
}

func TestCheck_IdentityMissing(t *testing.T) {
	c := NewCheckerWithLookPath(gitFound)
	cfg := config.Config{GitHubUser: "", Token: "tok"}

	err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg)
	if err == nil {
		t.Fatal("expected error when identity is missing, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingError, got %T: %v", err, err)
	}
	if len(me.Missing) != 1 {
		t.Fatalf("expected exactly 1 missing item, got %d: %v", len(me.Missing), me.Missing)
	}
	if !strings.Contains(strings.ToLower(me.Error()), "identity") &&
		!strings.Contains(strings.ToLower(me.Error()), "username") {
		t.Errorf("error should instruct the user to configure an identity: %q", me.Error())
	}
}

func TestCheck_NotRequestedNotChecked(t *testing.T) {
	// Identity is empty and token is empty, but only Git is requested, and git
	// resolves — so the check must pass.
	c := NewCheckerWithLookPath(gitFound)
	cfg := config.Config{} // no identity, no token

	if err := c.Check(Need{Git: true}, cfg); err != nil {
		t.Fatalf("expected no error when only Git requested and git present, got: %v", err)
	}
}

func TestCheck_MultipleMissingAggregated(t *testing.T) {
	// git missing, token missing, identity missing -> all three reported.
	c := NewCheckerWithLookPath(gitMissing)
	cfg := config.Config{GitHubUser: "", Token: ""}

	err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg)
	if err == nil {
		t.Fatal("expected aggregated error when multiple prerequisites missing, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingError, got %T: %v", err, err)
	}
	if len(me.Missing) != 3 {
		t.Fatalf("expected 3 missing items, got %d: %v", len(me.Missing), me.Missing)
	}

	msg := strings.ToLower(me.Error())
	for _, want := range []string{"git", "token", "identity"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error must name every missing item; missing %q in: %q", want, me.Error())
		}
	}
}

func TestCheck_TwoMissingGitAndToken(t *testing.T) {
	c := NewCheckerWithLookPath(gitMissing)
	cfg := config.Config{GitHubUser: "octocat", Token: ""}

	err := c.Check(Need{Git: true, Token: true, Identity: true}, cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var me *MissingError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MissingError, got %T: %v", err, err)
	}
	if len(me.Missing) != 2 {
		t.Fatalf("expected 2 missing items (git, token), got %d: %v", len(me.Missing), me.Missing)
	}
	msg := strings.ToLower(me.Error())
	if !strings.Contains(msg, "git") || !strings.Contains(msg, "token") {
		t.Errorf("error must name git and token: %q", me.Error())
	}
	if strings.Contains(msg, "identity") || strings.Contains(msg, "username") {
		t.Errorf("error must not name identity when identity is present: %q", me.Error())
	}
}

func TestNewChecker_DefaultLookPath(t *testing.T) {
	// Ensure the production constructor wires a non-nil LookPath and a request
	// with no needs passes regardless of environment.
	c := NewChecker()
	if err := c.Check(Need{}, config.Config{}); err != nil {
		t.Fatalf("expected no error for empty Need with default checker, got: %v", err)
	}
}
