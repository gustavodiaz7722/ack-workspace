package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func TestCheckCodegenLocation_DirectoryInsideRoot(t *testing.T) {
	root := t.TempDir()
	codegen := filepath.Join(root, "code-generator")
	mkdirAll(t, codegen)

	if err := CheckCodegenLocation(root, codegen); err != nil {
		t.Errorf("CheckCodegenLocation = %v, want nil for a code-generator inside the root", err)
	}
}

// TestCheckCodegenLocation_SymlinkIntoAnotherTree is the case the check exists
// for: the scripts would resolve the controller, runtime and test-infra under
// the other tree, so the error must name it.
func TestCheckCodegenLocation_SymlinkIntoAnotherTree(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	mkdirAll(t, filepath.Join(other, "code-generator"))
	codegen := filepath.Join(root, "code-generator")
	symlink(t, filepath.Join(other, "code-generator"), codegen)

	err := CheckCodegenLocation(root, codegen)
	if err == nil {
		t.Fatal("CheckCodegenLocation = nil, want an error for a code-generator symlinked into another tree")
	}
	realOther, _ := filepath.EvalSymlinks(other)
	if !strings.Contains(err.Error(), realOther) {
		t.Errorf("error = %q, want it to name the tree the scripts would use (%s)", err, realOther)
	}
}

// A root reached through a symlink, as /home/you resolving to /local/home/you
// on cloud desktops, must not be mistaken for a misplaced code-generator.
func TestCheckCodegenLocation_RootReachedThroughSymlink(t *testing.T) {
	target := t.TempDir()
	mkdirAll(t, filepath.Join(target, "code-generator"))
	root := filepath.Join(t.TempDir(), "ws")
	symlink(t, target, root)

	if err := CheckCodegenLocation(root, filepath.Join(root, "code-generator")); err != nil {
		t.Errorf("CheckCodegenLocation = %v, want nil when only the root is a symlink", err)
	}
}

func TestCheckCodegenLocation_MissingPathIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := CheckCodegenLocation(root, filepath.Join(root, "code-generator")); err == nil {
		t.Error("CheckCodegenLocation = nil, want an error when the code-generator does not exist")
	}
}
