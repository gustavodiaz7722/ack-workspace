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
// effective value is selected with precedence flag > env > persisted > default.
// Each case isolates one value and constructs the layers so that a specific
// layer is expected to win.
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
// persisted value supplies a configuration value. A GitHub identity is supplied
// so resolution does not fail on the missing required value; only the defaulted
// values are asserted.
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
// surfaces the token from the environment for the invocation.
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
// exists but cannot be parsed, Resolve returns a *ParseError naming the path.
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

// TestResolveMissingIdentityIsNotAnError pins that resolution succeeds with no
// configuration file and no identity supplied, leaving GitHubUser empty. Whether
// an identity is required is a per-command question that internal/prereq answers,
// so failing here would block the commands that legitimately need none
// (candidates, build, deploy, status).
func TestResolveMissingIdentityIsNotAnError(t *testing.T) {
	m := NewManagerWithHome(t.TempDir())

	cfg, err := m.Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if cfg.GitHubUser != "" {
		t.Errorf("GitHubUser = %q, want empty", cfg.GitHubUser)
	}
	// The defaults still apply, so the rest of the configuration is usable.
	if cfg.Concurrency != DefaultConcurrency || cfg.RepoPrefix != DefaultRepoPrefix {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

// TestResolveRecordsConfigPath pins that the resolved configuration carries the
// file it was resolved against, which is what lets the prerequisite checker name
// that file when it reports a missing identity.
func TestResolveRecordsConfigPath(t *testing.T) {
	m := NewManagerWithHome(t.TempDir())

	cfg, err := m.Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if cfg.Path != m.Path() {
		t.Errorf("Path = %q, want %q", cfg.Path, m.Path())
	}
}

// TestResolveLayerPrecedenceMatrix verifies, per configuration value, that a
// workspace-local file outranks the global file and that a flag still outranks
// both. Each case writes both layers so the winner is unambiguous.
func TestResolveLayerPrecedenceMatrix(t *testing.T) {
	cases := []struct {
		name   string
		global fileConfig
		local  fileConfig
		flags  map[string]string
		check  func(t *testing.T, got Config)
	}{
		{
			name:   "local github user overrides global",
			global: fileConfig{GitHubUser: "globaluser"},
			local:  fileConfig{GitHubUser: "localuser"},
			check: func(t *testing.T, got Config) {
				if got.GitHubUser != "localuser" {
					t.Errorf("GitHubUser = %q, want %q", got.GitHubUser, "localuser")
				}
			},
		},
		{
			name:   "flag overrides local github user",
			global: fileConfig{GitHubUser: "globaluser"},
			local:  fileConfig{GitHubUser: "localuser"},
			flags:  map[string]string{FlagGitHubUser: "flaguser"},
			check: func(t *testing.T, got Config) {
				if got.GitHubUser != "flaguser" {
					t.Errorf("GitHubUser = %q, want %q", got.GitHubUser, "flaguser")
				}
			},
		},
		{
			name:   "local prefix overrides global",
			global: fileConfig{RepoPrefix: "global-"},
			local:  fileConfig{RepoPrefix: "local-"},
			check: func(t *testing.T, got Config) {
				if got.RepoPrefix != "local-" {
					t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, "local-")
				}
			},
		},
		{
			name:   "flag overrides local prefix",
			global: fileConfig{RepoPrefix: "global-"},
			local:  fileConfig{RepoPrefix: "local-"},
			flags:  map[string]string{FlagRepoPrefix: "flag-"},
			check: func(t *testing.T, got Config) {
				if got.RepoPrefix != "flag-" {
					t.Errorf("RepoPrefix = %q, want %q", got.RepoPrefix, "flag-")
				}
			},
		},
		{
			name:   "local concurrency overrides global",
			global: fileConfig{Concurrency: 2},
			local:  fileConfig{Concurrency: 16},
			check: func(t *testing.T, got Config) {
				if got.Concurrency != 16 {
					t.Errorf("Concurrency = %d, want %d", got.Concurrency, 16)
				}
			},
		},
		{
			name:   "flag overrides local concurrency",
			global: fileConfig{Concurrency: 2},
			local:  fileConfig{Concurrency: 16},
			flags:  map[string]string{FlagConcurrency: "32"},
			check: func(t *testing.T, got Config) {
				if got.Concurrency != 32 {
					t.Errorf("Concurrency = %d, want %d", got.Concurrency, 32)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := t.TempDir()
			writeConfigFile(t, filepath.Join(home, configDirName, configFileName), tc.global)
			writeConfigFile(t, filepath.Join(root, configDirName, configFileName), tc.local)

			m := NewManagerWithHomeAndDir(home, root)
			got, err := m.Resolve(Source{Flags: tc.flags})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			tc.check(t, got)
		})
	}
}

// TestResolveLocalLayerInheritsUnsetValuesFromGlobal pins that the layers merge
// per value rather than the local file wholly replacing the global one. Without
// this, every workspace-local file would have to restate the GitHub identity and
// anything else shared across workspaces.
func TestResolveLocalLayerInheritsUnsetValuesFromGlobal(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()

	writeConfigFile(t, filepath.Join(home, configDirName, configFileName), fileConfig{
		GitHubUser:  "octocat",
		RepoPrefix:  "global-",
		Concurrency: 2,
	})
	// The local file overrides only the prefix.
	writeConfigFile(t, filepath.Join(root, configDirName, configFileName), fileConfig{
		RepoPrefix: "local-",
	})

	got, err := NewManagerWithHomeAndDir(home, root).Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.GitHubUser != "octocat" {
		t.Errorf("GitHubUser = %q, want %q inherited from the global file", got.GitHubUser, "octocat")
	}
	if got.Concurrency != 2 {
		t.Errorf("Concurrency = %d, want %d inherited from the global file", got.Concurrency, 2)
	}
	if got.RepoPrefix != "local-" {
		t.Errorf("RepoPrefix = %q, want %q from the local file", got.RepoPrefix, "local-")
	}
}

// TestResolveWorkspaceRootFromLocalConfigLocation is the crux of per-workspace
// configuration: a local file with no workspace_root still redirects commands to
// its own tree, outranking a root the global file names. Otherwise a second
// workspace could never be reached without passing --workspace-root every time.
func TestResolveWorkspaceRootFromLocalConfigLocation(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	const globalRoot = "/tmp/global/root"

	writeConfigFile(t, filepath.Join(home, configDirName, configFileName), fileConfig{
		GitHubUser:    "octocat",
		WorkspaceRoot: globalRoot,
	})
	writeConfigFile(t, filepath.Join(root, configDirName, configFileName), fileConfig{})

	// From the workspace root and from deep inside it, the answer is the same.
	deep := filepath.Join(root, "s3-controller", "pkg")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for _, dir := range []string{root, deep} {
		got, err := NewManagerWithHomeAndDir(home, dir).Resolve(Source{})
		if err != nil {
			t.Fatalf("Resolve() from %q error = %v", dir, err)
		}
		if got.WorkspaceRoot != root {
			t.Errorf("WorkspaceRoot from %q = %q, want %q", dir, got.WorkspaceRoot, root)
		}
	}
}

// TestResolveWorkspaceRootPrecedenceWithinLocalLayer pins the rest of the
// workspace-root chain: an explicit value in the local file beats the location it
// was found at, and a flag beats everything.
func TestResolveWorkspaceRootPrecedenceWithinLocalLayer(t *testing.T) {
	const explicitRoot = "/tmp/explicit/root"
	const flagRoot = "/tmp/flag/root"

	home := t.TempDir()
	root := t.TempDir()
	writeConfigFile(t, filepath.Join(home, configDirName, configFileName), fileConfig{WorkspaceRoot: "/tmp/global/root"})
	writeConfigFile(t, filepath.Join(root, configDirName, configFileName), fileConfig{WorkspaceRoot: explicitRoot})

	m := NewManagerWithHomeAndDir(home, root)

	got, err := m.Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.WorkspaceRoot != explicitRoot {
		t.Errorf("WorkspaceRoot = %q, want the local file's explicit %q", got.WorkspaceRoot, explicitRoot)
	}

	got, err = m.Resolve(Source{Flags: map[string]string{FlagWorkspaceRoot: flagRoot}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.WorkspaceRoot != flagRoot {
		t.Errorf("WorkspaceRoot = %q, want the flag's %q", got.WorkspaceRoot, flagRoot)
	}
}

// TestResolveRecordsLocalConfigPath pins that the resolved configuration names the
// local file when one is in effect. internal/prereq echoes this path when it
// reports a missing identity, so pointing at the global file here would send the
// contributor to edit the wrong one.
func TestResolveRecordsLocalConfigPath(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	localPath := filepath.Join(root, configDirName, configFileName)
	writeConfigFile(t, localPath, fileConfig{RepoPrefix: "local-"})

	got, err := NewManagerWithHomeAndDir(home, root).Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Path != localPath {
		t.Errorf("Path = %q, want %q", got.Path, localPath)
	}
}

// TestResolveUnparsableLocalFileError asserts that a malformed workspace-local
// file yields a *ParseError naming that file rather than the global one.
func TestResolveUnparsableLocalFileError(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	localPath := filepath.Join(root, configDirName, configFileName)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(localPath, []byte("this is = not [valid toml"), 0o644); err != nil {
		t.Fatalf("setup: writing malformed config: %v", err)
	}

	_, err := NewManagerWithHomeAndDir(home, root).Resolve(Source{})
	if err == nil {
		t.Fatalf("Resolve() error = nil, want *ParseError")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("Resolve() error = %v (%T), want *ParseError", err, err)
	}
	if pe.Path != localPath {
		t.Errorf("ParseError.Path = %q, want the local file %q", pe.Path, localPath)
	}
}
