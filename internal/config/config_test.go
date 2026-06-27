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
