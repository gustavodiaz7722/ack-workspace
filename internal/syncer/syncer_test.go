package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// defaultBranch is the branch CurrentBranch reports in tests; "upstream/main"
// is therefore the upstream ref the syncer compares and fast-forwards against.
const defaultBranch = "main"

// newApp builds an App context for tests with the given workspace root and git
// runner. Concurrency is 1 so the shared MockRunner is driven serially and its
// recorded Calls slice is safe to assert against.
func newApp(root string, runner git.Runner) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    "octocat",
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   1,
		},
		Git: runner,
	}
}

// makeRepo creates a discoverable managed repository directory (a directory
// containing a ".git" entry) named name under root.
func makeRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("setup repo %q: %v", name, err)
	}
	return dir
}

// scriptOptions configures the scripted git behavior for a repository keyed by
// its working directory.
type scriptOptions struct {
	fetchErr   error
	dirty      bool
	cannotFF   bool // merge-base --is-ancestor exits 1 => not fast-forwardable
	detached   bool // symbolic-ref reports a detached HEAD
	mergeErr   error
	pushErr    error
	currentErr error
}

// scriptedRunner returns a MockRunner whose ResponseFunc scripts git output per
// repository directory using the supplied options map (keyed by absolute repo
// path). Subcommands not configured succeed with empty output.
func scriptedRunner(opts map[string]scriptOptions) *git.MockRunner {
	r := &git.MockRunner{}
	r.ResponseFunc = func(dir string, args []string) (string, error) {
		o := opts[dir]
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "fetch":
			return "", o.fetchErr
		case "symbolic-ref":
			if o.currentErr != nil {
				return "", o.currentErr
			}
			if o.detached {
				// `symbolic-ref -q` exits 1 with no output on a detached HEAD.
				return "", &git.ExitError{Code: 1}
			}
			return defaultBranch + "\n", nil
		case "status":
			if o.dirty {
				return " M file.go\n", nil
			}
			return "", nil
		case "merge-base":
			if o.cannotFF {
				// Exit status 1 specifically means "not an ancestor".
				return "", &git.ExitError{Code: 1}
			}
			return "", nil
		case "merge":
			return "", o.mergeErr
		case "push":
			return "", o.pushErr
		default:
			// checkout and any other read-only/no-op commands succeed.
			return "", nil
		}
	}
	return r
}

// resultsByRepo indexes a Summary's Results by repository name.
func resultsByRepo(s workspace.Summary) map[string]workspace.Result {
	out := make(map[string]workspace.Result, len(s.Results))
	for _, r := range s.Results {
		out[r.Repo] = r
	}
	return out
}

// callsInDir returns the recorded git arg vectors that ran in dir.
func callsInDir(runner *git.MockRunner, dir string) [][]string {
	var out [][]string
	for _, c := range runner.Calls {
		if c.Dir == dir {
			out = append(out, c.Args)
		}
	}
	return out
}

// assertNoSubcommand fails the test if any recorded call in dir begins with sub.
func assertNoSubcommand(t *testing.T, runner *git.MockRunner, dir, sub string) {
	t.Helper()
	for _, args := range callsInDir(runner, dir) {
		if len(args) > 0 && args[0] == sub {
			t.Fatalf("expected no git %q in %s, but it was issued: %v", sub, dir, args)
		}
	}
}

// findCall returns the first recorded call in dir whose arg vector begins with
// the given prefix, and whether one was found.
func findCall(runner *git.MockRunner, dir string, prefix ...string) ([]string, bool) {
	for _, args := range callsInDir(runner, dir) {
		if len(args) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if args[i] != p {
				match = false
				break
			}
		}
		if match {
			return args, true
		}
	}
	return nil, false
}

func TestSync_FetchFailure(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {fetchErr: errors.New("network unreachable")},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("expected failed, got %s (%+v)", res.Outcome, res)
	}
	if !strings.Contains(res.Reason, "fetch") {
		t.Errorf("expected reason to describe fetch failure, got %q", res.Reason)
	}
	// A fetch failure must leave the branch untouched: no merge or push.
	assertNoSubcommand(t, runner, dir, "merge")
	assertNoSubcommand(t, runner, dir, "push")
	assertNoSubcommand(t, runner, dir, "checkout")
}

func TestSync_DirtySkip(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {dirty: true},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped, got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "uncommitted changes" {
		t.Errorf("expected reason %q, got %q", "uncommitted changes", res.Reason)
	}
	// A dirty repo must touch nothing: no merge-base, merge, checkout, or push.
	assertNoSubcommand(t, runner, dir, "merge-base")
	assertNoSubcommand(t, runner, dir, "merge")
	assertNoSubcommand(t, runner, dir, "checkout")
	assertNoSubcommand(t, runner, dir, "push")
}

func TestSync_DivergedSkip(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {cannotFF: true},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped, got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "diverged history" {
		t.Errorf("expected reason %q, got %q", "diverged history", res.Reason)
	}
	// The decisive check must have run against the documented upstream ref.
	mb, ok := findCall(runner, dir, "merge-base", "--is-ancestor", defaultBranch, "upstream/"+defaultBranch)
	if !ok {
		t.Fatalf("expected merge-base --is-ancestor %s upstream/%s, calls=%v", defaultBranch, defaultBranch, callsInDir(runner, dir))
	}
	_ = mb
	// A diverged repo's branch must be left unchanged: no merge or push.
	assertNoSubcommand(t, runner, dir, "merge")
	assertNoSubcommand(t, runner, dir, "checkout")
	assertNoSubcommand(t, runner, dir, "push")
}

func TestSync_SuccessfulFastForward(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {}, // clean, fast-forwardable
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected updated (created), got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "updated" {
		t.Errorf("expected reason %q, got %q", "updated", res.Reason)
	}
	// The fast-forward must target the documented upstream ref.
	if _, ok := findCall(runner, dir, "merge", "--ff-only", "upstream/"+defaultBranch); !ok {
		t.Fatalf("expected merge --ff-only upstream/%s, calls=%v", defaultBranch, callsInDir(runner, dir))
	}
	// push was not requested.
	assertNoSubcommand(t, runner, dir, "push")
}

func TestSync_SubsetSelection(t *testing.T) {
	root := t.TempDir()
	dirA := makeRepo(t, root, "alpha")
	dirB := makeRepo(t, root, "beta")
	makeRepo(t, root, "gamma")
	runner := scriptedRunner(map[string]scriptOptions{
		dirA: {},
		dirB: {},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), []string{"alpha"}, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	if len(sum.Results) != 1 {
		t.Fatalf("expected exactly 1 result for subset, got %d (%+v)", len(sum.Results), sum.Results)
	}
	if sum.Results[0].Repo != "alpha" {
		t.Fatalf("expected only alpha processed, got %q", sum.Results[0].Repo)
	}
	// beta and gamma must not have been touched at all.
	if len(callsInDir(runner, dirB)) != 0 {
		t.Errorf("expected no git calls in beta, got %v", callsInDir(runner, dirB))
	}
	if len(callsInDir(runner, filepath.Join(root, "gamma"))) != 0 {
		t.Errorf("expected no git calls in gamma")
	}
}

func TestSync_InvalidNameInOnly(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), []string{"alpha", "bogus"}, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	byRepo := resultsByRepo(sum)
	// The invalid name is reported as failed identifying the repository.
	bogus, ok := byRepo["bogus"]
	if !ok {
		t.Fatalf("expected a result for invalid name 'bogus', got %+v", sum.Results)
	}
	if bogus.Outcome != workspace.OutcomeFailed {
		t.Errorf("expected bogus failed, got %s", bogus.Outcome)
	}
	if !strings.Contains(bogus.Reason, "bogus") || !strings.Contains(bogus.Reason, "managed") {
		t.Errorf("expected reason to identify the invalid repo, got %q", bogus.Reason)
	}
	// The valid repository is still processed and updated.
	if byRepo["alpha"].Outcome != workspace.OutcomeCreated {
		t.Errorf("expected alpha updated, got %s", byRepo["alpha"].Outcome)
	}
	if _, ok := findCall(runner, dir, "merge", "--ff-only", "upstream/"+defaultBranch); !ok {
		t.Errorf("expected alpha to be fast-forwarded despite invalid sibling name")
	}
}

func TestSync_PushFailure(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {pushErr: errors.New("permission denied")},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, true)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("expected failed on push error, got %s (%+v)", res.Outcome, res)
	}
	if !strings.Contains(res.Reason, "push") {
		t.Errorf("expected reason to describe push failure, got %q", res.Reason)
	}
	// The fast-forward still happened before the push was attempted.
	if _, ok := findCall(runner, dir, "merge", "--ff-only", "upstream/"+defaultBranch); !ok {
		t.Errorf("expected fast-forward before push")
	}
	if _, ok := findCall(runner, dir, "push", originRemote, defaultBranch); !ok {
		t.Errorf("expected push %s %s to be attempted, calls=%v", originRemote, defaultBranch, callsInDir(runner, dir))
	}
}

func TestSync_PushSuccess(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{
		dir: {},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, true)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected updated, got %s (%+v)", res.Outcome, res)
	}
	if _, ok := findCall(runner, dir, "push", originRemote, defaultBranch); !ok {
		t.Errorf("expected push %s %s, calls=%v", originRemote, defaultBranch, callsInDir(runner, dir))
	}
}

func TestSync_MixedSummary(t *testing.T) {
	root := t.TempDir()
	dirClean := makeRepo(t, root, "clean")
	dirDirty := makeRepo(t, root, "dirty")
	dirDiverged := makeRepo(t, root, "diverged")
	dirFetchFail := makeRepo(t, root, "fetchfail")
	runner := scriptedRunner(map[string]scriptOptions{
		dirClean:     {},
		dirDirty:     {dirty: true},
		dirDiverged:  {cannotFF: true},
		dirFetchFail: {fetchErr: errors.New("boom")},
	})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	if len(sum.Results) != 4 {
		t.Fatalf("expected 4 results, got %d (%+v)", len(sum.Results), sum.Results)
	}
	if got := sum.Count(workspace.OutcomeCreated); got != 1 {
		t.Errorf("expected 1 updated, got %d", got)
	}
	if got := sum.Count(workspace.OutcomeSkipped); got != 2 {
		t.Errorf("expected 2 skipped, got %d", got)
	}
	if got := sum.Count(workspace.OutcomeFailed); got != 1 {
		t.Errorf("expected 1 failed, got %d", got)
	}
	if !sum.HasFailures() {
		t.Errorf("expected HasFailures to be true")
	}
}

func TestSync_EmptyWorkspace(t *testing.T) {
	root := t.TempDir()
	runner := scriptedRunner(map[string]scriptOptions{})

	sum, err := New().Sync(context.Background(), newApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}
	if len(sum.Results) != 0 {
		t.Fatalf("expected empty summary, got %+v", sum.Results)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no git calls, got %v", runner.ArgVectors())
	}
}

// dryRunApp builds an App context with DryRun enabled.
func dryRunApp(root string, runner git.Runner) app.App {
	a := newApp(root, runner)
	a.DryRun = true
	return a
}

// assertNoMutatingGitCalls fails the test if any recorded call in dir begins
// with a mutating git subcommand. Per Property 6 ("no fetch mutation") fetch is
// included in the forbidden set for dry-run.
func assertNoMutatingGitCalls(t *testing.T, runner *git.MockRunner, dir string) {
	t.Helper()
	for _, sub := range []string{"fetch", "checkout", "merge", "push"} {
		assertNoSubcommand(t, runner, dir, sub)
	}
}

// TestSync_DryRunFastForwardPreview verifies Requirements 8.4/8.5 and Property
// 6: a clean, fast-forwardable repo is previewed as "would fast-forward" using
// only read-only inspection, with no fetch/checkout/merge/push issued.
func TestSync_DryRunFastForwardPreview(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("expected would-update (created), got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "would fast-forward" {
		t.Errorf("expected reason %q, got %q", "would fast-forward", res.Reason)
	}
	// The read-only fast-forward check must still have run against the upstream ref.
	if _, ok := findCall(runner, dir, "merge-base", "--is-ancestor", defaultBranch, "upstream/"+defaultBranch); !ok {
		t.Errorf("expected read-only merge-base check, calls=%v", callsInDir(runner, dir))
	}
	assertNoMutatingGitCalls(t, runner, dir)
}

// TestSync_DryRunFastForwardPreviewWithPush verifies the push intent is
// reflected in the preview reason without any push being issued.
func TestSync_DryRunFastForwardPreviewWithPush(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {}})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, true)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Reason != "would fast-forward and push to origin" {
		t.Errorf("expected reason %q, got %q", "would fast-forward and push to origin", res.Reason)
	}
	assertNoMutatingGitCalls(t, runner, dir)
}

// TestSync_DryRunDirtySkipPreview verifies a dirty repo is previewed as skipped
// (uncommitted changes) with no mutation.
func TestSync_DryRunDirtySkipPreview(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {dirty: true}})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped, got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "would skip (uncommitted changes)" {
		t.Errorf("expected reason %q, got %q", "would skip (uncommitted changes)", res.Reason)
	}
	assertNoMutatingGitCalls(t, runner, dir)
	// A dirty repo is decided without even reaching the merge-base check.
	assertNoSubcommand(t, runner, dir, "merge-base")
}

// TestSync_DryRunDivergedSkipPreview verifies a diverged repo is previewed as
// skipped (diverged history) with no mutation.
func TestSync_DryRunDivergedSkipPreview(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {cannotFF: true}})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped, got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "would skip (diverged history)" {
		t.Errorf("expected reason %q, got %q", "would skip (diverged history)", res.Reason)
	}
	assertNoMutatingGitCalls(t, runner, dir)
}

// TestSync_DryRunDetachedSkipPreview verifies a detached HEAD is previewed as
// skipped with no mutation.
func TestSync_DryRunDetachedSkipPreview(t *testing.T) {
	root := t.TempDir()
	dir := makeRepo(t, root, "alpha")
	runner := scriptedRunner(map[string]scriptOptions{dir: {detached: true}})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, false)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	res := resultsByRepo(sum)["alpha"]
	if res.Outcome != workspace.OutcomeSkipped {
		t.Fatalf("expected skipped, got %s (%+v)", res.Outcome, res)
	}
	if res.Reason != "would skip (detached HEAD)" {
		t.Errorf("expected reason %q, got %q", "would skip (detached HEAD)", res.Reason)
	}
	assertNoMutatingGitCalls(t, runner, dir)
}

// TestSync_DryRunNoFetchEverIssued verifies across a mixed workspace that dry-run
// never issues a fetch (Property 6) yet still produces a preview for every repo.
func TestSync_DryRunNoFetchEverIssued(t *testing.T) {
	root := t.TempDir()
	dirClean := makeRepo(t, root, "clean")
	dirDirty := makeRepo(t, root, "dirty")
	dirDiverged := makeRepo(t, root, "diverged")
	runner := scriptedRunner(map[string]scriptOptions{
		dirClean:    {},
		dirDirty:    {dirty: true},
		dirDiverged: {cannotFF: true},
	})

	sum, err := New().Sync(context.Background(), dryRunApp(root, runner), nil, true)
	if err != nil {
		t.Fatalf("Sync returned unexpected error: %v", err)
	}

	// A preview result for every repo.
	if len(sum.Results) != 3 {
		t.Fatalf("expected 3 results, got %d (%+v)", len(sum.Results), sum.Results)
	}
	// No fetch anywhere in the workspace.
	for _, c := range runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "fetch" {
			t.Fatalf("dry-run must not fetch, but got: %v (in %s)", c.Args, c.Dir)
		}
	}
	for _, dir := range []string{dirClean, dirDirty, dirDiverged} {
		assertNoMutatingGitCalls(t, runner, dir)
	}
}
