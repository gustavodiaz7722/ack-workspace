package attributor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/config"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// validDoc is a minimal payload that passes document validation.
var validDoc = []byte(attributionHeader + "\n\n* `github.com/example/dep`\n")

// fakeBackend is a scriptable, recording Backend. It lets the whole flow run
// without AWS: provisioning, build start, polling, and artifact retrieval are
// all in memory.
type fakeBackend struct {
	prov    Provisioned
	provErr error

	buildID  string
	startErr error
	// states is returned by successive Status calls; the last entry repeats once
	// exhausted, so a test can model "in progress, then succeeded".
	states    []BuildStatus
	statusErr error

	artifact    []byte
	artifactErr error

	ensureCalls int
	startCalls  int
	statusCalls int
	gotRequests []BuildRequest
}

func (f *fakeBackend) EnsureInfrastructure(_ context.Context, in Infrastructure) (Provisioned, error) {
	f.ensureCalls++
	if f.provErr != nil {
		return Provisioned{}, f.provErr
	}
	prov := f.prov
	if prov.Project == "" {
		prov.Project = in.Project
	}
	if prov.Bucket == "" {
		prov.Bucket = "test-bucket"
	}
	return prov, nil
}

func (f *fakeBackend) StartBuild(_ context.Context, req BuildRequest) (string, error) {
	f.startCalls++
	f.gotRequests = append(f.gotRequests, req)
	if f.startErr != nil {
		return "", f.startErr
	}
	if f.buildID == "" {
		return "build:1", nil
	}
	return f.buildID, nil
}

func (f *fakeBackend) Status(_ context.Context, _ string) (BuildStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return BuildStatus{}, f.statusErr
	}
	if len(f.states) == 0 {
		return BuildStatus{State: StateSucceeded}, nil
	}
	idx := f.statusCalls - 1
	if idx >= len(f.states) {
		idx = len(f.states) - 1
	}
	return f.states[idx], nil
}

func (f *fakeBackend) FetchArtifact(_ context.Context, _, _ string) ([]byte, error) {
	if f.artifactErr != nil {
		return nil, f.artifactErr
	}
	if f.artifact == nil {
		return validDoc, nil
	}
	return f.artifact, nil
}

// newTestAttributor wires an Attributor with an instant sleep and a
// deterministic artifact key so the poll loop runs without delay and assertions
// are stable.
func newTestAttributor(b Backend, out *bytes.Buffer) *Attributor {
	a := NewWithWriter(b, out)
	a.sleep = func(context.Context, time.Duration) error { return nil }
	a.newKey = func(name string) string { return "attribution/" + name + "/fixed.md" }
	return a
}

// controllerWorkspace creates a workspace root holding git controller clones
// with a go.mod each, and returns the root.
func controllerWorkspace(t *testing.T, controllers ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range controllers {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}
	return root
}

// testApp wires an App around a root and git runner.
func testApp(root string, runner git.Runner, dryRun bool) app.App {
	return app.App{
		Config: config.Config{
			GitHubUser:    "octocat",
			WorkspaceRoot: root,
			RepoPrefix:    "ack-",
			Concurrency:   2,
		},
		Git:    runner,
		DryRun: dryRun,
	}
}

// gitRunner answers `symbolic-ref` with branch and `ls-remote` with a match,
// which is the happy path: a checked-out branch that exists on the remote.
func gitRunner(branch string) *git.MockRunner {
	return &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		switch {
		case len(args) > 0 && args[0] == "symbolic-ref":
			return branch + "\n", nil
		case len(args) > 0 && args[0] == "ls-remote":
			return "abc123\trefs/heads/" + branch + "\n", nil
		}
		return "", nil
	}}
}

func only(t *testing.T, s workspace.Summary) workspace.Result {
	t.Helper()
	if len(s.Results) != 1 {
		t.Fatalf("expected exactly one result, got %d: %+v", len(s.Results), s.Results)
	}
	return s.Results[0]
}

func TestGenerate_HappyPathWritesDocument(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}
	var out bytes.Buffer

	summary, err := newTestAttributor(backend, &out).Generate(
		context.Background(), testApp(root, gitRunner("feature-x"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}

	// The document must land in the controller checkout, byte-for-byte.
	dest := filepath.Join(root, "ecr-controller", attributionFileName)
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("reading %s: %v", dest, readErr)
	}
	if !bytes.Equal(got, validDoc) {
		t.Errorf("written document = %q, want %q", got, validDoc)
	}

	// The build must be told which repo and ref to clone: the fork at the
	// checked-out branch.
	if len(backend.gotRequests) != 1 {
		t.Fatalf("expected one build request, got %d", len(backend.gotRequests))
	}
	req := backend.gotRequests[0]
	if req.RepoURL != "https://github.com/octocat/ack-ecr-controller" {
		t.Errorf("repo = %q, want the contributor's fork", req.RepoURL)
	}
	if req.Ref != "feature-x" {
		t.Errorf("ref = %q, want the checked-out branch", req.Ref)
	}
}

func TestGenerate_FullControllerNameNormalizes(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr-controller"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if res := only(t, summary); res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
}

func TestGenerate_UpstreamTargetsOrganization(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}

	if _, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"ecr"}, Options{Upstream: true}); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	want := "https://github.com/" + UpstreamOwner + "/ecr-controller"
	if got := backend.gotRequests[0].RepoURL; got != want {
		t.Errorf("repo = %q, want %q", got, want)
	}
}

func TestGenerate_PRRefIsNormalized(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}

	if _, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"ecr"}, Options{Ref: "pr/42"}); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if got := backend.gotRequests[0].Ref; got != "refs/pull/42/head" {
		t.Errorf("ref = %q, want refs/pull/42/head", got)
	}
}

func TestGenerate_UnpushedRefFailsBeforeStartingCompute(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	// ls-remote finds nothing: the branch exists locally but was never pushed.
	runner := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return "local-only\n", nil
		}
		return "", nil
	}}
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, runner, false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "push it first") {
		t.Errorf("reason = %q, want it to tell the user to push", res.Reason)
	}
	if backend.startCalls != 0 {
		t.Error("no build must be started for an unpushed ref")
	}
	// Validation happens before provisioning, so a bad ref must not leave an IAM
	// role, bucket, or project behind in the user's account.
	if backend.ensureCalls != 0 {
		t.Error("no AWS resource must be provisioned when every target is invalid")
	}
}

func TestGenerate_ValidTargetsProceedAlongsideInvalidOnes(t *testing.T) {
	// ecr-controller exists; nosuch-controller does not.
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"nosuch", "ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(summary.Results), summary.Results)
	}
	// Both outcomes are reported, and only the viable target consumed a build.
	byRepo := map[string]workspace.Outcome{}
	for _, r := range summary.Results {
		byRepo[r.Repo] = r.Outcome
	}
	if byRepo["ecr-controller"] != workspace.OutcomeCreated {
		t.Errorf("ecr-controller = %q, want created", byRepo["ecr-controller"])
	}
	if byRepo["nosuch-controller"] != workspace.OutcomeFailed {
		t.Errorf("nosuch-controller = %q, want failed", byRepo["nosuch-controller"])
	}
	if backend.startCalls != 1 {
		t.Errorf("start calls = %d, want 1 (only the viable target)", backend.startCalls)
	}
}

func TestGenerate_DetachedHeadRequiresExplicitRef(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	runner := &git.MockRunner{ResponseFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return "", &git.ExitError{Code: 1}
		}
		return "", nil
	}}

	summary, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, runner, false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "--ref") {
		t.Errorf("reason = %q, want it to suggest --ref", res.Reason)
	}
}

func TestGenerate_PollsUntilTerminal(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{states: []BuildStatus{
		{State: StateInProgress, Phase: "INSTALL"},
		{State: StateInProgress, Phase: "BUILD"},
		{State: StateSucceeded, Phase: "COMPLETED"},
	}}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if res := only(t, summary); res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created; reason: %s", res.Outcome, res.Reason)
	}
	if backend.statusCalls != 3 {
		t.Errorf("status calls = %d, want 3 (two in-progress polls then success)", backend.statusCalls)
	}
}

func TestGenerate_FailedBuildReportsPhaseAndLogs(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{states: []BuildStatus{
		{State: StateFailed, Phase: "BUILD", LogsURL: "https://logs.example/stream"},
	}}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	// The failure must be diagnosable: which phase broke, and where to look.
	if !strings.Contains(res.Reason, "BUILD") || !strings.Contains(res.Reason, "https://logs.example/stream") {
		t.Errorf("reason = %q, want the phase and the log link", res.Reason)
	}
	// A failed build must never overwrite the checked-in file.
	if _, err := os.Stat(filepath.Join(root, "ecr-controller", attributionFileName)); !os.IsNotExist(err) {
		t.Error("a failed build must not write ATTRIBUTION.md")
	}
}

func TestGenerate_InvalidArtifactIsRefused(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	dest := filepath.Join(root, "ecr-controller", attributionFileName)
	existing := []byte(attributionHeader + "\noriginal\n")
	if err := os.WriteFile(dest, existing, 0o644); err != nil {
		t.Fatalf("seeding %s: %v", dest, err)
	}

	// A truncated payload: exactly the silent-corruption failure mode of the
	// log-scraping approach this feature replaced.
	backend := &fakeBackend{artifact: []byte("Waiting for agent ping\n")}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}

	// Critically, the existing document must be left intact.
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, existing) {
		t.Errorf("existing document was modified: %q", got)
	}
}

func TestGenerate_UnchangedDocumentIsReportedAsSuch(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	dest := filepath.Join(root, "ecr-controller", attributionFileName)
	if err := os.WriteFile(dest, validDoc, 0o644); err != nil {
		t.Fatalf("seeding %s: %v", dest, err)
	}

	summary, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created", res.Outcome)
	}
	if !strings.Contains(res.Reason, "already up to date") {
		t.Errorf("reason = %q, want it to report no change", res.Reason)
	}
}

func TestGenerate_TimeoutIsReported(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	// Never terminal, so the deadline is what stops the loop.
	backend := &fakeBackend{states: []BuildStatus{{State: StateInProgress, Phase: "BUILD"}}}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"ecr"}, Options{Timeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "did not finish within") {
		t.Errorf("reason = %q, want a timeout explanation", res.Reason)
	}
}

func TestGenerate_DryRunProvisionsNothing(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), true), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeCreated {
		t.Fatalf("outcome = %q, want created (preview)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "would") {
		t.Errorf("reason = %q, want a preview", res.Reason)
	}
	// Nothing at all may touch AWS in a preview.
	if backend.ensureCalls != 0 || backend.startCalls != 0 {
		t.Errorf("dry-run touched AWS: ensure=%d start=%d", backend.ensureCalls, backend.startCalls)
	}
	if _, err := os.Stat(filepath.Join(root, "ecr-controller", attributionFileName)); !os.IsNotExist(err) {
		t.Error("dry-run must not write ATTRIBUTION.md")
	}
}

func TestGenerate_AllExpandsAndProvisionsOnce(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller", "s3-controller")
	// A non-controller repo must be ignored by "all".
	if err := os.MkdirAll(filepath.Join(root, "runtime", ".git"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"all"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if len(summary.Results) != 2 {
		t.Fatalf("results = %d, want 2 (controllers only): %+v", len(summary.Results), summary.Results)
	}
	// Infrastructure is shared, so it must be ensured exactly once for the batch.
	if backend.ensureCalls != 1 {
		t.Errorf("ensure calls = %d, want 1", backend.ensureCalls)
	}
	if backend.startCalls != 2 {
		t.Errorf("start calls = %d, want 2", backend.startCalls)
	}
	// Results are ordered deterministically by repository name.
	if summary.Results[0].Repo != "ecr-controller" || summary.Results[1].Repo != "s3-controller" {
		t.Errorf("results not sorted by repo: %+v", summary.Results)
	}
}

func TestGenerate_MissingControllerFails(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{}

	summary, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "add it first") {
		t.Errorf("reason = %q, want it to suggest adding the controller", res.Reason)
	}
}

func TestGenerate_NoIdentifiersIsUsageError(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	_, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), nil, Options{})
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *UsageError", err, err)
	}
}

func TestGenerate_ProvisioningFailureIsPreflightError(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{provErr: errors.New("access denied")}

	_, err := newTestAttributor(backend, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{})
	if err == nil {
		t.Fatal("expected a pre-flight error when provisioning fails")
	}
	if backend.startCalls != 0 {
		t.Error("no build may start when provisioning failed")
	}
}

func TestGenerate_MissingIdentityWithoutUpstreamIsActionable(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	ap := testApp(root, gitRunner("main"), false)
	ap.Config.GitHubUser = ""

	summary, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), ap, []string{"ecr"}, Options{})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	res := only(t, summary)
	if res.Outcome != workspace.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Reason, "--upstream") {
		t.Errorf("reason = %q, want it to mention the --upstream escape hatch", res.Reason)
	}
}

func TestGenerate_ProvisioningNoticeIsReported(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	backend := &fakeBackend{prov: Provisioned{
		Project:        "p",
		Bucket:         "b",
		RoleARN:        "arn:aws:iam::1:role/ack-workspace-attribution-codebuild",
		CreatedRole:    true,
		CreatedBucket:  true,
		CreatedProject: true,
	}}
	var out bytes.Buffer

	if _, err := newTestAttributor(backend, &out).Generate(
		context.Background(), testApp(root, gitRunner("main"), false), []string{"ecr"}, Options{}); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	notice := out.String()
	for _, want := range []string{"IAM role", "S3 bucket b", "CodeBuild project p"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice = %q, want it to mention %q", notice, want)
		}
	}
}

func TestGenerate_OutputOverrideOnlyForSingleTarget(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller", "s3-controller")
	dest := filepath.Join(t.TempDir(), "custom.md")

	// With two targets the override is ignored, so each controller keeps its own
	// file rather than the two racing to write one path.
	if _, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"ecr", "s3"}, Options{Output: dest}); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("the output override must be ignored for multiple targets")
	}
	for _, name := range []string{"ecr-controller", "s3-controller"} {
		if _, err := os.Stat(filepath.Join(root, name, attributionFileName)); err != nil {
			t.Errorf("%s: expected its own ATTRIBUTION.md: %v", name, err)
		}
	}
}

func TestGenerate_SingleTargetHonorsOutputOverride(t *testing.T) {
	root := controllerWorkspace(t, "ecr-controller")
	dest := filepath.Join(t.TempDir(), "custom.md")

	if _, err := newTestAttributor(&fakeBackend{}, &bytes.Buffer{}).Generate(
		context.Background(), testApp(root, gitRunner("main"), false),
		[]string{"ecr"}, Options{Output: dest}); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("expected the document at %s: %v", dest, err)
	}
}

// --- pure helpers -------------------------------------------------------------

func TestValidateDocument(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"valid", validDoc, true},
		{"leading whitespace tolerated", append([]byte("\n\n"), validDoc...), true},
		{"empty", nil, false},
		{"truncated log noise", []byte("Waiting for agent ping\n"), false},
		{"wrong header", []byte("# Something Else\n"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDocument(tc.data)
			if tc.ok && err != nil {
				t.Errorf("validateDocument returned %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("validateDocument returned nil, want an error")
			}
		})
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"main":               "main",
		"v1.0.1":             "v1.0.1",
		"pr/42":              "refs/pull/42/head",
		"pull/7":             "refs/pull/7/head",
		"refs/pull/9/head":   "refs/pull/9/head",
		"feature/pr/not-num": "feature/pr/not-num",
	}
	for in, want := range cases {
		if got := normalizeRef(in); got != want {
			t.Errorf("normalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildspecUsesCanonicalInvocationAndS3Transport(t *testing.T) {
	spec := Infrastructure{}.withDefaults().buildspec()

	// The canonical attribution-gen invocation (per test-infra's
	// cd/lib/attribution.sh) uses --modfile and --output; the tool's own default
	// output name differs from ACK's ATTRIBUTION.md, so --output is required.
	if !strings.Contains(spec, "--modfile") || !strings.Contains(spec, "--output") {
		t.Errorf("buildspec must invoke attribution-gen with --modfile and --output:\n%s", spec)
	}
	// The document must travel through S3, never through logs.
	if !strings.Contains(spec, "aws s3 cp") {
		t.Errorf("buildspec must stage the document in S3:\n%s", spec)
	}
	// The guard that stops a failed generation from staging a partial object.
	if !strings.Contains(spec, `test -s "$OUT_FILE"`) {
		t.Errorf("buildspec must refuse to upload an empty document:\n%s", spec)
	}
	// The go runtime must be one the default image actually ships.
	if !strings.Contains(spec, `golang: "`+DefaultGoVersion+`"`) {
		t.Errorf("buildspec must request golang %s:\n%s", DefaultGoVersion, spec)
	}
	// Every per-build input must be referenced, since the project itself is
	// generic and never mutated between runs.
	for _, name := range []string{envRepoURL, envRepoRef, envArtifactBucket, envArtifactKey} {
		if !strings.Contains(spec, "$"+name) {
			t.Errorf("buildspec must consume $%s:\n%s", name, spec)
		}
	}
}

func TestPermissionPolicyIsScoped(t *testing.T) {
	policy := permissionPolicy("aws", "us-west-2", "123456789012", "proj", "bucket")

	// Least privilege: the log group is named, and the bucket is limited to a
	// single prefix. A wildcard resource would be a regression on the managed
	// CloudWatchLogsFullAccess policy this replaced.
	if !strings.Contains(policy, "arn:aws:logs:us-west-2:123456789012:log-group:/aws/codebuild/proj") {
		t.Errorf("policy must scope logs to the project's log group: %s", policy)
	}
	if !strings.Contains(policy, "arn:aws:s3:::bucket/*") {
		t.Errorf("policy must scope S3 writes to the artifact bucket: %s", policy)
	}
	if strings.Contains(policy, `"Resource":"*"`) {
		t.Errorf("policy must not grant a wildcard resource: %s", policy)
	}
}

func TestPartitionOf(t *testing.T) {
	cases := map[string]string{
		"arn:aws:iam::1:user/x":        "aws",
		"arn:aws-cn:iam::1:user/x":     "aws-cn",
		"arn:aws-us-gov:iam::1:role/x": "aws-us-gov",
		"":                             "aws",
		"garbage":                      "aws",
	}
	for in, want := range cases {
		if got := partitionOf(in); got != want {
			t.Errorf("partitionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteIfChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, attributionFileName)

	changed, err := writeIfChanged(path, validDoc)
	if err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	if !changed {
		t.Error("first write must report a change")
	}

	changed, err = writeIfChanged(path, validDoc)
	if err != nil {
		t.Fatalf("writeIfChanged: %v", err)
	}
	if changed {
		t.Error("rewriting identical content must report no change")
	}

	// The file must be readable by the world, like a checked-in document, rather
	// than carrying CreateTemp's 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("permissions = %v, want 0644", perm)
	}

	// No temporary files may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".attribution-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestBuildStateTerminal(t *testing.T) {
	if StateInProgress.Terminal() {
		t.Error("IN_PROGRESS must not be terminal")
	}
	for _, s := range []BuildState{StateSucceeded, StateFailed, StateStopped, StateFault, StateTimedOut} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	// An unrecognized state is treated as terminal rather than polled forever.
	if !BuildState("SOMETHING_NEW").Terminal() {
		t.Error("an unknown state must be treated as terminal")
	}
	if StateFailed.OK() {
		t.Error("FAILED must not be OK")
	}
	if !StateSucceeded.OK() {
		t.Error("SUCCEEDED must be OK")
	}
}
