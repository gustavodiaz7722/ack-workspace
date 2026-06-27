package cmd

import (
	"errors"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/spf13/cobra"
)

// parseArgs builds a fresh root command and parses the given argument vector so
// each test observes only the flags it set. It returns the parsed command.
func parseArgs(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := NewRootCommand()
	cmd.SetArgs(args)
	// Parse flags without executing the (empty) Run so cmd.Flags().Changed
	// reflects exactly the flags supplied on this invocation.
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v) returned error: %v", args, err)
	}
	return cmd
}

func TestBuildSource_OnlyChangedFlagsIncluded(t *testing.T) {
	// Ensure no ambient environment leaks into the env capture assertions.
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")

	cmd := parseArgs(t,
		"--"+config.FlagGitHubUser, "octocat",
		"--"+config.FlagConcurrency, "8",
	)

	src := buildSource(cmd)

	// Only the two flags that were set should be present.
	if got, want := len(src.Flags), 2; got != want {
		t.Fatalf("Source.Flags has %d entries (%v), want %d", got, src.Flags, want)
	}
	if got, ok := src.Flags[config.FlagGitHubUser]; !ok || got != "octocat" {
		t.Errorf("Source.Flags[%q] = %q (present=%v), want %q", config.FlagGitHubUser, got, ok, "octocat")
	}
	if got, ok := src.Flags[config.FlagConcurrency]; !ok || got != "8" {
		t.Errorf("Source.Flags[%q] = %q (present=%v), want %q", config.FlagConcurrency, got, ok, "8")
	}

	// Flags that were not set must be absent so they do not override persisted
	// or environment values (Requirement 2.4).
	for _, name := range []string{
		config.FlagWorkspaceRoot,
		config.FlagRepoPrefix,
		config.FlagToken,
	} {
		if _, ok := src.Flags[name]; ok {
			t.Errorf("Source.Flags contains unset flag %q", name)
		}
	}
}

func TestBuildSource_NoFlagsSet(t *testing.T) {
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")

	cmd := parseArgs(t)

	src := buildSource(cmd)

	if len(src.Flags) != 0 {
		t.Errorf("Source.Flags = %v, want empty when no flags set", src.Flags)
	}
}

func TestBuildSource_EnvCaptured(t *testing.T) {
	t.Setenv(config.EnvGitHubUser, "envuser")
	t.Setenv(config.EnvToken, "envtoken")

	cmd := parseArgs(t)

	src := buildSource(cmd)

	if got := src.Env[config.EnvGitHubUser]; got != "envuser" {
		t.Errorf("Source.Env[%q] = %q, want %q", config.EnvGitHubUser, got, "envuser")
	}
	if got := src.Env[config.EnvToken]; got != "envtoken" {
		t.Errorf("Source.Env[%q] = %q, want %q", config.EnvToken, got, "envtoken")
	}
}

func TestBuildSource_EmptyEnvNotCaptured(t *testing.T) {
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")

	cmd := parseArgs(t)

	src := buildSource(cmd)

	if _, ok := src.Env[config.EnvGitHubUser]; ok {
		t.Errorf("Source.Env should not contain empty %q", config.EnvGitHubUser)
	}
	if _, ok := src.Env[config.EnvToken]; ok {
		t.Errorf("Source.Env should not contain empty %q", config.EnvToken)
	}
}

func TestValidateConcurrency(t *testing.T) {
	cases := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{name: "reject zero", value: 0, wantErr: true},
		{name: "reject negative", value: -1, wantErr: true},
		{name: "reject above max", value: 33, wantErr: true},
		{name: "accept min", value: 1, wantErr: false},
		{name: "accept mid", value: 4, wantErr: false},
		{name: "accept max", value: 32, wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConcurrency(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateConcurrency(%d) = nil, want error", tc.value)
				}
				var ue *UsageError
				if !errors.As(err, &ue) {
					t.Fatalf("validateConcurrency(%d) error type = %T, want *UsageError", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConcurrency(%d) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestBuildApp_RejectsOutOfRangeConcurrency(t *testing.T) {
	// Supply an identity so configuration resolution succeeds regardless of
	// whether a persisted config file exists, isolating the concurrency check.
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")

	cmd := parseArgs(t,
		"--"+config.FlagGitHubUser, "octocat",
		"--"+config.FlagConcurrency, "33",
	)

	_, err := buildApp(cmd)
	if err == nil {
		t.Fatal("buildApp with concurrency 33 = nil error, want *UsageError")
	}
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("buildApp error type = %T, want *UsageError", err)
	}
}

func TestBuildApp_AcceptsInRangeConcurrency(t *testing.T) {
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")

	for _, n := range []string{"1", "4", "32"} {
		cmd := parseArgs(t,
			"--"+config.FlagGitHubUser, "octocat",
			"--"+config.FlagConcurrency, n,
		)

		a, err := buildApp(cmd)
		if err != nil {
			t.Fatalf("buildApp with concurrency %s = %v, want nil", n, err)
		}
		if a.GitHub == nil {
			t.Errorf("buildApp(%s) App.GitHub is nil, want a constructed adapter", n)
		}
		if a.Git == nil {
			t.Errorf("buildApp(%s) App.Git is nil, want a constructed runner", n)
		}
		if a.Config.GitHubUser != "octocat" {
			t.Errorf("buildApp(%s) App.Config.GitHubUser = %q, want %q", n, a.Config.GitHubUser, "octocat")
		}
	}
}
