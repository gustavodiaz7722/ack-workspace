// Package config implements the Configuration_Manager: it resolves the
// effective configuration for an invocation (flag > env > persisted > default)
// and persists durable settings to $HOME/.ack-workspace/config.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// configDirName is the directory under $HOME that holds the persisted config.
const configDirName = ".ack-workspace"

// configFileName is the name of the persisted configuration file.
const configFileName = "config"

// Config holds the effective configuration values for a command invocation.
type Config struct {
	GitHubUser    string // GitHub_Identity
	WorkspaceRoot string // absolute path
	RepoPrefix    string // default "ack-"
	Concurrency   int    // default 4, range 1..32
	Token         string // resolved, never persisted
}

// Source carries the raw, command-scoped inputs used during resolution.
type Source struct {
	Flags map[string]string // set flags only
	Env   map[string]string // GITHUB_TOKEN, etc.
}

// Manager resolves the effective configuration for an invocation and persists
// durable settings.
type Manager interface {
	// Resolve applies precedence: flag > env > persisted file > default.
	// Returns a typed error if the file is unparsable or a required value is missing.
	Resolve(src Source) (Config, error)
	// Save persists GitHubUser, WorkspaceRoot, RepoPrefix (never Token).
	Save(c Config) error
	// Path returns $HOME/.ack-workspace/config.
	Path() string
}

// fileConfig is the on-disk TOML representation. The token is intentionally
// absent so it is never written to disk (Requirement 2.5).
type fileConfig struct {
	GitHubUser    string `toml:"github_user"`
	WorkspaceRoot string `toml:"workspace_root"`
	RepoPrefix    string `toml:"repo_prefix"`
	Concurrency   int    `toml:"concurrency"`
}

// manager is the default Manager implementation. The home directory is
// injectable so tests can use a temporary $HOME.
type manager struct {
	home string
}

// NewManager returns a Manager that reads $HOME from the environment.
func NewManager() Manager {
	return &manager{home: os.Getenv("HOME")}
}

// NewManagerWithHome returns a Manager rooted at the given home directory. It is
// primarily intended for tests that need an isolated $HOME.
func NewManagerWithHome(home string) Manager {
	return &manager{home: home}
}

// Path returns $HOME/.ack-workspace/config.
func (m *manager) Path() string {
	return filepath.Join(m.home, configDirName, configFileName)
}

// dir returns $HOME/.ack-workspace.
func (m *manager) dir() string {
	return filepath.Join(m.home, configDirName)
}

// Save persists GitHubUser, WorkspaceRoot, and RepoPrefix as TOML to Path(),
// creating $HOME/.ack-workspace if it does not exist. The Token is never
// written (Requirement 2.5).
func (m *manager) Save(c Config) error {
	if err := os.MkdirAll(m.dir(), 0o755); err != nil {
		return fmt.Errorf("creating config directory %q: %w", m.dir(), err)
	}

	f, err := os.Create(m.Path())
	if err != nil {
		return fmt.Errorf("creating config file %q: %w", m.Path(), err)
	}
	defer f.Close()

	fc := fileConfig{
		GitHubUser:    c.GitHubUser,
		WorkspaceRoot: c.WorkspaceRoot,
		RepoPrefix:    c.RepoPrefix,
		Concurrency:   c.Concurrency,
	}
	if err := toml.NewEncoder(f).Encode(fc); err != nil {
		return fmt.Errorf("writing config file %q: %w", m.Path(), err)
	}
	return nil
}

// Resolve is implemented in resolve.go (Task 2.2).
