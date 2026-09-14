package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestPath(t *testing.T) {
	home := "/home/octocat"
	m := NewManagerWithHome(home)
	want := filepath.Join(home, ".ack-workspace", "config")
	if got := m.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestSaveCreatesDirectoryAndWritesTOML(t *testing.T) {
	home := t.TempDir()
	m := NewManagerWithHome(home)

	cfg := Config{
		GitHubUser:    "octocat",
		WorkspaceRoot: "/home/octocat/go/src/github.com/aws-controllers-k8s",
		RepoPrefix:    "ack-",
		Concurrency:   4,
		Token:         "super-secret-token",
	}
	if err := m.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The directory should have been created.
	dir := filepath.Join(home, ".ack-workspace")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("expected config directory %q to exist, err = %v", dir, err)
	}

	// The file should exist and parse back to the persisted fields.
	var fc fileConfig
	if _, err := toml.DecodeFile(m.Path(), &fc); err != nil {
		t.Fatalf("decoding saved config: %v", err)
	}
	if fc.GitHubUser != cfg.GitHubUser {
		t.Errorf("github_user = %q, want %q", fc.GitHubUser, cfg.GitHubUser)
	}
	if fc.WorkspaceRoot != cfg.WorkspaceRoot {
		t.Errorf("workspace_root = %q, want %q", fc.WorkspaceRoot, cfg.WorkspaceRoot)
	}
	if fc.RepoPrefix != cfg.RepoPrefix {
		t.Errorf("repo_prefix = %q, want %q", fc.RepoPrefix, cfg.RepoPrefix)
	}
	if fc.Concurrency != cfg.Concurrency {
		t.Errorf("concurrency = %d, want %d", fc.Concurrency, cfg.Concurrency)
	}
}

func TestSaveNeverWritesToken(t *testing.T) {
	home := t.TempDir()
	m := NewManagerWithHome(home)

	cfg := Config{
		GitHubUser: "octocat",
		Token:      "super-secret-token",
	}
	if err := m.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-token") {
		t.Errorf("config file must never contain the token, got:\n%s", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Errorf("config file must not contain a token key, got:\n%s", raw)
	}
}

func TestSaveWhenDirectoryAlreadyExists(t *testing.T) {
	home := t.TempDir()
	// Pre-create the directory to confirm Save tolerates an existing dir.
	if err := os.MkdirAll(filepath.Join(home, ".ack-workspace"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	m := NewManagerWithHome(home)

	if err := m.Save(Config{GitHubUser: "octocat"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(m.Path()); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

// writeConfigFile writes a TOML config file at path, creating its parent
// directory. It fails the test on error.
func writeConfigFile(t *testing.T, path string, fc fileConfig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("setup: creating %q: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("setup: creating %q: %v", path, err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(fc); err != nil {
		t.Fatalf("setup: encoding %q: %v", path, err)
	}
}

// TestGlobalPathIsAlwaysHome pins that GlobalPath ignores discovery entirely: it
// is the file `config set` without --local falls back to, so it must not move.
func TestGlobalPathIsAlwaysHome(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeConfigFile(t, filepath.Join(root, ".ack-workspace", "config"), fileConfig{RepoPrefix: "local-"})

	m := NewManagerWithHomeAndDir(home, root)
	want := filepath.Join(home, ".ack-workspace", "config")
	if got := m.GlobalPath(); got != want {
		t.Errorf("GlobalPath() = %q, want %q", got, want)
	}
	// Path, by contrast, must follow discovery.
	if got := m.Path(); got == want {
		t.Errorf("Path() = %q, want the workspace-local file, not the global one", got)
	}
}

// TestPathDiscoversWorkspaceLocalConfig verifies that Path walks up from the
// working directory to find a workspace-local file, so a command run from deep
// inside a controller repo picks up the workspace's own configuration.
func TestPathDiscoversWorkspaceLocalConfig(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	localPath := filepath.Join(root, ".ack-workspace", "config")
	writeConfigFile(t, localPath, fileConfig{RepoPrefix: "local-"})

	deep := filepath.Join(root, "s3-controller", "pkg", "resource", "bucket")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for _, dir := range []string{root, deep} {
		m := NewManagerWithHomeAndDir(home, dir)
		if got := m.Path(); got != localPath {
			t.Errorf("Path() from %q = %q, want %q", dir, got, localPath)
		}
	}
}

// TestPathFallsBackToGlobalWithoutLocalConfig pins the pre-existing behaviour for
// a working directory that is not inside any workspace: the global file is used.
func TestPathFallsBackToGlobalWithoutLocalConfig(t *testing.T) {
	home := t.TempDir()
	m := NewManagerWithHomeAndDir(home, t.TempDir())

	want := filepath.Join(home, ".ack-workspace", "config")
	if got := m.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// TestPathNeverTreatsGlobalConfigAsLocal guards the case that would otherwise
// misfire: running from $HOME, or any directory above a workspace, must not let
// the global file be discovered as a workspace-local one. If it were, its
// directory would be taken as the workspace root and every command would target
// $HOME.
func TestPathNeverTreatsGlobalConfigAsLocal(t *testing.T) {
	home := t.TempDir()
	globalPath := filepath.Join(home, ".ack-workspace", "config")
	writeConfigFile(t, globalPath, fileConfig{GitHubUser: "octocat"})

	sub := filepath.Join(home, "some", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for _, dir := range []string{home, sub} {
		m := NewManagerWithHomeAndDir(home, dir)
		if got := m.Path(); got != globalPath {
			t.Errorf("Path() from %q = %q, want the global file %q", dir, got, globalPath)
		}
		cfg, err := m.Resolve(Source{})
		if err != nil {
			t.Fatalf("Resolve() from %q error = %v", dir, err)
		}
		// The decisive assertion: no workspace root was inferred from $HOME.
		if cfg.WorkspaceRoot == home {
			t.Errorf("WorkspaceRoot from %q = %q, want the default root, not $HOME", dir, cfg.WorkspaceRoot)
		}
	}
}

// TestPathPrefersNearestLocalConfig pins that the innermost workspace-local file
// wins when workspaces are nested.
func TestPathPrefersNearestLocalConfig(t *testing.T) {
	home := t.TempDir()
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")

	writeConfigFile(t, filepath.Join(outer, ".ack-workspace", "config"), fileConfig{RepoPrefix: "outer-"})
	innerPath := filepath.Join(inner, ".ack-workspace", "config")
	writeConfigFile(t, innerPath, fileConfig{RepoPrefix: "inner-"})

	m := NewManagerWithHomeAndDir(home, inner)
	if got := m.Path(); got != innerPath {
		t.Errorf("Path() = %q, want %q", got, innerPath)
	}
}

// TestSaveToLocalOmitsRedundantWorkspaceRoot pins that a workspace-local file
// leaves workspace_root out when it matches the file's own directory. The path is
// already implied by where the file sits, so recording it would only make the file
// stop working if the directory were moved or renamed.
func TestSaveToLocalOmitsRedundantWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	localPath := filepath.Join(root, ".ack-workspace", "config")

	m := NewManagerWithHomeAndDir(home, root)
	cfg := Config{GitHubUser: "octocat", WorkspaceRoot: root, RepoPrefix: "ack-ws-", Concurrency: 8}
	if err := m.SaveTo(cfg, localPath); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	var fc fileConfig
	if _, err := toml.DecodeFile(localPath, &fc); err != nil {
		t.Fatalf("decoding saved config: %v", err)
	}
	if fc.WorkspaceRoot != "" {
		t.Errorf("workspace_root = %q, want it omitted as implied by the file's location", fc.WorkspaceRoot)
	}
	// The values that are not implied by location must still be written.
	if fc.RepoPrefix != "ack-ws-" || fc.GitHubUser != "octocat" || fc.Concurrency != 8 {
		t.Errorf("saved config lost non-implied values: %+v", fc)
	}

	// And resolution must recover the root from the file's location.
	got, err := m.Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.WorkspaceRoot != root {
		t.Errorf("WorkspaceRoot = %q, want %q", got.WorkspaceRoot, root)
	}
}

// TestSaveToLocalRecordsDivergentWorkspaceRoot is the counterpart: a root that is
// not the file's own directory is not implied by anything, so it must be written.
func TestSaveToLocalRecordsDivergentWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	elsewhere := t.TempDir()
	localPath := filepath.Join(root, ".ack-workspace", "config")

	m := NewManagerWithHomeAndDir(home, root)
	if err := m.SaveTo(Config{WorkspaceRoot: elsewhere}, localPath); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	var fc fileConfig
	if _, err := toml.DecodeFile(localPath, &fc); err != nil {
		t.Fatalf("decoding saved config: %v", err)
	}
	if fc.WorkspaceRoot != elsewhere {
		t.Errorf("workspace_root = %q, want %q", fc.WorkspaceRoot, elsewhere)
	}
}

// TestSaveToGlobalAlwaysRecordsWorkspaceRoot pins that the omission rule is
// scoped to workspace-local files. The global file sits in $HOME, which is never
// a workspace root, so dropping the value there would silently repoint every
// command at the built-in default.
func TestSaveToGlobalAlwaysRecordsWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	m := NewManagerWithHomeAndDir(home, home)

	// A root equal to the global file's implied directory ($HOME) is the case the
	// local rule would drop.
	if err := m.SaveTo(Config{WorkspaceRoot: home}, m.GlobalPath()); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	var fc fileConfig
	if _, err := toml.DecodeFile(m.GlobalPath(), &fc); err != nil {
		t.Fatalf("decoding saved config: %v", err)
	}
	if fc.WorkspaceRoot != home {
		t.Errorf("workspace_root = %q, want %q recorded verbatim in the global file", fc.WorkspaceRoot, home)
	}
}

// TestSaveWritesToFileInEffect pins that Save targets the discovered
// workspace-local file rather than the global one, so `config set` inside a
// workspace edits that workspace's configuration.
func TestSaveWritesToFileInEffect(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	localPath := filepath.Join(root, ".ack-workspace", "config")
	writeConfigFile(t, localPath, fileConfig{RepoPrefix: "local-"})

	m := NewManagerWithHomeAndDir(home, root)
	if err := m.Save(Config{GitHubUser: "octocat", WorkspaceRoot: root, RepoPrefix: "updated-"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var local fileConfig
	if _, err := toml.DecodeFile(localPath, &local); err != nil {
		t.Fatalf("decoding local config: %v", err)
	}
	if local.RepoPrefix != "updated-" {
		t.Errorf("local repo_prefix = %q, want %q", local.RepoPrefix, "updated-")
	}
	if _, err := os.Stat(m.GlobalPath()); err == nil {
		t.Errorf("global config at %q must not have been created", m.GlobalPath())
	}
}

// TestLocalPathIsWorkingDirectory pins that LocalPath names the working
// directory's own file, not a discovered ancestor's. It is what `config set
// --local` writes to, so it must be able to create a nested workspace config
// rather than silently editing the enclosing one.
func TestLocalPathIsWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	outer := t.TempDir()
	writeConfigFile(t, filepath.Join(outer, ".ack-workspace", "config"), fileConfig{RepoPrefix: "outer-"})

	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m := NewManagerWithHomeAndDir(home, inner)
	want := filepath.Join(inner, ".ack-workspace", "config")
	if got := m.LocalPath(); got != want {
		t.Errorf("LocalPath() = %q, want %q", got, want)
	}
}

// TestDiscoveryIgnoresGlobalConfigReachedBySymlink is a regression test for a
// home directory whose $HOME spelling differs from its real path — $HOME set to
// /home/user while the directory actually lives at /local/home/user, which is how
// cloud desktops are commonly laid out.
//
// Comparing those two spellings literally makes the global-directory check miss,
// and the global config file is then discovered as though it were workspace-local,
// putting the workspace root at $HOME and pointing every command at the wrong
// tree.
func TestDiscoveryIgnoresGlobalConfigReachedBySymlink(t *testing.T) {
	base := t.TempDir()

	// realHome is where the directory lives; linkHome is the path $HOME uses.
	realHome := filepath.Join(base, "local", "home", "user")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	linkHome := filepath.Join(base, "home-user")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	globalPath := filepath.Join(linkHome, configDirName, configFileName)
	writeConfigFile(t, globalPath, fileConfig{GitHubUser: "octocat", WorkspaceRoot: "/tmp/global/root"})

	// $HOME is the symlinked spelling; the working directory is the real one, as
	// os.Getwd may report either.
	m := NewManagerWithHomeAndDir(linkHome, realHome)

	if got := m.Path(); got != m.GlobalPath() {
		t.Errorf("Path() = %q, want the global file %q; the global config was taken for a local one", got, m.GlobalPath())
	}

	cfg, err := m.Resolve(Source{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// The global file's own root must survive; nothing may be inferred from $HOME.
	if cfg.WorkspaceRoot != "/tmp/global/root" {
		t.Errorf("WorkspaceRoot = %q, want %q from the global file", cfg.WorkspaceRoot, "/tmp/global/root")
	}
}
