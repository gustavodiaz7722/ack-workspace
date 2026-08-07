package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws-controllers-k8s/ack-workspace/internal/adder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/attributor"
	"github.com/aws-controllers-k8s/ack-workspace/internal/builder"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/prereq"
	"github.com/aws-controllers-k8s/ack-workspace/internal/releaser"
	"github.com/aws-controllers-k8s/ack-workspace/internal/remover"
	"github.com/aws-controllers-k8s/ack-workspace/internal/scanner"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// fakeChecker is a stubbed prereq.Checker that records the Need it was asked to
// evaluate and returns a scripted error. It lets the command tests assert that
// each command declares the correct prerequisites and that a failing check
// stops the command before it delegates, all without depending on the real
// PATH, token, or identity.
type fakeChecker struct {
	called  bool
	gotNeed prereq.Need
	err     error
}

func (f *fakeChecker) Check(need prereq.Need, cfg config.Config) error {
	f.called = true
	f.gotNeed = need
	return f.err
}

// recorder captures whether each fake component runner was invoked and with what
// arguments, so the tests can assert the commands parse positional args and
// flags correctly and delegate. The runErr and summary fields let a test script
// a component's return.
type recorder struct {
	initCalled bool

	addCalled bool
	addIDs    []string

	refreshCalled bool
	refreshOnly   []string

	statusCalled bool
	statusJSON   bool

	removeCalled bool
	removeIDs    []string
	removeOpts   remover.Options

	releaseCalled  bool
	releaseService string
	releaseVersion string
	releaseBase    string
	releaseSkipPR  bool
	releasePRBody  string

	buildCalled     bool
	buildService    string
	buildSDKVersion string

	attributionCalled bool
	attributionIDs    []string
	attributionOpts   attributor.Options
	attributionRegion string

	candidatesCalled bool
	candidatesOpts   scanner.CandidatesOptions

	summary workspace.Summary
	runErr  error
}

// fakeDeps builds a deps wired to the given checker and a recorder so command
// execution performs no git or GitHub work.
func fakeDeps(chk prereq.Checker, rec *recorder) deps {
	return deps{
		checker: chk,
		initRun: func(ctx context.Context, a app.App) (workspace.Summary, error) {
			rec.initCalled = true
			return rec.summary, rec.runErr
		},
		addRun: func(ctx context.Context, a app.App, identifiers []string) (workspace.Summary, error) {
			rec.addCalled = true
			rec.addIDs = identifiers
			return rec.summary, rec.runErr
		},
		refreshRun: func(ctx context.Context, a app.App, only []string) (workspace.Summary, error) {
			rec.refreshCalled = true
			rec.refreshOnly = only
			return rec.summary, rec.runErr
		},
		statusRun: func(ctx context.Context, a app.App, jsonOut bool, out io.Writer) (workspace.Summary, error) {
			rec.statusCalled = true
			rec.statusJSON = jsonOut
			return rec.summary, rec.runErr
		},
		candidatesRun: func(ctx context.Context, a app.App, opts scanner.CandidatesOptions, out, errOut io.Writer) (workspace.Summary, error) {
			rec.candidatesCalled = true
			rec.candidatesOpts = opts
			return rec.summary, rec.runErr
		},
		removeRun: func(ctx context.Context, a app.App, identifiers []string, opts remover.Options) (workspace.Summary, error) {
			rec.removeCalled = true
			rec.removeIDs = identifiers
			rec.removeOpts = opts
			return rec.summary, rec.runErr
		},
		releaseRun: func(ctx context.Context, a app.App, service, version, baseBranch string, skipPR bool, prBody string) (workspace.Summary, error) {
			rec.releaseCalled = true
			rec.releaseService = service
			rec.releaseVersion = version
			rec.releaseBase = baseBranch
			rec.releaseSkipPR = skipPR
			rec.releasePRBody = prBody
			return rec.summary, rec.runErr
		},
		buildRun: func(ctx context.Context, a app.App, service, sdkVersion string) (workspace.Summary, error) {
			rec.buildCalled = true
			rec.buildService = service
			rec.buildSDKVersion = sdkVersion
			return rec.summary, rec.runErr
		},
		attributionRun: func(ctx context.Context, a app.App, identifiers []string, opts attributor.Options, region string, out io.Writer) (workspace.Summary, error) {
			rec.attributionCalled = true
			rec.attributionIDs = identifiers
			rec.attributionOpts = opts
			rec.attributionRegion = region
			return rec.summary, rec.runErr
		},
	}
}

// isolateEnv points $HOME at a temporary directory and clears the identity/token
// environment variables so configuration resolution is deterministic and never
// reads a real config file. It returns the temporary home.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.EnvGitHubUser, "")
	t.Setenv(config.EnvToken, "")
	return home
}

// runCmd builds a root command with the given deps, runs it with args, and
// returns the populated Result, the captured stdout, and the execution error.
func runCmd(t *testing.T, d deps, args ...string) (*Result, string, error) {
	t.Helper()
	cmd, res := newRootCmd(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return res, out.String(), err
}

// --- Prerequisite Need wiring -------------------------------------------------

func TestPrerequisiteNeedPerCommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want prereq.Need
	}{
		{
			name: "init needs git+token+identity",
			args: []string{"init", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"},
			want: prereq.Need{Git: true, Token: true, Identity: true},
		},
		{
			name: "add needs git+token+identity",
			args: []string{"add", "s3", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"},
			want: prereq.Need{Git: true, Token: true, Identity: true},
		},
		{
			name: "refresh needs git+token+identity",
			args: []string{"refresh", "--yes", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"},
			want: prereq.Need{Git: true, Token: true, Identity: true},
		},
		{
			name: "status needs git only",
			args: []string{"status", "--" + config.FlagGitHubUser, "octocat"},
			want: prereq.Need{Git: true},
		},
		{
			// attribution reads the checked-out branch and lists remote refs, so it
			// needs git. It must NOT declare Identity: --upstream and --repo remove
			// the need for a fork owner, and declaring it would block those paths.
			name: "attribution needs git only",
			args: []string{"attribution", "ecr", "--" + config.FlagGitHubUser, "octocat"},
			want: prereq.Need{Git: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			chk := &fakeChecker{}
			rec := &recorder{}
			_, _, err := runCmd(t, fakeDeps(chk, rec), tc.args...)
			if err != nil {
				t.Fatalf("execute returned error: %v", err)
			}
			if !chk.called {
				t.Fatal("prerequisite checker was not called")
			}
			if chk.gotNeed != tc.want {
				t.Errorf("Need = %+v, want %+v", chk.gotNeed, tc.want)
			}
		})
	}
}

func TestConfigCommandsSkipPrerequisiteCheck(t *testing.T) {
	for _, args := range [][]string{
		{"config", "path"},
		{"config", "get", "--" + config.FlagGitHubUser, "octocat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateEnv(t)
			chk := &fakeChecker{}
			rec := &recorder{}
			if _, _, err := runCmd(t, fakeDeps(chk, rec), args...); err != nil {
				t.Fatalf("execute returned error: %v", err)
			}
			if chk.called {
				t.Error("config command unexpectedly invoked the prerequisite checker")
			}
		})
	}
}

// --- Prerequisite enforcement (failing check stops delegation) ---------------

func TestFailingPrerequisiteStopsDelegation(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wasCalled func(*recorder) bool
	}{
		{"init", []string{"init", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"}, func(r *recorder) bool { return r.initCalled }},
		{"add", []string{"add", "s3", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"}, func(r *recorder) bool { return r.addCalled }},
		{"refresh", []string{"refresh", "--yes", "--" + config.FlagGitHubUser, "octocat", "--" + config.FlagToken, "tok"}, func(r *recorder) bool { return r.refreshCalled }},
		{"status", []string{"status", "--" + config.FlagGitHubUser, "octocat"}, func(r *recorder) bool { return r.statusCalled }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			wantErr := &prereq.MissingError{Missing: []string{"git: not found"}}
			chk := &fakeChecker{err: wantErr}
			rec := &recorder{}

			res, _, err := runCmd(t, fakeDeps(chk, rec), tc.args...)
			if err == nil {
				t.Fatal("expected the prerequisite error to propagate, got nil")
			}
			var me *prereq.MissingError
			if !errors.As(err, &me) {
				t.Fatalf("error type = %T, want *prereq.MissingError", err)
			}
			if tc.wasCalled(rec) {
				t.Error("component was delegated to despite a failing prerequisite check")
			}
			if _, ok := res.Summary(); ok {
				t.Error("Result should hold no summary when the prerequisite check fails")
			}
		})
	}
}

// --- Delegation, argument and flag parsing -----------------------------------

func TestInitDelegatesAndStashesSummary(t *testing.T) {
	isolateEnv(t)
	chk := &fakeChecker{}
	rec := &recorder{summary: workspace.Summary{Results: []workspace.Result{
		{Repo: "runtime", Outcome: workspace.OutcomeCreated},
	}}}

	res, _, err := runCmd(t, fakeDeps(chk, rec),
		"init", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.initCalled {
		t.Fatal("initRun was not called")
	}
	summary, ok := res.Summary()
	if !ok {
		t.Fatal("Result holds no summary after a successful init")
	}
	if got := summary.Count(workspace.OutcomeCreated); got != 1 {
		t.Errorf("created count = %d, want 1", got)
	}
}

func TestAddParsesIdentifiers(t *testing.T) {
	isolateEnv(t)
	chk := &fakeChecker{}
	rec := &recorder{}

	if _, _, err := runCmd(t, fakeDeps(chk, rec),
		"add", "s3", "sns", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok"); err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.addCalled {
		t.Fatal("addRun was not called")
	}
	want := []string{"s3", "sns"}
	if len(rec.addIDs) != len(want) || rec.addIDs[0] != "s3" || rec.addIDs[1] != "sns" {
		t.Errorf("addRun identifiers = %v, want %v", rec.addIDs, want)
	}
}

func TestAddEmptyIdentifierListSurfacesUsageError(t *testing.T) {
	isolateEnv(t)
	// Use the real Controller_Adder so the empty-list rule (Requirement 4.2) is
	// enforced where it lives. A passing checker isolates the adder's behavior.
	d := defaultDeps()
	d.checker = &fakeChecker{}

	_, _, err := runCmd(t, d,
		"add", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err == nil {
		t.Fatal("expected a usage error for an empty identifier list, got nil")
	}
	var ue *adder.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *adder.UsageError", err)
	}
}

func TestRefreshParsesSubsetWithConfirmation(t *testing.T) {
	t.Run("subset confirmed", func(t *testing.T) {
		isolateEnv(t)
		rec := &recorder{}
		if _, _, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "yes\n",
			"refresh", "runtime", "s3-controller", "--"+config.FlagGitHubUser, "octocat"); err != nil {
			t.Fatalf("execute returned error: %v", err)
		}
		if !rec.refreshCalled {
			t.Fatal("refreshRun was not called")
		}
		if len(rec.refreshOnly) != 2 || rec.refreshOnly[0] != "runtime" || rec.refreshOnly[1] != "s3-controller" {
			t.Errorf("refreshRun only = %v, want [runtime s3-controller]", rec.refreshOnly)
		}
	})

	t.Run("no args means all", func(t *testing.T) {
		isolateEnv(t)
		rec := &recorder{}
		if _, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
			"refresh", "--yes", "--"+config.FlagGitHubUser, "octocat"); err != nil {
			t.Fatalf("execute returned error: %v", err)
		}
		if !rec.refreshCalled {
			t.Fatal("refreshRun was not called")
		}
		if len(rec.refreshOnly) != 0 {
			t.Errorf("refreshRun only = %v, want empty", rec.refreshOnly)
		}
	})
}

func TestRefreshAbortsWhenNotConfirmed(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, out, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "no\n",
		"refresh", "--"+config.FlagGitHubUser, "octocat")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if rec.refreshCalled {
		t.Error("refreshRun must not be called when the user declines")
	}
	if !strings.Contains(out, "Aborted") {
		t.Errorf("expected an abort message, got %q", out)
	}
}

func TestRefreshDryRunDoesNotPrompt(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	// No stdin provided; dry-run must not prompt and must still delegate.
	if _, _, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "",
		"refresh", "--dry-run", "--"+config.FlagGitHubUser, "octocat"); err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.refreshCalled {
		t.Fatal("refreshRun was not called in dry-run")
	}
}

func TestStatusParsesJSONFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"json", []string{"status", "--json", "--" + config.FlagGitHubUser, "octocat"}, true},
		{"table", []string{"status", "--" + config.FlagGitHubUser, "octocat"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			chk := &fakeChecker{}
			rec := &recorder{}
			if _, _, err := runCmd(t, fakeDeps(chk, rec), tc.args...); err != nil {
				t.Fatalf("execute returned error: %v", err)
			}
			if !rec.statusCalled {
				t.Fatal("statusRun was not called")
			}
			if rec.statusJSON != tc.wantJSON {
				t.Errorf("statusRun jsonOut = %v, want %v", rec.statusJSON, tc.wantJSON)
			}
		})
	}
}

// --- config set/get/path ------------------------------------------------------

func TestConfigPathPrintsManagerPath(t *testing.T) {
	home := isolateEnv(t)
	_, out, err := runCmd(t, fakeDeps(&fakeChecker{}, &recorder{}), "config", "path")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	want := config.NewManagerWithHome(home).Path()
	if strings.TrimSpace(out) != want {
		t.Errorf("config path printed %q, want %q", strings.TrimSpace(out), want)
	}
}

func TestConfigSetPersistsAndGetReadsBack(t *testing.T) {
	isolateEnv(t)
	d := fakeDeps(&fakeChecker{}, &recorder{})

	// set persists the identity supplied via flag.
	if _, _, err := runCmd(t, d,
		"config", "set", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagRepoPrefix, "myfork-"); err != nil {
		t.Fatalf("config set returned error: %v", err)
	}

	// get, with no flags, must read the persisted values back.
	_, out, err := runCmd(t, d, "config", "get")
	if err != nil {
		t.Fatalf("config get returned error: %v", err)
	}
	if !strings.Contains(out, "github-user:    octocat") {
		t.Errorf("config get output missing persisted identity:\n%s", out)
	}
	if !strings.Contains(out, "prefix:         myfork-") {
		t.Errorf("config get output missing persisted prefix:\n%s", out)
	}
}

func TestConfigGetMissingIdentityErrors(t *testing.T) {
	// No config file and no identity supplied: resolution fails (Requirement 2.7).
	isolateEnv(t)
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, &recorder{}), "config", "get")
	if err == nil {
		t.Fatal("expected a missing-identity error, got nil")
	}
	var mu *config.MissingGitHubUserError
	if !errors.As(err, &mu) {
		t.Fatalf("error type = %T, want *config.MissingGitHubUserError", err)
	}
}

// Guard: the root command exposes the expected subcommands.
func TestRootRegistersSubcommands(t *testing.T) {
	cmd := NewRootCommand()
	want := map[string]bool{"init": false, "add": false, "refresh": false, "status": false, "remove": false, "release": false, "deploy": false, "build": false, "scan": false, "candidates": false, "attribution": false, "config": false}
	for _, c := range cmd.Commands() {
		name := strings.Fields(c.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("root command missing subcommand %q", name)
		}
	}
}

// --- remove command ----------------------------------------------------------

// runCmdIn is like runCmd but also wires an input reader so the confirmation
// prompt can be driven from a test.
func runCmdIn(t *testing.T, d deps, stdin string, args ...string) (*Result, string, error) {
	t.Helper()
	cmd, res := newRootCmd(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return res, out.String(), err
}

func TestRemove_NeedsGitTokenIdentity(t *testing.T) {
	isolateEnv(t)
	chk := &fakeChecker{}
	rec := &recorder{}
	// --yes avoids the confirmation prompt so we isolate the prereq wiring.
	_, _, err := runCmd(t, fakeDeps(chk, rec),
		"remove", "s3", "--yes", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !chk.called {
		t.Fatal("prerequisite checker was not called")
	}
	want := prereq.Need{Git: true, Token: true, Identity: true}
	if chk.gotNeed != want {
		t.Errorf("Need = %+v, want %+v", chk.gotNeed, want)
	}
	if !rec.removeCalled {
		t.Error("removeRun was not called")
	}
	if len(rec.removeIDs) != 1 || rec.removeIDs[0] != "s3" {
		t.Errorf("removeRun identifiers = %v, want [s3]", rec.removeIDs)
	}
}

func TestRemove_ConfirmationAbortsWithoutYes(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	// User answers "no" at the prompt; the component must not be invoked.
	_, out, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "no\n",
		"remove", "s3", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if rec.removeCalled {
		t.Error("removeRun must not be called when the user declines confirmation")
	}
	if !strings.Contains(out, "Aborted") {
		t.Errorf("expected an abort message, got:\n%s", out)
	}
}

func TestRemove_ConfirmationProceedsOnYes(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, _, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "yes\n",
		"remove", "s3", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.removeCalled {
		t.Error("removeRun should be called after the user confirms")
	}
}

func TestRemove_DryRunSkipsConfirmation(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	// No stdin provided; dry-run must not prompt and must still delegate.
	_, _, err := runCmdIn(t, fakeDeps(&fakeChecker{}, rec), "",
		"remove", "s3", "--dry-run", "--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.removeCalled {
		t.Error("dry-run remove should delegate without a confirmation prompt")
	}
}

func TestRemove_ParsesKeepForkAndForce(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"remove", "all", "--yes", "--keep-fork", "--force",
		"--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.removeOpts.KeepFork || !rec.removeOpts.Force {
		t.Errorf("expected KeepFork and Force set, got %+v", rec.removeOpts)
	}
	if len(rec.removeIDs) != 1 || rec.removeIDs[0] != "all" {
		t.Errorf("removeRun identifiers = %v, want [all]", rec.removeIDs)
	}
}

// --- release command ---------------------------------------------------------

func TestRelease_NeedsGitTokenIdentity(t *testing.T) {
	isolateEnv(t)
	chk := &fakeChecker{}
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(chk, rec),
		"release", "ecr", "--version", "v1.0.1",
		"--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !chk.called {
		t.Fatal("prerequisite checker was not called")
	}
	want := prereq.Need{Git: true, Token: true, Identity: true}
	if chk.gotNeed != want {
		t.Errorf("Need = %+v, want %+v", chk.gotNeed, want)
	}
}

func TestRelease_ParsesServiceAndFlags(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"release", "ecr-controller", "--version", "v1.2.0", "--base-branch", "release-1.x", "--skip-pr",
		"--pr-body", "custom body",
		"--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.releaseCalled {
		t.Fatal("releaseRun was not called")
	}
	if rec.releaseService != "ecr-controller" {
		t.Errorf("service = %q, want ecr-controller", rec.releaseService)
	}
	if rec.releaseVersion != "v1.2.0" {
		t.Errorf("version = %q, want v1.2.0", rec.releaseVersion)
	}
	if rec.releaseBase != "release-1.x" {
		t.Errorf("base branch = %q, want release-1.x", rec.releaseBase)
	}
	if !rec.releaseSkipPR {
		t.Error("skipPR = false, want true")
	}
	if rec.releasePRBody != "custom body" {
		t.Errorf("pr body = %q, want \"custom body\"", rec.releasePRBody)
	}
}

func TestRelease_MissingVersionSurfacesUsageError(t *testing.T) {
	isolateEnv(t)
	// Use the real Controller_Releaser so the version-required rule is enforced
	// where it lives. A passing checker isolates the releaser's behavior.
	d := defaultDeps()
	d.checker = &fakeChecker{}

	_, _, err := runCmd(t, d,
		"release", "ecr",
		"--"+config.FlagGitHubUser, "octocat", "--"+config.FlagToken, "tok")
	if err == nil {
		t.Fatal("expected a usage error for a missing version, got nil")
	}
	var ue *releaser.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *releaser.UsageError", err)
	}
}

// --- build command -----------------------------------------------------------

func TestBuild_NeedsGitOnly(t *testing.T) {
	isolateEnv(t)
	chk := &fakeChecker{}
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(chk, rec),
		"build", "ecr", "--"+config.FlagGitHubUser, "octocat")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !chk.called {
		t.Fatal("prerequisite checker was not called")
	}
	want := prereq.Need{Git: true}
	if chk.gotNeed != want {
		t.Errorf("Need = %+v, want %+v", chk.gotNeed, want)
	}
	if !rec.buildCalled {
		t.Error("buildRun was not called")
	}
	if rec.buildService != "ecr" {
		t.Errorf("service = %q, want ecr", rec.buildService)
	}
}

func TestBuild_ParsesServiceAndSDKVersion(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"build", "ecr-controller", "--"+flagSDKVersion, "v1.41.0",
		"--"+config.FlagGitHubUser, "octocat")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.buildCalled {
		t.Fatal("buildRun was not called")
	}
	if rec.buildService != "ecr-controller" {
		t.Errorf("service = %q, want ecr-controller", rec.buildService)
	}
	if rec.buildSDKVersion != "v1.41.0" {
		t.Errorf("sdk version = %q, want v1.41.0", rec.buildSDKVersion)
	}
}

func TestBuild_EmptyServiceSurfacesUsageError(t *testing.T) {
	isolateEnv(t)
	// Use the real Controller_Builder so the service-required rule is enforced
	// where it lives. A passing checker isolates the builder's behavior.
	d := defaultDeps()
	d.checker = &fakeChecker{}

	_, _, err := runCmd(t, d, "build", "--"+config.FlagGitHubUser, "octocat")
	if err == nil {
		t.Fatal("expected a usage error for a missing service, got nil")
	}
	var ue *builder.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *builder.UsageError", err)
	}
}

// --- attribution command -----------------------------------------------------

func TestAttribution_ParsesIdentifiersAndFlags(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"attribution", "ecr", "s3-controller",
		"--"+flagRef, "pr/42",
		"--"+flagUpstream,
		"--"+flagRegion, "us-west-2",
		"--"+flagProject, "my-project",
		"--"+flagBucket, "my-bucket",
		"--"+flagRole, "my-role",
		"--"+flagImage, "aws/codebuild/standard:8.0",
		"--"+flagGoVersion, "1.24",
		"--"+flagTimeout, "5m",
		"--"+config.FlagGitHubUser, "octocat",
	)
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if !rec.attributionCalled {
		t.Fatal("attribution command did not delegate")
	}
	if got := strings.Join(rec.attributionIDs, ","); got != "ecr,s3-controller" {
		t.Errorf("identifiers = %q, want \"ecr,s3-controller\"", got)
	}
	if rec.attributionRegion != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", rec.attributionRegion)
	}

	opts := rec.attributionOpts
	if opts.Ref != "pr/42" {
		t.Errorf("ref = %q, want pr/42 (normalization belongs to the component)", opts.Ref)
	}
	if !opts.Upstream {
		t.Error("upstream flag was not passed through")
	}
	if opts.Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", opts.Timeout)
	}
	if opts.Infra.Project != "my-project" || opts.Infra.Bucket != "my-bucket" || opts.Infra.Role != "my-role" {
		t.Errorf("infrastructure overrides not passed through: %+v", opts.Infra)
	}
	if opts.Infra.Image != "aws/codebuild/standard:8.0" || opts.Infra.GoVersion != "1.24" {
		t.Errorf("image/go-version overrides not passed through: %+v", opts.Infra)
	}
}

func TestAttribution_DefaultsLeaveInfrastructureEmpty(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{}
	if _, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"attribution", "ecr", "--"+config.FlagGitHubUser, "octocat"); err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	// Unset overrides must stay empty so the component (not the flag defaults)
	// owns the defaulting, keeping one source of truth for the names.
	if opts := rec.attributionOpts; opts.Infra != (attributor.Infrastructure{}) {
		t.Errorf("Infrastructure = %+v, want zero value when no overrides are set", opts.Infra)
	}
}

func TestAttribution_UsesGeneratedLabelForSummary(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{summary: workspace.Summary{Results: []workspace.Result{
		{Repo: "ecr-controller", Outcome: workspace.OutcomeCreated, Reason: "updated"},
	}}}
	res, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"attribution", "ecr", "--"+config.FlagGitHubUser, "octocat")
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if res.CreatedLabel() != "generated" {
		t.Errorf("created label = %q, want \"generated\"", res.CreatedLabel())
	}
}

func TestAttribution_EmptyIdentifierListSurfacesUsageError(t *testing.T) {
	isolateEnv(t)
	rec := &recorder{runErr: &attributor.UsageError{Msg: "at least one service identifier (or 'all') is required"}}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"attribution", "--"+config.FlagGitHubUser, "octocat")
	var ue *attributor.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *attributor.UsageError", err, err)
	}
}

// --- candidates command ------------------------------------------------------

// Defaults: with no argument and no flags the command indexes every resource of
// every controller, so an audit can be started without naming anything.
func TestCandidatesCommandDefaultsToAll(t *testing.T) {
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec), "candidates")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.candidatesCalled {
		t.Fatal("candidatesRun was not called")
	}
	if rec.candidatesOpts.Controller != scanner.All {
		t.Errorf("Controller = %q, want %q", rec.candidatesOpts.Controller, scanner.All)
	}
	if rec.candidatesOpts.Resource != scanner.All {
		t.Errorf("Resource = %q, want %q", rec.candidatesOpts.Resource, scanner.All)
	}
	if rec.candidatesOpts.OutDir != "" {
		t.Errorf("OutDir = %q, want empty by default", rec.candidatesOpts.OutDir)
	}
}

func TestCandidatesCommandPassesSelectors(t *testing.T) {
	rec := &recorder{}
	_, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec),
		"candidates", "eks", "--resource", "Nodegroup", "--out-dir", "/tmp/idx")
	if err != nil {
		t.Fatal(err)
	}
	got := rec.candidatesOpts
	if got.Controller != "eks" || got.Resource != "Nodegroup" || got.OutDir != "/tmp/idx" {
		t.Errorf("options = %+v, want {eks Nodegroup /tmp/idx}", got)
	}
}

// The command declares no prerequisites: it reads local repositories and the
// public API models, so it must run without git, a GitHub token, or AWS
// credentials.
func TestCandidatesCommandDeclaresNoPrerequisites(t *testing.T) {
	rec := &recorder{}
	chk := &fakeChecker{}
	if _, _, err := runCmd(t, fakeDeps(chk, rec), "candidates", "eks"); err != nil {
		t.Fatal(err)
	}
	if !chk.called {
		t.Fatal("prerequisite check was not run")
	}
	if chk.gotNeed != (prereq.Need{}) {
		t.Errorf("candidates declared prerequisites %+v, want none", chk.gotNeed)
	}
}

// More than one positional argument is a usage error: the controller selector is
// a single value (or "all").
func TestCandidatesCommandRejectsExtraArgs(t *testing.T) {
	rec := &recorder{}
	if _, _, err := runCmd(t, fakeDeps(&fakeChecker{}, rec), "candidates", "eks", "s3"); err == nil {
		t.Error("expected an error for two controller arguments")
	}
}
