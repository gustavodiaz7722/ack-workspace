package workspace

import (
	"fmt"
	"path/filepath"
)

// CheckCodegenLocation returns an error unless the code-generator at
// codegenPath is a directory directly inside the workspace root, once symlinks
// are resolved on both sides.
//
// The code-generator scripts find everything else relative to their own
// directory, which the shell resolves through any symlink: the controller they
// regenerate or build ("<code-generator>/../<service>-controller"), the
// runtime, test-infra, and the Docker build context of the controller image
// ("<code-generator>/.."). When "<root>/code-generator" is a symlink into
// another tree, every one of those lands in that other tree. A build or deploy
// then acts on a different controller from the one it reports, and succeeds,
// so the mismatch is refused before any work starts.
//
// A workspace root reached through a symlink (for example /home/you resolving
// to /local/home/you) is fine: both paths are resolved before comparing.
func CheckCodegenLocation(root, codegenPath string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving workspace root %s: %w", root, err)
	}
	realCodegen, err := filepath.EvalSymlinks(codegenPath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", codegenPath, err)
	}
	if parent := filepath.Dir(realCodegen); parent != realRoot {
		return fmt.Errorf(
			"%s resolves to %s, outside the workspace root %s; the code-generator scripts find "+
				"the controller, runtime and test-infra relative to their own location, so they "+
				"would use the copies under %s instead; replace the symlink with a clone or a "+
				"`git worktree add` of the code-generator",
			codegenPath, realCodegen, realRoot, parent)
	}
	return nil
}
