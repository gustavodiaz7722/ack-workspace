package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// makeRepo creates a directory under root with a ".git" child. When asDir is
// true the ".git" child is a directory (a normal clone); otherwise it is a file
// (a worktree-style gitfile).
func makeRepo(t *testing.T, root, name string, asDir bool) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating repo dir %q: %v", dir, err)
	}
	gitPath := filepath.Join(dir, ".git")
	if asDir {
		if err := os.MkdirAll(gitPath, 0o755); err != nil {
			t.Fatalf("creating .git dir %q: %v", gitPath, err)
		}
		return
	}
	if err := os.WriteFile(gitPath, []byte("gitdir: ../.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("writing .git file %q: %v", gitPath, err)
	}
}

// makePlainDir creates a directory under root with no ".git" entry.
func makePlainDir(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatalf("creating plain dir %q: %v", name, err)
	}
}

func TestDiscover_ReturnsSortedGitRepos(t *testing.T) {
	root := t.TempDir()

	// Intentionally created out of alphabetical order to verify sorting.
	makeRepo(t, root, "test-infra", true)
	makeRepo(t, root, "code-generator", true)
	makeRepo(t, root, "runtime", false) // worktree-style gitfile

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	want := []string{"code-generator", "runtime", "test-infra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover() = %v, want %v", got, want)
	}
}

func TestDiscover_ExcludesNonGitDirsAndFiles(t *testing.T) {
	root := t.TempDir()

	makeRepo(t, root, "s3-controller", true)
	makePlainDir(t, root, "not-a-repo") // directory without .git
	makePlainDir(t, root, "scratch")    // directory without .git
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing top-level file: %v", err)
	}

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	want := []string{"s3-controller"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover() = %v, want %v", got, want)
	}
}

func TestDiscover_MissingRootReturnsEmptyNoError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover on missing root returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Discover on missing root = %v, want empty slice", got)
	}
}

func TestDiscover_EmptyRootReturnsEmptyNoError(t *testing.T) {
	root := t.TempDir()

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover on empty root returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Discover on empty root = %v, want empty slice", got)
	}
}
