package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Flag key names. These are the stable identifiers the CLI layer (Task 13.1)
// MUST use when populating Source.Flags. A key is present in the map only when
// the user explicitly set the corresponding flag.
const (
	FlagGitHubUser    = "github-user"
	FlagWorkspaceRoot = "workspace-root"
	FlagRepoPrefix    = "prefix"
	FlagConcurrency   = "concurrency"
	FlagToken         = "token"
)

// Environment variable key names. These are the stable identifiers the CLI
// layer (Task 13.1) MUST use when populating Source.Env. Only configuration
// values that define an environment variable appear here: the GitHub identity
// and the GitHub token (Requirements 2.4, 2.5, 2.7).
const (
	EnvGitHubUser = "GITHUB_USER"
	EnvToken      = "GITHUB_TOKEN"
)

// Default values applied when neither a flag, environment variable, nor
// persisted value supplies a configuration value (Requirements 2.2, 2.3).
const (
	// DefaultRepoPrefix is the default Repository_Prefix (Requirement 2.3).
	DefaultRepoPrefix = "ack-"
	// DefaultConcurrency is the default maximum concurrency (Requirement 7.1).
	DefaultConcurrency = 4
)

// upstreamOrgPath is the GitHub organization path appended to $GOPATH/src when
// computing the default Workspace_Root (Requirement 2.2).
const upstreamOrgPath = "src/github.com/aws-controllers-k8s"

// ParseError indicates the persisted configuration file exists but could not be
// read or parsed. It always names the configuration file path (Requirement 2.6).
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("configuration file %q exists but could not be parsed: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// MissingGitHubUserError indicates the configuration file does not exist and no
// GitHub_Identity was supplied through a flag or environment variable. The
// GitHub_Identity is the only required value that has no default
// (Requirement 2.7).
type MissingGitHubUserError struct {
	// Path is the configuration file path the user should create.
	Path string
}

func (e *MissingGitHubUserError) Error() string {
	return fmt.Sprintf(
		"missing required GitHub identity: supply it with the --%s flag, set the %s environment variable, or create the configuration file at %q",
		FlagGitHubUser, EnvGitHubUser, e.Path,
	)
}

// Resolve applies per-value precedence, highest first: command-line flag value,
// then environment variable value (where one is defined for that value), then
// the persisted file value, then the default value. The selected value applies
// only for this invocation (Requirement 2.4).
//
// The persisted TOML file at Path() is read when present. A missing file is
// acceptable. If the file exists but cannot be parsed, a *ParseError naming the
// path is returned (Requirement 2.6). If the file is absent and no GitHub
// identity is supplied by flag or environment variable, a
// *MissingGitHubUserError is returned (Requirement 2.7).
func (m *manager) Resolve(src Source) (Config, error) {
	persisted, fileExists, err := m.loadFile()
	if err != nil {
		return Config{}, err
	}

	var cfg Config

	// GitHubUser: flag > env > persisted. No default (Requirement 2.7).
	if v, ok := lookup(src.Flags, FlagGitHubUser); ok {
		cfg.GitHubUser = v
	} else if v, ok := lookup(src.Env, EnvGitHubUser); ok {
		cfg.GitHubUser = v
	} else {
		cfg.GitHubUser = persisted.GitHubUser
	}

	// WorkspaceRoot: flag > persisted > default, expanded to an absolute path
	// (Requirement 2.2). No environment variable is defined for this value.
	workspaceRoot := ""
	if v, ok := lookup(src.Flags, FlagWorkspaceRoot); ok {
		workspaceRoot = v
	} else if persisted.WorkspaceRoot != "" {
		workspaceRoot = persisted.WorkspaceRoot
	}
	if workspaceRoot == "" {
		workspaceRoot = m.defaultWorkspaceRoot()
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolving workspace root %q to an absolute path: %w", workspaceRoot, err)
	}
	cfg.WorkspaceRoot = abs

	// RepoPrefix: flag > persisted > default (Requirement 2.3). No environment
	// variable is defined for this value.
	if v, ok := lookup(src.Flags, FlagRepoPrefix); ok {
		cfg.RepoPrefix = v
	} else if persisted.RepoPrefix != "" {
		cfg.RepoPrefix = persisted.RepoPrefix
	} else {
		cfg.RepoPrefix = DefaultRepoPrefix
	}

	// Concurrency: flag > persisted > default (Requirements 2.4, 7.1). No
	// environment variable is defined for this value. Range validation (1..32,
	// Requirement 7.3) is performed by the concurrency-handling component, not
	// here.
	cfg.Concurrency = DefaultConcurrency
	if persisted.Concurrency != 0 {
		cfg.Concurrency = persisted.Concurrency
	}
	if v, ok := lookup(src.Flags, FlagConcurrency); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("invalid concurrency value %q: must be an integer", v)
		}
		cfg.Concurrency = n
	}

	// Token: flag > env. Never persisted (Requirement 2.5).
	if v, ok := lookup(src.Flags, FlagToken); ok {
		cfg.Token = v
	} else if v, ok := lookup(src.Env, EnvToken); ok {
		cfg.Token = v
	}

	// The configuration file is the only source of a persisted GitHub identity.
	// When it is absent and no identity was supplied by flag or environment
	// variable, the only required-without-default value is missing
	// (Requirement 2.7).
	if !fileExists && cfg.GitHubUser == "" {
		return Config{}, &MissingGitHubUserError{Path: m.Path()}
	}

	return cfg, nil
}

// loadFile reads the persisted configuration file. It reports whether the file
// exists. A missing file is not an error. A file that exists but cannot be read
// or parsed yields a *ParseError naming the path (Requirement 2.6).
func (m *manager) loadFile() (fileConfig, bool, error) {
	path := m.Path()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileConfig{}, false, nil
		}
		return fileConfig{}, false, &ParseError{Path: path, Err: err}
	}

	var fc fileConfig
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return fileConfig{}, false, &ParseError{Path: path, Err: err}
	}
	return fc, true, nil
}

// defaultWorkspaceRoot computes $GOPATH/src/github.com/aws-controllers-k8s,
// resolving $GOPATH via `go env GOPATH` and falling back to $HOME/go
// (Requirement 2.2).
func (m *manager) defaultWorkspaceRoot() string {
	return filepath.Join(m.gopath(), filepath.FromSlash(upstreamOrgPath))
}

// gopath resolves the effective GOPATH. It prefers `go env GOPATH` and falls
// back to $HOME/go when the go tool is unavailable or returns nothing.
func (m *manager) gopath() string {
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if gp := strings.TrimSpace(string(out)); gp != "" {
			return gp
		}
	}
	return filepath.Join(m.home, "go")
}

// lookup returns the value for key and whether the key was present. Presence
// distinguishes an explicitly set (possibly empty) value from an unset one,
// which keeps the precedence rules exact (Requirement 2.4).
func lookup(m map[string]string, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}
