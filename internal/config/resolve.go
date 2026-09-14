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

// Flag key names. These are the stable identifiers the CLI layer MUST use when
// populating Source.Flags. A key is present in the map only when the user
// explicitly set the corresponding flag.
const (
	FlagGitHubUser    = "github-user"
	FlagWorkspaceRoot = "workspace-root"
	FlagRepoPrefix    = "prefix"
	FlagConcurrency   = "concurrency"
	FlagToken         = "token"
)

// Environment variable key names. These are the stable identifiers the CLI
// layer MUST use when populating Source.Env. Only configuration values that
// define an environment variable appear here: the GitHub identity and the
// GitHub token.
const (
	EnvGitHubUser = "GITHUB_USER"
	EnvToken      = "GITHUB_TOKEN"
)

// Default values applied when neither a flag, environment variable, nor
// persisted value supplies a configuration value.
const (
	// DefaultRepoPrefix is the default fork name prefix.
	DefaultRepoPrefix = "ack-"
	// DefaultConcurrency is the default maximum concurrency.
	DefaultConcurrency = 4
)

// upstreamOrgPath is the GitHub organization path appended to $GOPATH/src when
// computing the default workspace root.
const upstreamOrgPath = "src/github.com/aws-controllers-k8s"

// ParseError indicates the persisted configuration file exists but could not be
// read or parsed. It always names the configuration file path.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("configuration file %q exists but could not be parsed: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// fileLayers holds the persisted configuration layers found for an invocation.
type fileLayers struct {
	// local holds the workspace-local file's values; meaningful only when
	// localFound is true.
	local      fileConfig
	localFound bool
	// localRoot is the directory holding the workspace-local .ack-workspace
	// directory, and therefore the workspace root that file implies.
	localRoot string
	// global holds the $HOME file's values, zero when that file is absent.
	global fileConfig
	// path is the file in effect: the local one when found, else the global one.
	path string
}

// loadLayers reads both persisted layers. The global file is always consulted so
// that a workspace-local file can override individual values without having to
// restate the ones it shares, notably the GitHub identity.
func (m *manager) loadLayers() (fileLayers, error) {
	layers := fileLayers{path: m.GlobalPath()}

	global, err := m.loadFile(m.GlobalPath())
	if err != nil {
		return fileLayers{}, err
	}
	layers.global = global

	if path, root, ok := m.discover(); ok {
		local, err := m.loadFile(path)
		if err != nil {
			return fileLayers{}, err
		}
		layers.local, layers.localFound, layers.localRoot, layers.path = local, true, root, path
	}

	return layers, nil
}

// Resolve applies per-value precedence, highest first: command-line flag value,
// then environment variable value (where one is defined for that value), then the
// workspace-local file value, then the global file value, then the default value.
// The selected value applies only for this invocation.
//
// Both persisted TOML files are read when present. A missing file is acceptable;
// so is a missing GitHub identity, which resolves to the empty string. Whether an
// identity is required depends on the command -- only the ones that name a fork
// need one -- so that is enforced per command by internal/prereq rather than
// here. The only error is a *ParseError, for a file that exists but cannot be
// parsed.
func (m *manager) Resolve(src Source) (Config, error) {
	layers, err := m.loadLayers()
	if err != nil {
		return Config{}, err
	}

	var cfg Config

	// GitHubUser: flag > env > local file > global file. No default.
	if v, ok := lookup(src.Flags, FlagGitHubUser); ok {
		cfg.GitHubUser = v
	} else if v, ok := lookup(src.Env, EnvGitHubUser); ok {
		cfg.GitHubUser = v
	} else if layers.local.GitHubUser != "" {
		cfg.GitHubUser = layers.local.GitHubUser
	} else {
		cfg.GitHubUser = layers.global.GitHubUser
	}

	// WorkspaceRoot: flag > local file > the local file's own directory > global
	// file > default, expanded to an absolute path. No environment variable is
	// defined for this value.
	//
	// The local file's own directory outranks the global file because standing
	// inside a workspace is a stronger statement of intent than a root persisted
	// once for the whole machine. That ordering is what lets a workspace-local file
	// carry no workspace_root at all and still redirect every command to its own
	// tree.
	workspaceRoot := ""
	if v, ok := lookup(src.Flags, FlagWorkspaceRoot); ok {
		workspaceRoot = v
	} else if layers.local.WorkspaceRoot != "" {
		workspaceRoot = layers.local.WorkspaceRoot
	} else if layers.localFound {
		workspaceRoot = layers.localRoot
	} else if layers.global.WorkspaceRoot != "" {
		workspaceRoot = layers.global.WorkspaceRoot
	}
	if workspaceRoot == "" {
		workspaceRoot = m.defaultWorkspaceRoot()
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolving workspace root %q to an absolute path: %w", workspaceRoot, err)
	}
	cfg.WorkspaceRoot = abs

	// RepoPrefix: flag > local file > global file > default. No environment
	// variable is defined for this value.
	if v, ok := lookup(src.Flags, FlagRepoPrefix); ok {
		cfg.RepoPrefix = v
	} else if layers.local.RepoPrefix != "" {
		cfg.RepoPrefix = layers.local.RepoPrefix
	} else if layers.global.RepoPrefix != "" {
		cfg.RepoPrefix = layers.global.RepoPrefix
	} else {
		cfg.RepoPrefix = DefaultRepoPrefix
	}

	// Concurrency: flag > local file > global file > default. No environment
	// variable is defined for this value. The 1..32 range is enforced by the
	// command layer (cmd.validateConcurrency), which rejects an out-of-range value
	// before any work starts; resolution only decides which value wins.
	cfg.Concurrency = DefaultConcurrency
	if layers.global.Concurrency != 0 {
		cfg.Concurrency = layers.global.Concurrency
	}
	if layers.local.Concurrency != 0 {
		cfg.Concurrency = layers.local.Concurrency
	}
	if v, ok := lookup(src.Flags, FlagConcurrency); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("invalid concurrency value %q: must be an integer", v)
		}
		cfg.Concurrency = n
	}

	// Token: flag > env. Never persisted.
	if v, ok := lookup(src.Flags, FlagToken); ok {
		cfg.Token = v
	} else if v, ok := lookup(src.Env, EnvToken); ok {
		cfg.Token = v
	}

	cfg.Path = layers.path

	return cfg, nil
}

// loadFile reads a persisted configuration file, returning a zero fileConfig when
// it does not exist — an absent file is the normal state before the first
// `config set`. A file that exists but cannot be read or parsed yields a
// *ParseError naming the path.
func (m *manager) loadFile(path string) (fileConfig, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileConfig{}, nil
		}
		return fileConfig{}, &ParseError{Path: path, Err: err}
	}

	var fc fileConfig
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return fileConfig{}, &ParseError{Path: path, Err: err}
	}
	return fc, nil
}

// defaultWorkspaceRoot computes $GOPATH/src/github.com/aws-controllers-k8s,
// resolving $GOPATH via `go env GOPATH` and falling back to $HOME/go.
func (m *manager) defaultWorkspaceRoot() string {
	return filepath.Join(m.gopath(), filepath.FromSlash(upstreamOrgPath))
}

// gopath resolves the effective GOPATH: the GOPATH environment variable when it
// is set, then `go env GOPATH`, then $HOME/go when the go tool is unavailable or
// returns nothing.
//
// Reading the environment first is not just a shortcut. Shelling out to `go` has
// side effects — among them writing telemetry under the user's config directory —
// so a caller that already knows its GOPATH (a test with an isolated HOME, for
// one) can say so and keep this hermetic.
func (m *manager) gopath() string {
	if gp := strings.TrimSpace(os.Getenv("GOPATH")); gp != "" {
		return gp
	}
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if gp := strings.TrimSpace(string(out)); gp != "" {
			return gp
		}
	}
	return filepath.Join(m.home, "go")
}

// lookup returns the value for key and whether the key was present. Presence
// distinguishes an explicitly set (possibly empty) value from an unset one,
// which keeps the precedence rules exact.
func lookup(m map[string]string, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}
