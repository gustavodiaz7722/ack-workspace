// Package attributor regenerates a service controller's ATTRIBUTION.md by
// running the upstream `attribution-gen` tool on ephemeral AWS CodeBuild
// compute and staging the result through S3.
//
// # Why the work runs remotely
//
// Generating an attribution document means walking the module dependency graph,
// which requires fetching every dependency from the public Go module proxy.
// From inside the Amazon corporate network those fetches are blocked, so
// running `attribution-gen` locally fails. Ephemeral compute outside the
// corporate network is therefore a hard requirement of this feature, not an
// optimization: CodeBuild is where the generation has to happen.
//
// # How this differs from the scripts it replaces
//
// The workflow is modeled on the misc/bootstrap_codebuild.sh and
// misc/run_attribution.sh scripts, but every mechanism they used to move data
// around has been replaced:
//
//   - The document is transported through S3, not scraped out of CloudWatch logs.
//     The scripts reconstructed the file by extracting log text between marker
//     lines and stripping tab characters, which silently corrupted license text
//     and truncated the result whenever logs were incomplete. Logs are now used
//     only to give a human a link when a build fails.
//   - The CodeBuild project is immutable and generic. The scripts pointed a
//     single shared project at one repository and rewrote its source with
//     update-project for every run, so two concurrent runs raced. Here the
//     repository and ref are per-build environment overrides, and the project
//     uses a NO_SOURCE source type whose buildspec clones the target itself.
//   - The IAM role gets a scoped inline policy (one log group, one bucket
//     prefix) rather than the managed CloudWatchLogsFullAccess policy, which was
//     only needed because logs were the transport.
//   - The ref is verified to exist on the remote before compute is started, and
//     the fetched document is validated before it is allowed to overwrite a
//     checked-in file.
//
// # Reporting
//
// Like the builder and deployer, the component never returns an error for a
// per-controller problem: every such failure is captured as a failed Result so
// the caller renders a uniform summary. A returned error means a pre-flight
// failure that stopped the run before any controller was processed.
package attributor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/engine"
	"github.com/aws-controllers-k8s/ack-workspace/internal/git"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

const (
	// controllerSuffix is the conventional suffix of every service controller
	// repository name. A bare alias ("ecr") and its full form ("ecr-controller")
	// normalize to the same repository.
	controllerSuffix = "-controller"
	// allToken expands to every managed controller repository under the workspace
	// root. It is matched case-insensitively.
	allToken = "all"
	// UpstreamOwner is the GitHub organization hosting the canonical ACK repos. It
	// is exported so the command layer can name it in help text.
	UpstreamOwner = "aws-controllers-k8s"
	// attributionFileName is the checked-in document this feature regenerates.
	// Note the singular spelling: it is an ACK convention that differs from
	// attribution-gen's own ATTRIBUTIONS.md default, which is why the tool is
	// always invoked with an explicit --output.
	attributionFileName = "ATTRIBUTION.md"

	// defaultPollInterval is how often a running build is polled.
	defaultPollInterval = 10 * time.Second
	// defaultTimeout bounds how long one build is waited on. Generation is
	// normally a few minutes; the ceiling exists so a wedged build cannot hang the
	// command forever.
	defaultTimeout = 20 * time.Minute
)

// prRefPattern matches the shorthand "pr/123" (and "pull/123") a reviewer is
// likely to type, which is rewritten to the ref GitHub actually serves.
var prRefPattern = regexp.MustCompile(`^(?:pr|pull)/(\d+)$`)

// shaPattern matches an abbreviated-or-full hex commit id.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// UsageError is a typed argument/validation error returned before any AWS
// resource is touched. The cmd layer maps it to the usage exit code.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Options are the resolved inputs for one attribution run.
type Options struct {
	// Ref is the git ref to generate from. When empty, each controller's currently
	// checked-out branch is used, which matches how `build` and `deploy` operate
	// on local state.
	Ref string
	// RepoURL overrides the repository the build clones. When empty it is derived
	// from the contributor's fork (or the upstream org when Upstream is set).
	RepoURL string
	// Upstream targets the aws-controllers-k8s organization instead of the
	// contributor's fork.
	Upstream bool
	// Output overrides where the generated document is written. It is only
	// meaningful for a single target; with several it is ignored in favor of each
	// controller's own ATTRIBUTION.md.
	Output string
	// Infra names and sizes the remote compute. Empty fields take package
	// defaults.
	Infra Infrastructure
	// PollInterval and Timeout control the wait loop. Zero means the default.
	PollInterval time.Duration
	Timeout      time.Duration
}

// Attributor regenerates a controller's ATTRIBUTION.md.
type Attributor struct {
	backend Backend
	out     io.Writer
	// sleep waits between status polls. It is a field so tests can run the poll
	// loop without real delays.
	sleep func(ctx context.Context, d time.Duration) error
	// newKey builds the S3 object key one build stages its document at. It is a
	// field so tests get deterministic keys.
	newKey func(name string) string
}

// New returns an Attributor backed by b, reporting provisioning notices to
// os.Stdout.
func New(b Backend) *Attributor { return NewWithWriter(b, os.Stdout) }

// NewWithWriter is New with an injectable writer for tests and for directing
// notices at the command's own stdout.
func NewWithWriter(b Backend, out io.Writer) *Attributor {
	return &Attributor{
		backend: b,
		out:     out,
		sleep:   sleepCtx,
		newKey:  randomKey,
	}
}

// Generate regenerates ATTRIBUTION.md for each identified controller and
// returns a Summary recording every target in exactly one of the generated
// (OutcomeSucceeded), skipped, or failed buckets.
//
// The special "all" identifier expands to every managed controller under the
// workspace root and supersedes any other identifier. Infrastructure is
// provisioned once, before any build starts; builds then run concurrently under
// the configured concurrency limit.
//
// The returned error is non-nil only for a pre-flight failure: an empty
// identifier list, a discovery failure while expanding "all", or a failure to
// provision the remote compute.
func (a *Attributor) Generate(ctx context.Context, ap app.App, identifiers []string, opts Options) (workspace.Summary, error) {
	if len(identifiers) == 0 {
		return workspace.Summary{}, &UsageError{Msg: "at least one service identifier (or 'all') is required"}
	}

	names, err := a.expand(ap, identifiers)
	if err != nil {
		return workspace.Summary{}, err
	}
	if len(names) == 0 {
		return workspace.Summary{}, nil
	}

	// Dry-run reports what each target would do and provisions nothing, so a
	// preview never creates an IAM role, a bucket, or a project. It also stays
	// purely local: no remote ref lookup, matching the dry-run contract that a
	// preview touches neither GitHub, git, nor the filesystem.
	if ap.DryRun {
		results := make([]workspace.Result, 0, len(names))
		for _, name := range names {
			results = append(results, a.preview(ctx, ap, name, opts))
		}
		sortResults(results)
		return workspace.Summary{Results: results}, nil
	}

	// Validate every target before provisioning anything. All of these checks are
	// local or read-only, so a typo, a missing controller, or an unpushed branch
	// is reported without having created an IAM role, a bucket, or a project in
	// the user's account.
	plans := make([]plan, 0, len(names))
	results := make([]workspace.Result, 0, len(names))
	for _, name := range names {
		p, err := a.prepare(ctx, ap, name, opts)
		if err == nil {
			err = verifyRefIsPushed(ctx, ap, p)
		}
		if err != nil {
			results = append(results, failed(name, err))
			continue
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		sortResults(results)
		return workspace.Summary{Results: results}, nil
	}

	prov, err := a.backend.EnsureInfrastructure(ctx, opts.Infra.withDefaults())
	if err != nil {
		return workspace.Summary{}, fmt.Errorf("provisioning attribution compute: %w", err)
	}
	a.reportProvisioning(prov)

	// The --output override only applies when a single controller was requested;
	// see destination.
	single := len(names) == 1
	tasks := make([]engine.Task, 0, len(plans))
	for _, p := range plans {
		p := p
		tasks = append(tasks, func(ctx context.Context) workspace.Result {
			return a.process(ctx, p, opts, prov, single)
		})
	}
	results = append(results, engine.Run(ctx, ap.Config.Concurrency, tasks)...)
	sortResults(results)
	return workspace.Summary{Results: results}, nil
}

// expand turns the supplied identifiers into concrete controller repository
// names. "all" supersedes the rest and expands to the controllers discovered
// under the workspace root.
func (a *Attributor) expand(ap app.App, identifiers []string) ([]string, error) {
	for _, id := range identifiers {
		if strings.EqualFold(strings.TrimSpace(id), allToken) {
			return discoverControllers(ap)
		}
	}

	seen := make(map[string]bool, len(identifiers))
	names := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		alias := strings.TrimSuffix(strings.TrimSpace(id), controllerSuffix)
		if alias == "" {
			// Keep the invalid token so it is reported as a failed Result rather than
			// silently dropped.
			names = append(names, strings.TrimSpace(id))
			continue
		}
		name := alias + controllerSuffix
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// discoverControllers lists managed "*-controller" repositories under the
// workspace root, sorted by name.
func discoverControllers(ap app.App) ([]string, error) {
	repos, err := workspace.Discover(ap.Config.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering managed repositories under %q: %w", ap.Config.WorkspaceRoot, err)
	}
	controllers := make([]string, 0, len(repos))
	for _, name := range repos {
		if strings.HasSuffix(name, controllerSuffix) {
			controllers = append(controllers, name)
		}
	}
	sort.Strings(controllers)
	return controllers, nil
}

// plan is one target's resolved build inputs.
type plan struct {
	name    string
	path    string
	repoURL string
	ref     string
}

// prepare validates a target and resolves the repository and ref its build will
// use. Everything here is read-only and local, so an invalid target is reported
// without starting remote compute.
func (a *Attributor) prepare(ctx context.Context, ap app.App, name string, opts Options) (plan, error) {
	if name == "" || !strings.HasSuffix(name, controllerSuffix) {
		return plan{}, fmt.Errorf("invalid controller identifier %q", name)
	}

	path := filepath.Join(ap.Config.WorkspaceRoot, name)
	if !dirExists(path) {
		alias := strings.TrimSuffix(name, controllerSuffix)
		return plan{}, fmt.Errorf("controller %s not found at %s; add it first with `ack-workspace add %s`", name, path, alias)
	}
	if !isGitRepo(path) {
		return plan{}, fmt.Errorf("%s is not a git repository", path)
	}
	if !fileExists(filepath.Join(path, "go.mod")) {
		return plan{}, fmt.Errorf("no go.mod in %s; attribution is generated from the module graph", path)
	}

	repoURL, err := resolveRepoURL(ap, name, opts)
	if err != nil {
		return plan{}, err
	}
	ref, err := a.resolveRef(ctx, ap, path, opts)
	if err != nil {
		return plan{}, err
	}
	return plan{name: name, path: path, repoURL: repoURL, ref: ref}, nil
}

// resolveRepoURL determines which repository the remote build clones. The
// contributor's fork is the default because that is where in-progress work
// lives in the fork-first workflow; --upstream targets the canonical repo and
// --repo overrides both.
func resolveRepoURL(ap app.App, name string, opts Options) (string, error) {
	if url := strings.TrimSpace(opts.RepoURL); url != "" {
		return url, nil
	}
	if opts.Upstream {
		return fmt.Sprintf("https://github.com/%s/%s", UpstreamOwner, name), nil
	}
	if ap.Config.GitHubUser == "" {
		return "", fmt.Errorf("no GitHub identity is configured, so the fork to generate from is unknown; " +
			"pass --github-user, set GITHUB_USER, or use --upstream to generate from " + UpstreamOwner)
	}
	return fmt.Sprintf("https://github.com/%s/%s%s", ap.Config.GitHubUser, ap.Config.RepoPrefix, name), nil
}

// resolveRef determines the git ref to generate from: the explicit --ref, or
// the controller's currently checked-out branch. A detached HEAD cannot be
// named remotely, so it is reported as an actionable error rather than guessed
// at.
func (a *Attributor) resolveRef(ctx context.Context, ap app.App, path string, opts Options) (string, error) {
	if ref := strings.TrimSpace(opts.Ref); ref != "" {
		return normalizeRef(ref), nil
	}
	branch, detached, err := git.NewRepo(path, ap.Git).CurrentBranch(ctx)
	if err != nil {
		return "", fmt.Errorf("determining the checked-out branch of %s: %w (pass --ref to name one explicitly)", path, err)
	}
	if detached {
		return "", fmt.Errorf("%s has a detached HEAD, which cannot be resolved remotely; pass --ref", path)
	}
	return normalizeRef(branch), nil
}

// normalizeRef rewrites the "pr/123" shorthand to the pull-request head ref
// GitHub actually serves. Any other ref is passed through untouched.
func normalizeRef(ref string) string {
	if m := prRefPattern.FindStringSubmatch(ref); m != nil {
		return "refs/pull/" + m[1] + "/head"
	}
	return ref
}

// verifyRefIsPushed checks that the resolved ref is visible on the remote
// before compute is started.
//
// An unpushed branch would otherwise surface as an opaque CodeBuild source
// failure minutes later. A commit id is exempt because ls-remote does not
// advertise arbitrary commits; for those the build itself is the only check.
func verifyRefIsPushed(ctx context.Context, ap app.App, p plan) error {
	if shaPattern.MatchString(p.ref) {
		return nil
	}
	out, err := ap.Git.Run(ctx, p.path, "ls-remote", p.repoURL, p.ref)
	if err != nil {
		return fmt.Errorf("listing refs of %s: %w", p.repoURL, err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("ref %q does not exist in %s; push it first "+
			"(the build clones from the remote, so unpushed commits are invisible to it)", p.ref, p.repoURL)
	}
	return nil
}

// preview computes what a target would do without provisioning or building.
func (a *Attributor) preview(ctx context.Context, ap app.App, name string, opts Options) workspace.Result {
	p, err := a.prepare(ctx, ap, name, opts)
	if err != nil {
		return failed(name, err)
	}
	infra := opts.Infra.withDefaults()
	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeSucceeded,
		Reason: fmt.Sprintf(
			"would run attribution-gen on CodeBuild project %s (image %s, golang %s) against %s@%s and write %s",
			infra.Project, infra.Image, infra.GoVersion, p.repoURL, p.ref, a.destination(p, opts, true)),
	}
}

// process runs the remote generation flow for one already-validated target and
// returns its terminal Result. It never returns an error out-of-band.
func (a *Attributor) process(ctx context.Context, p plan, opts Options, prov Provisioned, single bool) workspace.Result {
	name := p.name
	key := a.newKey(name)
	buildID, err := a.backend.StartBuild(ctx, BuildRequest{
		Project: prov.Project,
		RepoURL: p.repoURL,
		Ref:     p.ref,
		Bucket:  prov.Bucket,
		Key:     key,
	})
	if err != nil {
		return failed(name, fmt.Errorf("starting attribution build: %w", err))
	}

	status, err := a.wait(ctx, buildID, opts)
	if err != nil {
		return failed(name, err)
	}
	if !status.State.OK() {
		return failed(name, buildFailure(status))
	}

	data, err := a.backend.FetchArtifact(ctx, prov.Bucket, key)
	if err != nil {
		return failed(name, fmt.Errorf("fetching the generated document: %w", err))
	}
	if err := validateDocument(data); err != nil {
		return failed(name, err)
	}

	dest := a.destination(p, opts, single)
	changed, err := writeIfChanged(dest, data)
	if err != nil {
		return failed(name, err)
	}

	verb := "updated"
	if !changed {
		verb = "already up to date"
	}
	return workspace.Result{
		Repo:    name,
		Outcome: workspace.OutcomeSucceeded,
		Reason:  fmt.Sprintf("%s (%s, %d bytes, from %s@%s)", verb, dest, len(data), p.repoURL, p.ref),
	}
}

// destination is the path the generated document is written to. An explicit
// --output only applies when a single controller was targeted; fanning several
// controllers into one file would silently discard all but the last.
func (a *Attributor) destination(p plan, opts Options, single bool) string {
	if single {
		if out := strings.TrimSpace(opts.Output); out != "" {
			return out
		}
	}
	return filepath.Join(p.path, attributionFileName)
}

// wait polls the build until it reaches a terminal state, the deadline passes,
// or the context is cancelled.
func (a *Attributor) wait(ctx context.Context, buildID string, opts Options) (BuildStatus, error) {
	interval := opts.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		status, err := a.backend.Status(ctx, buildID)
		if err != nil {
			return BuildStatus{}, fmt.Errorf("polling build %s: %w", buildID, err)
		}
		if status.State.Terminal() {
			return status, nil
		}
		if !time.Now().Before(deadline) {
			return status, fmt.Errorf("build %s did not finish within %s (last phase %s); it may still be running: %s",
				buildID, timeout, orUnknown(status.Phase), orUnknown(status.LogsURL))
		}
		if err := a.sleep(ctx, interval); err != nil {
			return status, err
		}
	}
}

// buildFailure renders a failed build as an actionable error. The log link is
// included instead of log contents: diagnosing a build is a human task, and
// parsing logs is exactly the fragile behavior this feature removed.
func buildFailure(status BuildStatus) error {
	return fmt.Errorf("attribution build %s in phase %s; see %s",
		strings.ToLower(string(status.State)), orUnknown(status.Phase), orUnknown(status.LogsURL))
}

// reportProvisioning notes any AWS resource that had to be created, so the user
// is told what this command added to their account rather than finding out
// later.
func (a *Attributor) reportProvisioning(prov Provisioned) {
	if a.out == nil || !prov.Created() {
		return
	}
	var created []string
	if prov.CreatedRole {
		created = append(created, "IAM role "+lastPathSegment(prov.RoleARN))
	}
	if prov.CreatedBucket {
		created = append(created, "S3 bucket "+prov.Bucket)
	}
	if prov.CreatedProject {
		created = append(created, "CodeBuild project "+prov.Project)
	} else if prov.UpdatedProject {
		created = append(created, "updated CodeBuild project "+prov.Project)
	}
	fmt.Fprintf(a.out, "Provisioned attribution compute: %s\n", strings.Join(created, ", "))
}

// writeIfChanged writes data to path only when it differs from the current
// contents, reporting whether it wrote. The write goes to a temporary file in
// the destination directory and is then renamed, so an interrupted run can
// never leave a half-written ATTRIBUTION.md in the repository.
func writeIfChanged(path string, data []byte) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return false, nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".attribution-*")
	if err != nil {
		return false, fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("closing %s: %w", tmpName, err)
	}
	// Match the 0644 a checked-in file is expected to carry; CreateTemp uses 0600.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return false, fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

// randomKey builds a collision-free S3 key for one build's staged document. The
// key is chosen locally, before the build starts, so it can travel to the build
// as an environment override and be read back without consulting CodeBuild's
// artifact naming rules.
func randomKey(name string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A failure of the system CSPRNG is not recoverable here, but it also does
		// not need to be fatal: the timestamp alone is enough to avoid a collision in
		// practice for a human-driven command.
		return fmt.Sprintf("attribution/%s/%d.md", name, time.Now().UnixNano())
	}
	return fmt.Sprintf("attribution/%s/%s.md", name, hex.EncodeToString(buf[:]))
}

// sleepCtx waits for d, returning early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sortResults orders results by repository name so preview output matches the
// deterministic ordering engine.Run produces for real runs.
func sortResults(results []workspace.Result) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Repo < results[j].Repo })
}

// orUnknown substitutes a placeholder for an empty diagnostic string.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// lastPathSegment returns the final "/"-separated segment, turning an IAM role
// ARN into its role name for display.
func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return s
}

func failed(name string, err error) workspace.Result {
	return workspace.Result{Repo: name, Outcome: workspace.OutcomeFailed, Reason: err.Error(), Err: err}
}

// dirExists reports whether path exists.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isGitRepo reports whether dir contains a ".git" entry (a clone or a worktree
// gitfile).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
