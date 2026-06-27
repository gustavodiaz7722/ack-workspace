// Package workspace defines the shared data models used across all ack-workspace
// components so that every command reports its results uniformly.
package workspace

// RepoSpec fully describes how to fork, clone, and configure one repository.
type RepoSpec struct {
	UpstreamOwner string // "aws-controllers-k8s"
	UpstreamName  string // "runtime", "s3-controller"
	ForkOwner     string // contributor's GitHub_Identity
	ForkName      string // "<prefix><UpstreamName>"
	LocalPath     string // "<WorkspaceRoot>/<UpstreamName>"
}

// Outcome is the single terminal state of processing one repository.
type Outcome string

const (
	// OutcomeCreated indicates a repository was cloned and configured.
	// The add command renders this bucket with the label "added".
	OutcomeCreated Outcome = "created"
	// OutcomeSkipped indicates a repository was left untouched because it was
	// already present, dirty, or had diverged history.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeFailed indicates an error occurred while processing the repository.
	OutcomeFailed Outcome = "failed"
)

// Result is one repository's processing record.
type Result struct {
	Repo    string // upstream repo name
	Outcome Outcome
	Reason  string // human-readable: "uncommitted changes", "diverged history", error text
	Err     error  // underlying error when Outcome == failed
}

// Summary aggregates all Results for a command invocation.
type Summary struct {
	Results []Result
}

// Count returns the number of Results whose Outcome equals o.
func (s Summary) Count(o Outcome) int {
	n := 0
	for _, r := range s.Results {
		if r.Outcome == o {
			n++
		}
	}
	return n
}

// HasFailures reports whether any Result has an OutcomeFailed. It drives the
// process exit code (Requirements 7.4, 7.5, 7.6).
func (s Summary) HasFailures() bool {
	for _, r := range s.Results {
		if r.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}

// StatusEntry is the per-repository status record reported by the status command.
type StatusEntry struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"` // "" when detached
	Detached   bool   `json:"detached"`
	Comparison string `json:"comparison"` // "up_to_date" | "ahead" | "behind" | "unavailable"
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Dirty      bool   `json:"dirty"`
}
