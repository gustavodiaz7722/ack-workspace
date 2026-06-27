package git

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// newRepoWithMock returns a Repo bound to a fixed path and the MockRunner
// backing it, so tests can both drive operations and inspect recorded calls.
func newRepoWithMock(path string) (*Repo, *MockRunner) {
	m := &MockRunner{}
	return NewRepo(path, m), m
}

func TestClone_RunsExpectedArgVector(t *testing.T) {
	ctx := context.Background()
	m := &MockRunner{}

	repo, err := Clone(ctx, m, "https://github.com/octocat/ack-runtime.git", "/work/runtime")
	if err != nil {
		t.Fatalf("Clone returned error: %v", err)
	}
	if repo.Path != "/work/runtime" {
		t.Errorf("expected Repo.Path %q, got %q", "/work/runtime", repo.Path)
	}

	want := [][]string{{"clone", "https://github.com/octocat/ack-runtime.git", "/work/runtime"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("clone arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestClone_PropagatesError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	m := &MockRunner{}
	m.Queue("fatal: repository not found", boom)

	if _, err := Clone(ctx, m, "https://example/none.git", "/work/none"); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped clone error, got %v", err)
	}
}

func TestSetRemote_UpdatesExistingViaSetURL(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// set-url succeeds: only one call is expected.
	m.Queue("", nil)

	if err := repo.SetRemote(ctx, "origin", "https://github.com/octocat/ack-runtime.git"); err != nil {
		t.Fatalf("SetRemote returned error: %v", err)
	}

	want := [][]string{{"remote", "set-url", "origin", "https://github.com/octocat/ack-runtime.git"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SetRemote arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestSetRemote_AddsWhenSetURLFails(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// First call (set-url) fails -> falls back to remote add (second call succeeds).
	m.Queue("error: No such remote 'upstream'", errors.New("exit status 2")).Queue("", nil)

	if err := repo.SetRemote(ctx, "upstream", "https://github.com/aws-controllers-k8s/runtime.git"); err != nil {
		t.Fatalf("SetRemote returned error: %v", err)
	}

	want := [][]string{
		{"remote", "set-url", "upstream", "https://github.com/aws-controllers-k8s/runtime.git"},
		{"remote", "add", "upstream", "https://github.com/aws-controllers-k8s/runtime.git"},
	}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SetRemote fallback arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestSetRemote_ErrorWhenBothFail(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	addErr := errors.New("add failed")
	m.Queue("", errors.New("set-url failed")).Queue("", addErr)

	if err := repo.SetRemote(ctx, "upstream", "https://example/up.git"); !errors.Is(err, addErr) {
		t.Fatalf("expected wrapped add error, got %v", err)
	}
}

func TestFetch_RunsExpectedArgVector(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")

	if err := repo.Fetch(ctx, "upstream"); err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	want := [][]string{{"fetch", "upstream"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Fetch arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestFetch_PropagatesError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	boom := errors.New("network down")
	m.Queue("fatal: unable to access", boom)

	if err := repo.Fetch(ctx, "upstream"); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped fetch error, got %v", err)
	}
}

func TestCurrentBranch_ReturnsBranchName(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// Trailing newline must be trimmed.
	m.Queue("main\n", nil)

	name, detached, err := repo.CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if detached {
		t.Error("expected not detached for a named branch")
	}
	if name != "main" {
		t.Errorf("expected branch %q, got %q", "main", name)
	}

	want := [][]string{{"symbolic-ref", "--short", "-q", "HEAD"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CurrentBranch arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestCurrentBranch_DetachedWhenEmptyOutputNoError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// symbolic-ref produced no output: detached HEAD.
	m.Queue("", nil)

	name, detached, err := repo.CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if !detached {
		t.Error("expected detached HEAD for empty output")
	}
	if name != "" {
		t.Errorf("expected empty branch name, got %q", name)
	}
}

func TestCurrentBranch_DetachedWhenExitStatusOne(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// symbolic-ref --short -q HEAD exits 1 with no output on a detached HEAD.
	m.Queue("", &ExitError{Code: 1})

	name, detached, err := repo.CurrentBranch(ctx)
	if err != nil {
		t.Fatalf("CurrentBranch returned error: %v", err)
	}
	if !detached || name != "" {
		t.Errorf("expected detached HEAD with empty name, got name=%q detached=%v", name, detached)
	}
}

func TestCurrentBranch_RealErrorSurfaced(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// A higher exit code (e.g. not a git repository) is a genuine failure.
	m.Queue("fatal: not a git repository", &ExitError{Code: 128})

	if _, _, err := repo.CurrentBranch(ctx); err == nil {
		t.Fatal("expected an error for a non-detached git failure, got nil")
	}
}

func TestIsDirty_ReportsTrueForNonEmptyPorcelain(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	m.Queue(" M internal/git/repo.go\n?? new.txt\n", nil)

	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		t.Fatalf("IsDirty returned error: %v", err)
	}
	if !dirty {
		t.Error("expected dirty working tree for non-empty porcelain output")
	}

	want := [][]string{{"status", "--porcelain"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IsDirty arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestIsDirty_ReportsFalseForEmptyPorcelain(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// Only whitespace/newlines means a clean tree.
	m.Queue("\n", nil)

	dirty, err := repo.IsDirty(ctx)
	if err != nil {
		t.Fatalf("IsDirty returned error: %v", err)
	}
	if dirty {
		t.Error("expected clean working tree for empty porcelain output")
	}
}

func TestIsDirty_PropagatesError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	boom := errors.New("status failed")
	m.Queue("", boom)

	if _, err := repo.IsDirty(ctx); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped status error, got %v", err)
	}
}

func TestAheadBehind_ParsesCounts(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// rev-list --left-right --count prints "<ahead>\t<behind>".
	m.Queue("2\t5\n", nil)

	ahead, behind, err := repo.AheadBehind(ctx, "main", "upstream/main")
	if err != nil {
		t.Fatalf("AheadBehind returned error: %v", err)
	}
	if ahead != 2 || behind != 5 {
		t.Errorf("expected ahead=2 behind=5, got ahead=%d behind=%d", ahead, behind)
	}

	want := [][]string{{"rev-list", "--left-right", "--count", "main...upstream/main"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AheadBehind arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestAheadBehind_UpToDate(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	m.Queue("0\t0\n", nil)

	ahead, behind, err := repo.AheadBehind(ctx, "main", "upstream/main")
	if err != nil {
		t.Fatalf("AheadBehind returned error: %v", err)
	}
	if ahead != 0 || behind != 0 {
		t.Errorf("expected up-to-date (0,0), got ahead=%d behind=%d", ahead, behind)
	}
}

func TestAheadBehind_UnexpectedOutput(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	m.Queue("garbage", nil)

	if _, _, err := repo.AheadBehind(ctx, "main", "upstream/main"); err == nil {
		t.Fatal("expected an error for malformed rev-list output, got nil")
	}
}

func TestAheadBehind_PropagatesError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	boom := errors.New("rev-list failed")
	m.Queue("", boom)

	if _, _, err := repo.AheadBehind(ctx, "main", "upstream/main"); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped rev-list error, got %v", err)
	}
}

func TestCanFastForward_TrueWhenAncestor(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// merge-base --is-ancestor exits 0 when local is an ancestor of upstream.
	m.Queue("", nil)

	ok, err := repo.CanFastForward(ctx, "main", "upstream/main")
	if err != nil {
		t.Fatalf("CanFastForward returned error: %v", err)
	}
	if !ok {
		t.Error("expected fast-forward possible when local is an ancestor")
	}

	want := [][]string{{"merge-base", "--is-ancestor", "main", "upstream/main"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CanFastForward arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestCanFastForward_FalseWhenNotAncestor(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// Exit status 1 specifically means "not an ancestor": (false, nil), not an error.
	m.Queue("", &ExitError{Code: 1})

	ok, err := repo.CanFastForward(ctx, "main", "upstream/main")
	if err != nil {
		t.Fatalf("expected nil error for the not-an-ancestor case, got %v", err)
	}
	if ok {
		t.Error("expected fast-forward NOT possible when local is not an ancestor")
	}
}

func TestCanFastForward_RealErrorSurfaced(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// A higher exit code (e.g. unknown revision) is a genuine error, distinct
	// from the exit-1 "not an ancestor" answer.
	m.Queue("fatal: Not a valid object name", &ExitError{Code: 128})

	ok, err := repo.CanFastForward(ctx, "main", "upstream/bogus")
	if err == nil {
		t.Fatal("expected a real error for a non-1 exit code, got nil")
	}
	if ok {
		t.Error("expected false result alongside a real error")
	}
}

func TestCanFastForward_NonExitErrorSurfaced(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	// An error with no recoverable exit code must be treated as a real failure,
	// never as "not an ancestor".
	m.Queue("", errors.New("context deadline exceeded"))

	if _, err := repo.CanFastForward(ctx, "main", "upstream/main"); err == nil {
		t.Fatal("expected a real error when no exit code is available, got nil")
	}
}

func TestFastForward_ChecksOutThenMerges(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	m.Queue("", nil).Queue("", nil)

	if err := repo.FastForward(ctx, "main", "upstream/main"); err != nil {
		t.Fatalf("FastForward returned error: %v", err)
	}

	want := [][]string{
		{"checkout", "main"},
		{"merge", "--ff-only", "upstream/main"},
	}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("FastForward arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestFastForward_PropagatesMergeError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	mergeErr := errors.New("not a fast-forward")
	// checkout succeeds, merge --ff-only fails.
	m.Queue("", nil).Queue("fatal: Not possible to fast-forward", mergeErr)

	if err := repo.FastForward(ctx, "main", "upstream/main"); !errors.Is(err, mergeErr) {
		t.Fatalf("expected wrapped merge error, got %v", err)
	}
}

func TestFastForward_PropagatesCheckoutError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	checkoutErr := errors.New("checkout failed")
	m.Queue("", checkoutErr)

	if err := repo.FastForward(ctx, "main", "upstream/main"); !errors.Is(err, checkoutErr) {
		t.Fatalf("expected wrapped checkout error, got %v", err)
	}
	// merge must not be attempted after a failed checkout.
	want := [][]string{{"checkout", "main"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected only checkout to run, got %v", got)
	}
}

func TestPush_RunsExpectedArgVector(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")

	if err := repo.Push(ctx, "origin", "main"); err != nil {
		t.Fatalf("Push returned error: %v", err)
	}

	want := [][]string{{"push", "origin", "main"}}
	if got := m.ArgVectors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Push arg vectors mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestPush_PropagatesError(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/runtime")
	boom := errors.New("permission denied")
	m.Queue("remote: Permission denied", boom)

	if err := repo.Push(ctx, "origin", "main"); !errors.Is(err, boom) {
		t.Fatalf("expected wrapped push error, got %v", err)
	}
}

// TestRepoUsesItsPathAsWorkingDir asserts that Repo methods run git in the
// repository's own directory (not the process cwd), so operations target the
// intended checkout.
func TestRepoUsesItsPathAsWorkingDir(t *testing.T) {
	ctx := context.Background()
	repo, m := newRepoWithMock("/work/code-generator")
	m.Queue("", nil)

	if _, err := repo.IsDirty(ctx); err != nil {
		t.Fatalf("IsDirty returned error: %v", err)
	}
	last, ok := m.Last()
	if !ok {
		t.Fatal("expected a recorded call")
	}
	if last.Dir != "/work/code-generator" {
		t.Errorf("expected git to run in %q, got %q", "/work/code-generator", last.Dir)
	}
}

// TestExitError_Behaviour documents the sentinel error's contract used by
// CanFastForward and CurrentBranch to interpret git exit codes.
func TestExitError_Behaviour(t *testing.T) {
	inner := errors.New("inner")
	e := &ExitError{Code: 1, Err: inner}
	if e.Error() == "" {
		t.Error("expected non-empty Error() string")
	}
	if !errors.Is(e, inner) {
		t.Error("expected ExitError to unwrap to its inner error")
	}
	code, ok := exitCodeOf(e)
	if !ok || code != 1 {
		t.Errorf("expected exitCodeOf to report (1,true), got (%d,%v)", code, ok)
	}
	if _, ok := exitCodeOf(errors.New("plain")); ok {
		t.Error("expected exitCodeOf to report false for a plain error")
	}
}
