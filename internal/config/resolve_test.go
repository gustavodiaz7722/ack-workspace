package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePersisted persists the given config to the manager's path so that
// resolution reads it as the "persisted" layer. It fails the test on error.
func writePersisted(t *testing.T, m Manager, c Config) {
	t.Helper()
	if err := m.Save(c); err != nil {
		t.Fatalf("setup: persisting config: %v", err)
	}
}

// TestResolvePrecedenceMatrix verifies, per configuration value, that the
// effective value is selected with precedence flag > env > persisted > default
// (Requirement 2.4). Each case isolates one value and constructs the layers so
// that a specific layer is expected to win.
func TestResolvePrecedenceMatrix(t *testing.T) {
	const absFlagRoot = "/tmp/flag/root"
	const absFileRoot = "/tmp/file/root"

	cases := []struct {
		name string
		// persisted, when non-nil, is written to the config file before Resolve.
		persisted *Config
		flags     map[string]string
		env       map[string]string
		// check inspects the resolved config for the value under test.
		check func(t *testing.T, got Config)
	}{
		// --- GitHubUser: flag > env > persisted (no default) ---
		{
			name:      "github user flag overrides env and persisted",
			persisted: &Config{GitHubUser: "fileuser"},
			flags:     map[string]string{FlagGitHubUser: "flaguser"},
			env:       map[string]string{EnvGitHubUser: "envuser"},
			check: func(t *testing.T, got Config) {
				if got.GitHubUser != "flaguser" {
					t.Errorf("GitHubUser = %q, want %q", got.GitHubUser, "flaguser")
				}
			},
		},
		{
			name:      "github user env overrides persisted",
			persisted: &Config{GitHubUser: "fileuser"},
			env:       map[string]string{EnvGitHubUser: "envuser"},
			check: func(t *testing.T, got Config) {
				if got.GitHubUser != "envuser" {
					t.Errorf("GitHubUser = %q, want %q", got.GitHubUser, "envuser")
				}
			},
		},
		{
			name:      "github user persisted wins when no flag or env",
			persisted: &Config{GitHubUser: "fileuser"},
			check: func(t *testing.T, got Config) {
				if got.GitHubUser != "fileuser" {
					t.Errorf("GitHubUser = %q, want %q", got.GitHubUser, "fileuser")
				}
			},
		},

		// --- WorkspaceRoot: flag > persisted > default (no env) ---
		{
			name:      "workspace root flag overrides persisted",
			persisted: &Config{GitHubUser: "fileuser", WorkspaceRoot: absFileRoot},
			flags:     map[string]string{FlagWorkspaceRoot: absFlagRoot},
			check: func(t *testing.T, got Config) {
				if got.WorkspaceRoot != absFlagRoot {
					t.Errorf("WorkspaceRoot = %q, want %q", got.WorkspaceRoot, absFlagRoot)
				}
			},
		},
		{
			name:      "workspace root persisted wins when no flag",
			persisted: &Config{GitHubUser: "fileuser", WorkspaceRoot: absFileRoot},
			check: func(t *testing.T, got Config) {
				if got.WorkspaceRoot != absFileRoot {
					t.Errorf("WorkspaceRoot = %q, want %q", got.WorkspaceRoot, absFileRoot)
				}
			},
		},

		// --- RepoPrefix: flag > persisted > default ---
		{
			name:      "repo prefix flag overrides persisted",
			persisted: &Config{GitHubUser: "fileuser", RepoPrefix: "file-"},
			flags:     map[string]string{FlagRepoPrefix: "flag-"},
			check: func(t *testing.T, got Config) {
				if got.RepoPrefix != "flag-" {
					t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, "flag-")
				}
			},
		},
		{
			name:      "repo prefix persisted wins when no flag",
			persisted: &Config{GitHubUser: "fileuser", RepoPrefix: "file-"},
			check: func(t *testing.T, got Config) {
				if got.RepoPrefix != "file-" {
					t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, "file-")
				}
			},
		},

		// --- Concurrency: flag > persisted > default ---
		{
			name:      "concurrency flag overrides persisted",
			persisted: &Config{GitHubUser: "fileuser", Concurrency: 8},
			flags:     map[string]string{FlagConcurrency: "16"},
			check: func(t *testing.T, got Config) {
				if got.Concurrency != 16 {
					t.Errorf("Concurrency = %d, want %d", got.Concurrency, 16)
				}
			},
		},
		{
			name:      "concurrency persisted wins when no flag",
			persisted: &Config{GitHubUser: "fileuser", Concurrency: 8},
			check: func(t *testing.T, got Config) {
				if got.Concurrency != 8 {
					t.Errorf("Concurrency = %d, want %d", got.Concurrency, 8)
				}
			},
		},

		// --- Token: flag > env (never default, never persisted) ---
		{
			name:      "token flag overrides env",
			persisted: &Config{GitHubUser: "fileuser"},
			flags:     map[string]string{FlagToken: "flag-token"},
			env:       map[string]string{EnvToken: "env-token"},
			check: func(t *testing.T, got Config) {
				if got.Token != "flag-token" {
					t.Errorf("Token = %q, want %q", got.Token, "flag-token")
				}
			},
		},
		{
			name:      "token env used when no flag",
			persisted: &Config{GitHubUser: "fileuser"},
			env:       map[string]string{EnvToken: "env-token"},
			check: func(t *testing.T, got Config) {
				if got.Token != "env-token" {
					t.Errorf("Token = %q, want %q", got.Token, "env-token")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManagerWithHome(t.TempDir())
			if tc.persisted != nil {
				writePersisted(t, m, *tc.persisted)
			}
			got, err := m.Resolve(Source{Flags: tc.flags, Env: tc.env})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			tc.check(t, got)
		})
	}
}

// TestResolveDefaults asserts the default values applied when no flag, env, or
// persisted value supplies a configuration value (Requirements 2.2, 2.3). A
// GitHub identity is supplied so resolution does not fail on the missing
// required value; only the defaulted values are asserted.
func TestResolveDefaults(t *testing.T) {
	m := NewManagerWithHome(t.TempDir())

	got, err := m.Resolve(Source{Flags: map[string]string{FlagGitHubUser: "octocat"}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	// WorkspaceRoot default ends with the upstream org path and is absolute.
	wantSuffix := filepath.FromSlash("src/github.com/aws-controllers-k8s")
	if !strings.HasSuffix(got.WorkspaceRoot, wantSuffix) {
		t.Errorf("WorkspaceRoot = %q, want suffix %q", got.WorkspaceRoot, wantSuffix)
	}
	if !filepath.IsAbs(got.WorkspaceRoot) {
		t.Errorf("WorkspaceRoot = %q, want an absolute path", got.WorkspaceRoot)
	}

	if got.RepoPrefix != DefaultRepoPrefix {
		t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, DefaultRepoPrefix)
	}
	if got.RepoPrefix != "ack-" {
		t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, "ack-")
	}
	if got.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", got.Concurrency, DefaultConcurrency)
	}
	if got.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want %d", got.Concurrency, 4)
	}
}

// TestResolveTokenNeverPersisted round-trips a Save (with a token) then reads
// the raw file to assert the token is absent, and confirms Resolve still
// surfaces the token from the environment for the invocation (Requirement 2.5).
func TestResolveTokenNeverPersisted(t *testing.T) {
	m := NewManagerWithHome(t.TempDir())

	const secret = "ghp_supersecrettokenvalue"
	if err := m.Save(Config{GitHubUser: "octocat", Token: secret}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("config file must never contain the token, got:\n%s", raw)
	}

	// The token is supplied for this invocation via the environment; it must be
	// resolved even though it was never written to the file.
	got, err := m.Resolve(Source{Env: map[string]string{EnvToken: secret}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Token != secret {
		t.Errorf("Token = %q, want %q", got.Token, secret)
	}
}

// TestResolveUnparsableFileError asserts that when the configuration file
// exists but cannot be parsed, Resolve returns a *ParseError naming the path
// (Requirement 2.6).
func TestResolveUnparsableFileError(t *testing.T) {
	home := t.TempDir()
	m := NewManagerWithHome(home)

	// Create the config directory and write malformed TOML at Path().
	if err := os.MkdirAll(filepath.Join(home, configDirName), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(m.Path(), []byte("this is = not [valid toml"), 0o644); err != nil {
		t.Fatalf("setup: writing malformed config: %v", err)
	}

	_, err := m.Resolve(Source{Flags: map[string]string{FlagGitHubUser: "octocat"}})
	if err == nil {
		t.Fatalf("Resolve() error = nil, want *ParseError")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("Resolve() error = %v (%T), want *ParseError", err, err)
	}
	if pe.Path != m.Path() {
		t.Errorf("ParseError.Path = %q, want %q", pe.Path, m.Path())
	}
}

// TestResolveMissingIdentityError asserts that when no configuration file
// exists and no GitHub identity is supplied by flag or environment variable,
// Resolve returns a *MissingGitHubUserError (Requirement 2.7).
func TestResolveMissingIdentityError(t *testing.T) {
	m := NewManagerWithHome(t.TempDir())

	_, err := m.Resolve(Source{})
	if err == nil {
		t.Fatalf("Resolve() error = nil, want *MissingGitHubUserError")
	}
	var me *MissingGitHubUserError
	if !errors.As(err, &me) {
		t.Fatalf("Resolve() error = %v (%T), want *MissingGitHubUserError", err, err)
	}
	if me.Path != m.Path() {
		t.Errorf("MissingGitHubUserError.Path = %q, want %q", me.Path, m.Path())
	}
}
