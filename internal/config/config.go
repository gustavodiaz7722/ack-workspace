// Package config resolves the effective configuration for an invocation (flag >
// env > workspace-local file > global file > default) and persists durable
// settings.
//
// There are two persisted layers. The global file at $HOME/.ack-workspace/config
// holds the settings shared by every workspace. A workspace-local file at
// <workspace-root>/.ack-workspace/config overrides it for work anywhere inside
// that tree, which is what lets several workspaces coexist with different roots,
// fork prefixes, or concurrency without repeating flags on every command.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// configDirName is the directory that holds a persisted config: under $HOME for
// the global layer, under a workspace root for the local layer.
const configDirName = ".ack-workspace"

// configFileName is the name of the persisted configuration file.
const configFileName = "config"

// Config holds the effective configuration values for a command invocation.
type Config struct {
	GitHubUser    string // GitHub identity, empty when none was supplied
	WorkspaceRoot string // absolute path
	RepoPrefix    string // default "ack-"
	Concurrency   int    // default 4, range 1..32
	Token         string // resolved, never persisted
	// Path is the configuration file this was resolved against, whether or not the
	// file exists: the workspace-local file when one was discovered, otherwise the
	// global one. It is carried so a component reporting a missing value can name
	// the file to persist it in.
	Path string
}

// Source carries the raw, command-scoped inputs used during resolution.
type Source struct {
	Flags map[string]string // set flags only
	Env   map[string]string // GITHUB_TOKEN, etc.
}

// Manager resolves the effective configuration for an invocation and persists
// durable settings.
type Manager interface {
	// Resolve applies precedence: flag > env > workspace-local file > global file
	// > default. Returns a typed error if a configuration file is unparsable.
	Resolve(src Source) (Config, error)
	// Save persists GitHubUser, WorkspaceRoot, RepoPrefix, and Concurrency (never
	// Token) to the file in effect for this invocation, which is Path().
	Save(c Config) error
	// SaveTo persists the same values to an explicit configuration file path,
	// creating its parent directory when needed.
	SaveTo(c Config, path string) error
	// Path returns the configuration file in effect: the nearest workspace-local
	// file found by walking up from the working directory, or GlobalPath when
	// there is none.
	Path() string
	// GlobalPath returns $HOME/.ack-workspace/config.
	GlobalPath() string
	// LocalPath returns the workspace-local configuration file for the working
	// directory, ./.ack-workspace/config. It returns the empty string when the
	// working directory cannot be determined.
	LocalPath() string
}

// fileConfig is the on-disk TOML representation, shared by both layers. The
// token is intentionally absent so it is never written to disk.
type fileConfig struct {
	GitHubUser    string `toml:"github_user"`
	WorkspaceRoot string `toml:"workspace_root"`
	RepoPrefix    string `toml:"repo_prefix"`
	Concurrency   int    `toml:"concurrency"`
}

// manager is the default Manager implementation. The home directory and the
// directory that discovery starts from are injectable so tests can run against
// a temporary $HOME without depending on the process working directory.
type manager struct {
	home string
	// cwd is where discovery starts. Empty means use the process working
	// directory, which is what the CLI wants.
	cwd string
}

// NewManager returns a Manager that reads $HOME from the environment and starts
// discovery at the process working directory.
func NewManager() Manager {
	return &manager{home: os.Getenv("HOME")}
}

// NewManagerWithHome returns a Manager rooted at the given home directory,
// starting discovery there as well. It is primarily intended for tests that need
// an isolated $HOME and no dependence on the process working directory.
func NewManagerWithHome(home string) Manager {
	return &manager{home: home, cwd: home}
}

// NewManagerWithHomeAndDir returns a Manager rooted at the given home directory
// that starts discovery at cwd. It is intended for tests that exercise
// workspace-local configuration discovery.
func NewManagerWithHomeAndDir(home, cwd string) Manager {
	return &manager{home: home, cwd: cwd}
}

// Path returns the configuration file in effect for this invocation.
func (m *manager) Path() string {
	if path, _, ok := m.discover(); ok {
		return path
	}
	return m.GlobalPath()
}

// GlobalPath returns $HOME/.ack-workspace/config.
func (m *manager) GlobalPath() string {
	return filepath.Join(m.home, configDirName, configFileName)
}

// LocalPath returns ./.ack-workspace/config for the working directory.
func (m *manager) LocalPath() string {
	dir, err := m.workdir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, configDirName, configFileName)
}

// workdir returns the absolute directory that discovery starts from.
func (m *manager) workdir() (string, error) {
	if m.cwd != "" {
		return filepath.Abs(m.cwd)
	}
	return os.Getwd()
}

// discover walks up from the working directory looking for a workspace-local
// <dir>/.ack-workspace/config, returning the first one found along with the
// directory holding it — the workspace root that file implies.
//
// The global config directory is skipped so that running from $HOME, or anywhere
// above a workspace, never mistakes the global file for a local one and infers a
// workspace root of $HOME from it.
func (m *manager) discover() (path string, root string, found bool) {
	dir, err := m.workdir()
	if err != nil {
		return "", "", false
	}

	globalDir := canonical(filepath.Join(m.home, configDirName))
	for {
		if candidateDir := filepath.Join(dir, configDirName); canonical(candidateDir) != globalDir {
			candidate := filepath.Join(candidateDir, configFileName)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// canonical resolves path through any symlinks so two spellings of the same
// directory compare equal, falling back to the input when it cannot be resolved
// (most often because it does not exist yet, which is normal for a config
// directory).
//
// Discovery depends on this. A home directory reached by a symlink -- $HOME set to
// /home/user while the real path is /local/home/user, a common cloud-desktop
// layout -- would otherwise fail the global-directory comparison, and the global
// config file would be picked up as though it were workspace-local, taking $HOME
// for the workspace root.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// Save persists to the configuration file in effect for this invocation.
func (m *manager) Save(c Config) error {
	return m.SaveTo(c, m.Path())
}

// SaveTo persists GitHubUser, WorkspaceRoot, RepoPrefix, and Concurrency as TOML
// to path, creating the parent directory if it does not exist. The Token is
// never written.
func (m *manager) SaveTo(c Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory %q: %w", dir, err)
	}

	fc := fileConfig{
		GitHubUser:    c.GitHubUser,
		WorkspaceRoot: c.WorkspaceRoot,
		RepoPrefix:    c.RepoPrefix,
		Concurrency:   c.Concurrency,
	}
	// A workspace-local file already states its root by where it sits, so writing
	// that same path into it would add nothing and would break the file if the
	// directory were later moved or renamed. Record workspace_root only when it
	// points somewhere other than the file's own directory. The global file has no
	// implied root, so it always records the value.
	if path != m.GlobalPath() && c.WorkspaceRoot == impliedRoot(path) {
		fc.WorkspaceRoot = ""
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating config file %q: %w", path, err)
	}
	defer f.Close()

	if err := toml.NewEncoder(f).Encode(fc); err != nil {
		return fmt.Errorf("writing config file %q: %w", path, err)
	}
	return nil
}

// impliedRoot returns the workspace root that a configuration file at path
// implies: for <root>/.ack-workspace/config that is <root>.
func impliedRoot(path string) string {
	return filepath.Dir(filepath.Dir(path))
}

// Resolve is implemented in resolve.go.
