package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aws-controllers-k8s/ack-workspace/internal/app"
	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// All is the sentinel accepted on any of the controller, resource, and issue
// dimensions to mean "every one", triggering fan-out into multiple
// conversations.
const All = "all"

// UsageError marks an argument/validation failure specific to the scan command
// (an unknown issue number or an unparsable issue selector), as opposed to a
// runtime failure. The CLI entrypoint maps it to the usage exit code.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

// Options are the resolved inputs for one scan invocation. Each of Controller,
// Resource, and Issue may be a concrete value or the sentinel "all"; any "all"
// widens the set of (controller, resource, issue) jobs that are run, and each
// job is one independent agent conversation.
type Options struct {
	// Controller is a controller alias ("acm") or "all".
	Controller string
	// Resource is a resource Kind ("Certificate") or "all".
	Resource string
	// Issue is an issue number ("1") or "all".
	Issue string
	// JSON requests machine-readable output instead of the human table.
	JSON bool
	// Concurrency bounds how many conversations run simultaneously.
	Concurrency int
}

// Finding is the outcome of one scan job: one issue investigated against one
// resource of one controller.
type Finding struct {
	Controller string `json:"controller"`
	Resource   string `json:"resource"`
	Issue      int    `json:"issue"`
	IssueName  string `json:"issue_name"`
	// Status is operational: whether the agent completed and reported.
	Status string `json:"status"` // "ok" | "failed"
	// Verdict is the issue's pass/fail judgment of the findings (only meaningful
	// when Status is "ok"): "pass" | "fail" | "error".
	Verdict string `json:"verdict,omitempty"`
	// Summary is the issue's reduced, human-readable summary of the findings.
	Summary string `json:"summary,omitempty"`
	// Findings is the raw structured report the agent submitted.
	Findings json.RawMessage `json:"findings,omitempty"`
	// Error is set when Status is "failed" or Verdict is "error".
	Error string `json:"error,omitempty"`
}

// Job status values.
const (
	statusOK     = "ok"
	statusFailed = "failed"
)

// Scanner drives the scan feature: it resolves the set of (controller,
// resource, issue) jobs an invocation selects, runs each as an independent
// agent conversation, and renders the aggregated findings.
//
// Like the status command, Scanner renders its own output and returns a neutral
// (empty) Summary; per-job failures are reported in the findings themselves
// (Status "failed" with an Error) rather than through the process exit code,
// which is reserved for pre-flight failures (an unknown issue, a missing
// controller, or an unreadable workspace root).
type Scanner struct {
	client   ModelClient
	registry *Registry
	out      io.Writer
	// traceOut, when non-nil, receives a human-readable transcript of every
	// conversation (system/user prompts, model turns, tool calls and results,
	// and the final findings). It is separate from out so a transcript never
	// corrupts the findings written to stdout, in particular the --json output.
	traceOut io.Writer
	// traceMu serializes writes from the per-job tracers so their lines are not
	// interleaved.
	traceMu sync.Mutex
}

// New returns a Scanner using client to talk to the model and the default issue
// registry, writing output to os.Stdout. Model selection belongs to the client.
func New(client ModelClient) *Scanner {
	return &Scanner{client: client, registry: NewRegistry(), out: os.Stdout}
}

// NewWithWriter is New with an injectable writer for tests.
func NewWithWriter(client ModelClient, out io.Writer) *Scanner {
	return &Scanner{client: client, registry: NewRegistry(), out: out}
}

// NewWithWriterToken is the production constructor: it builds the issue registry
// with a GitHub-authenticated docs fetcher so documentation-listing requests are
// not throttled. An empty githubToken degrades to anonymous access.
func NewWithWriterToken(client ModelClient, out io.Writer, githubToken string) *Scanner {
	return &Scanner{client: client, registry: NewRegistryWithToken(githubToken), out: out}
}

// SetTraceWriter enables conversation tracing, directing the transcript at w
// (typically stderr). Passing a non-nil writer also serializes job execution so
// the transcripts of concurrent conversations do not interleave.
func (s *Scanner) SetTraceWriter(w io.Writer) {
	s.traceOut = w
}

// job is one unit of work: an issue to investigate against a target.
type job struct {
	target Target
	issue  Issue
}

// Scan resolves the selected jobs, runs them (bounded by opts.Concurrency), and
// renders the findings. The returned Summary is always neutral; the error is
// non-nil only for a pre-flight failure that prevents any work from starting.
func (s *Scanner) Scan(ctx context.Context, a app.App, opts Options) (workspace.Summary, error) {
	issues, err := s.resolveIssues(opts.Issue)
	if err != nil {
		return workspace.Summary{}, err
	}

	controllers, err := resolveControllers(a.Config.WorkspaceRoot, opts.Controller)
	if err != nil {
		return workspace.Summary{}, err
	}
	if len(controllers) == 0 {
		fmt.Fprintf(s.out, "No controllers found under %s\n", a.Config.WorkspaceRoot)
		return workspace.Summary{}, nil
	}

	jobs, err := buildJobs(controllers, opts.Resource, issues)
	if err != nil {
		return workspace.Summary{}, err
	}
	if len(jobs) == 0 {
		fmt.Fprintln(s.out, "No matching resources to scan")
		return workspace.Summary{}, nil
	}

	concurrency := opts.Concurrency
	if s.traceOut != nil {
		// Serialize while tracing so each conversation's transcript is emitted as
		// a contiguous block rather than interleaved with others.
		concurrency = 1
	}
	findings := s.runJobs(ctx, jobs, concurrency)
	s.render(findings, opts.JSON)
	return workspace.Summary{}, nil
}

// resolveIssues turns the issue selector into the concrete issues to run: "all"
// expands to every registered issue; otherwise the selector must be a known
// issue number. An unparsable or unknown selector is a *UsageError.
func (s *Scanner) resolveIssues(selector string) ([]Issue, error) {
	if selector == All {
		return s.registry.All(), nil
	}
	n, err := strconv.Atoi(selector)
	if err != nil {
		return nil, &UsageError{Msg: fmt.Sprintf("invalid issue %q: expected an issue number or %q", selector, All)}
	}
	issue, ok := s.registry.Get(n)
	if !ok {
		return nil, &UsageError{Msg: fmt.Sprintf("unknown issue %d", n)}
	}
	return []Issue{issue}, nil
}

// resolveControllers turns the controller selector into concrete controller
// references: "all" discovers every controller under root; otherwise a single
// named controller is resolved.
func resolveControllers(root, selector string) ([]controllerRef, error) {
	if selector == All {
		return discoverControllers(root)
	}
	ref, err := findController(root, selector)
	if err != nil {
		return nil, err
	}
	return []controllerRef{ref}, nil
}

// buildJobs expands the selected controllers, resource selector, and issues into
// the full, deterministically ordered set of jobs. For each controller the
// resource selector is resolved ("all" discovers the controller's resources;
// otherwise the single named resource is used), and every (resource, issue)
// pair becomes a job.
func buildJobs(controllers []controllerRef, resourceSel string, issues []Issue) ([]job, error) {
	var jobs []job
	for _, c := range controllers {
		resources, err := resolveResources(c, resourceSel)
		if err != nil {
			return nil, err
		}
		for _, res := range resources {
			target := Target{Controller: c.Alias, Resource: res, RepoPath: c.Path}
			for _, issue := range issues {
				jobs = append(jobs, job{target: target, issue: issue})
			}
		}
	}
	return jobs, nil
}

// resolveResources returns the resources to scan for one controller: every
// resource declared in its generator.yaml when the selector is "all", or the
// single named resource otherwise.
func resolveResources(c controllerRef, selector string) ([]string, error) {
	if selector != All {
		return []string{selector}, nil
	}
	return discoverResources(c.Path)
}

// runJobs executes jobs with at most concurrency conversations running at once
// and returns one Finding per job in input order (which is deterministic).
func (s *Scanner) runJobs(ctx context.Context, jobs []job, concurrency int) []Finding {
	if concurrency <= 0 {
		concurrency = 1
	}
	findings := make([]Finding, len(jobs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			findings[idx] = s.runJob(ctx, jobs[idx])
		}(i)
	}
	wg.Wait()
	return findings
}

// runJob runs one issue investigation against one target as a single agent
// conversation and packages the outcome as a Finding. A model/transport error
// or a conversation that never reports findings is captured as a failed
// Finding, never as a returned error, so one bad job never aborts the batch.
func (s *Scanner) runJob(ctx context.Context, j job) Finding {
	f := Finding{
		Controller: j.target.Controller,
		Resource:   j.target.Resource,
		Issue:      j.issue.Number,
		IssueName:  j.issue.Name,
	}
	agent := NewAgent(s.client)
	if s.traceOut != nil {
		agent.tr = &writerTracer{
			w:      s.traceOut,
			mu:     &s.traceMu,
			prefix: fmt.Sprintf("%s/%s#%d", j.target.Controller, j.target.Resource, j.issue.Number),
		}
	}
	result, err := agent.Run(ctx, j.target,
		j.issue.System(j.target), j.issue.Prompt(j.target),
		j.issue.agentTools(), reportToolName)
	if err != nil {
		f.Status = statusFailed
		f.Error = err.Error()
		return f
	}
	f.Status = statusOK
	f.Findings = result

	// Apply the issue's pass/fail evaluation and reduced summary to the reported
	// findings.
	if j.issue.Evaluate != nil {
		verdict, evalErr := j.issue.Evaluate(result)
		f.Verdict = string(verdict)
		if evalErr != nil {
			f.Error = evalErr.Error()
		}
	}
	if j.issue.Summarize != nil {
		f.Summary = j.issue.Summarize(result)
	}
	return f
}

// render writes the findings as indented JSON (when jsonOut) or a human-readable
// per-job result with the issue's reduced summary.
func (s *Scanner) render(findings []Finding, jsonOut bool) {
	if jsonOut {
		data, err := json.MarshalIndent(findings, "", "  ")
		if err != nil {
			fmt.Fprintf(s.out, "error rendering JSON: %v\n", err)
			return
		}
		fmt.Fprintf(s.out, "%s\n", data)
		return
	}
	s.renderText(findings)
}

// renderText prints, per job, a result line (controller/resource, issue, and the
// PASS/FAIL/ERROR result) followed by the issue's reduced summary indented
// beneath it.
func (s *Scanner) renderText(findings []Finding) {
	for _, f := range findings {
		fmt.Fprintf(s.out, "%s/%s  issue %d (%s)  %s\n",
			f.Controller, f.Resource, f.Issue, f.IssueName, resultLabel(f))
		detail := f.Summary
		if f.Status == statusFailed || f.Verdict == string(VerdictError) {
			detail = f.Error
		}
		for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(s.out, "    %s\n", line)
		}
	}
}

// resultLabel is the uppercase result shown per job: PASS/FAIL from the issue's
// verdict, or ERROR when the agent failed or the findings could not be
// evaluated.
func resultLabel(f Finding) string {
	if f.Status == statusFailed {
		return "ERROR"
	}
	switch f.Verdict {
	case string(VerdictPass):
		return "PASS"
	case string(VerdictFail):
		return "FAIL"
	default:
		return "ERROR"
	}
}
