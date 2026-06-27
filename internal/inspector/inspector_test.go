package inspector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// makeWorkspace creates a temp Workspace_Root containing one subdirectory per
// repo name, each holding a ".git" entry so workspace.Discover treats it as a
// Managed_Repository.
func makeWorkspace(t *testing.T, repos ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, r := range repos {
		if err := os.MkdirAll(filepath.Join(root, r, ".git"), 0o755); err != nil {
			t.Fatalf("creating repo %q: %v", r, err)
		}
	}
	return root
}

// newApp builds an App wired to the given mock git Runner and workspace root.
func newApp(root string, runner git.Runner) app.App {
	return app.App{
		Config: config.Config{WorkspaceRoot: root, Concurrency: 4},
		Git:    runner,
	}
}

// TestStatusTableNormalRepos verifies the human-readable table for repositories
// that are up to date, ahead, and behind, with clean/dirty working trees
// (Requirements 6.1, 6.2, 6.4, 6.5).
func TestStatusTableNormalRepos(t *testing.T) {
	root := makeWorkspace(t, "code-generator", "runtime", "test-infra")
	runner := &git.MockRunner{
		ResponseFunc: func(dir string, args []string) (string, error) {
			repo := filepath.Base(dir)
			switch args[0] {
			case "symbolic-ref":
				return "main\n", nil
			case "status":
				if repo == "test-infra" {
					return " M file.go\n", nil // dirty
				}
				return "", nil // clean
			case "rev-list":
				switch repo {
				case "code-generator":
					return "3\t0\n", nil // ahead 3
				case "test-infra":
					return "0\t5\n", nil // behind 5
				default:
					return "0\t0\n", nil // up to date
				}
			}
			return "", nil
		},
	}

	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	summary, err := in.Status(context.Background(), newApp(root, runner), false)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if summary.HasFailures() {
		t.Errorf("status must never report failures, got %d results", len(summary.Results))
	}

	out := buf.String()

	// Repositories listed alphabetically (Requirement 6.1).
	if i, j := strings.Index(out, "code-generator"), strings.Index(out, "runtime"); i < 0 || j < 0 || i > j {
		t.Errorf("repos not listed alphabetically:\n%s", out)
	}
	if i, j := strings.Index(out, "runtime"), strings.Index(out, "test-infra"); i < 0 || j < 0 || i > j {
		t.Errorf("repos not listed alphabetically:\n%s", out)
	}

	for _, want := range []string{"main", "ahead 3", "up to date", "behind 5", "dirty", "clean"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusJSON verifies that --json emits a single JSON document (array) whose
// per-entry fields are correct (Requirement 6.8).
func TestStatusJSON(t *testing.T) {
	root := makeWorkspace(t, "s3-controller")
	runner := &git.MockRunner{
		ResponseFunc: func(_ string, args []string) (string, error) {
			switch args[0] {
			case "symbolic-ref":
				return "main\n", nil
			case "status":
				return " M pkg/resource.go\n", nil // dirty
			case "rev-list":
				return "0\t2\n", nil // behind 2
			}
			return "", nil
		},
	}

	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	if _, err := in.Status(context.Background(), newApp(root, runner), true); err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	// A single JSON document: the whole output unmarshals into one array.
	var entries []workspace.StatusEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("output is not a single valid JSON document: %v\n%s", err, buf.String())
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	want := workspace.StatusEntry{
		Repo:       "s3-controller",
		Branch:     "main",
		Detached:   false,
		Comparison: "behind",
		Ahead:      0,
		Behind:     2,
		Dirty:      true,
	}
	if got != want {
		t.Errorf("entry mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestStatusDetachedHEAD verifies that a detached HEAD is reported as such with
// no branch name, and the comparison is unavailable (Requirements 6.3, 6.6).
func TestStatusDetachedHEAD(t *testing.T) {
	root := makeWorkspace(t, "runtime")
	revListCalled := false
	runner := &git.MockRunner{
		ResponseFunc: func(_ string, args []string) (string, error) {
			switch args[0] {
			case "symbolic-ref":
				// `git symbolic-ref --short -q HEAD` exits 1 on a detached HEAD.
				return "", &git.ExitError{Code: 1}
			case "status":
				return "", nil
			case "rev-list":
				revListCalled = true
				return "0\t0\n", nil
			}
			return "", nil
		},
	}

	// JSON makes the structured fields easy to assert.
	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	if _, err := in.Status(context.Background(), newApp(root, runner), true); err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	var entries []workspace.StatusEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if !got.Detached {
		t.Errorf("expected Detached=true, got %+v", got)
	}
	if got.Branch != "" {
		t.Errorf("expected empty Branch on detached HEAD, got %q", got.Branch)
	}
	if got.Comparison != "unavailable" {
		t.Errorf("expected Comparison=unavailable on detached HEAD, got %q", got.Comparison)
	}
	if revListCalled {
		t.Errorf("comparison must not be attempted for a detached HEAD")
	}

	// The human table should label the detached state.
	var tbuf bytes.Buffer
	tin := NewWithWriter(&tbuf)
	if _, err := tin.Status(context.Background(), newApp(root, runner), false); err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !strings.Contains(tbuf.String(), "detached") {
		t.Errorf("table output missing detached indicator:\n%s", tbuf.String())
	}
}

// TestStatusComparisonUnavailableContinues verifies that when the ahead/behind
// comparison cannot be determined for one repository (AheadBehind errors), that
// repository is reported as "unavailable" and the remaining repositories are
// still listed (Requirement 6.6).
func TestStatusComparisonUnavailableContinues(t *testing.T) {
	root := makeWorkspace(t, "alpha", "beta")
	runner := &git.MockRunner{
		ResponseFunc: func(dir string, args []string) (string, error) {
			switch args[0] {
			case "symbolic-ref":
				return "main\n", nil
			case "status":
				return "", nil
			case "rev-list":
				if filepath.Base(dir) == "alpha" {
					// upstream/main not present locally -> AheadBehind fails.
					return "", errors.New("fatal: ambiguous argument 'upstream/main'")
				}
				return "0\t0\n", nil
			}
			return "", nil
		},
	}

	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	if _, err := in.Status(context.Background(), newApp(root, runner), true); err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	var entries []workspace.StatusEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (listing must continue), got %d", len(entries))
	}

	byName := map[string]workspace.StatusEntry{}
	for _, e := range entries {
		byName[e.Repo] = e
	}
	if byName["alpha"].Comparison != "unavailable" {
		t.Errorf("expected alpha Comparison=unavailable, got %q", byName["alpha"].Comparison)
	}
	if byName["beta"].Comparison != "up_to_date" {
		t.Errorf("expected beta Comparison=up_to_date, got %q", byName["beta"].Comparison)
	}
}

// TestStatusEmptyWorkspace verifies that an empty workspace produces a friendly
// message and no error (Requirement 6.7).
func TestStatusEmptyWorkspace(t *testing.T) {
	root := makeWorkspace(t) // no repos

	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	summary, err := in.Status(context.Background(), newApp(root, &git.MockRunner{}), false)
	if err != nil {
		t.Fatalf("Status returned error on empty workspace: %v", err)
	}
	if summary.HasFailures() {
		t.Errorf("empty workspace must not report failures")
	}
	if !strings.Contains(strings.ToLower(buf.String()), "no managed repositories") {
		t.Errorf("expected friendly empty-workspace message, got:\n%s", buf.String())
	}
}

// TestStatusEmptyWorkspaceJSON verifies that an empty workspace in JSON mode
// emits an empty JSON array rather than null (Requirements 6.7, 6.8).
func TestStatusEmptyWorkspaceJSON(t *testing.T) {
	root := makeWorkspace(t) // no repos

	var buf bytes.Buffer
	in := NewWithWriter(&buf)
	if _, err := in.Status(context.Background(), newApp(root, &git.MockRunner{}), true); err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	var entries []workspace.StatusEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON for empty workspace: %v\n%s", err, buf.String())
	}
	if len(entries) != 0 {
		t.Errorf("expected empty array, got %d entries", len(entries))
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Errorf("expected \"[]\", got %q", strings.TrimSpace(buf.String()))
	}
}
