package workspace

import (
	"os"
	"path/filepath"
	"sort"
)

// Discover lists the Managed_Repositories found directly under root: immediate
// subdirectories that are git repositories (that is, they contain a ".git"
// entry, whether a directory or a worktree gitfile). The returned names are the
// repository directory names, sorted alphabetically (Requirement 6.1).
//
// A non-existent root is treated as an empty workspace: Discover returns an
// empty slice and a nil error so callers (such as the status command) can emit
// a friendly "no managed repositories" message rather than failing
// (Requirement 6.7).
func Discover(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	repos := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !isGitRepo(filepath.Join(root, entry.Name())) {
			continue
		}
		repos = append(repos, entry.Name())
	}

	sort.Strings(repos)
	return repos, nil
}

// isGitRepo reports whether dir looks like a git repository by checking for a
// ".git" entry inside it. The entry may be a directory (a normal clone) or a
// file (a git worktree's gitfile), so the check only asserts existence.
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
