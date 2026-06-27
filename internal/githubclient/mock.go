package githubclient

import (
	"context"
	"sync"
)

// Mock is a scripted, call-recording implementation of GitHubClient for use in
// tests of components that depend on GitHubClient (such as the initializer and
// adder). It is designed for ergonomic table-driven scripting:
//
//   - Per-repository state (exists / not-found / API error) is configured via
//     SetRepo or by populating Repos directly.
//   - Per-call sequences (for example, a fork that is missing on the first poll
//     and present on a later one, or one that never appears to force a timeout)
//     are configured via QueueExists.
//   - Fork creation behavior is controlled by CreateForkErr (to simulate a
//     create failure or timeout) and ForkAppears (whether the new fork becomes
//     queryable afterward).
//
// Every call is recorded in Calls so tests can assert which methods were
// invoked and with what arguments. The zero value is not usable; construct a
// Mock with NewMock.
type Mock struct {
	mu sync.Mutex

	// Repos holds the static scripted state keyed by "owner/name". A key that
	// is absent resolves as not-found (exists=false, nil error).
	Repos map[string]RepoState

	// existSeq holds optional per-reference RepoExists result sequences, keyed
	// by "owner/name". When a sequence exists and is non-empty, the next call
	// consumes its head; once exhausted the Mock falls back to Repos.
	existSeq map[string][]ExistResult

	// ForkOwner is the owner assigned to forks returned by CreateFork. It
	// defaults to "octocat".
	ForkOwner string

	// ForkAppears controls whether a successful CreateFork registers the new
	// fork as an existing repository in Repos. It defaults to true.
	ForkAppears bool

	// CreateForkErr, when non-nil, is returned by CreateFork without creating a
	// fork. Set it to an arbitrary error to simulate a create failure or to a
	// *ForkTimeoutError to simulate the fork never becoming queryable.
	CreateForkErr error

	// OrgRepos holds the scripted repository names returned by ListOrgRepos,
	// keyed by organization. A key that is absent yields an empty list.
	OrgRepos map[string][]string

	// ListOrgReposErr, when non-nil, is returned by ListOrgRepos to simulate a
	// listing/API failure.
	ListOrgReposErr error

	// DeleteRepoErr, when non-nil, is returned by DeleteRepo to simulate a
	// delete failure.
	DeleteRepoErr error

	// PullRequestURL is the URL CreatePullRequest returns on success. It
	// defaults to a synthetic URL derived from the upstream reference.
	PullRequestURL string

	// CreatePullRequestErr, when non-nil, is returned by CreatePullRequest to
	// simulate a PR-creation failure.
	CreatePullRequestErr error

	// Optional full overrides. When set, they take precedence over the scripted
	// state above and are still recorded in Calls.
	RepoExistsFn        func(ctx context.Context, ref RepoRef) (bool, error)
	DefaultBranchFn     func(ctx context.Context, ref RepoRef) (string, error)
	CreateForkFn        func(ctx context.Context, upstream RepoRef, forkName string) (RepoRef, error)
	ListOrgReposFn      func(ctx context.Context, org string) ([]string, error)
	DeleteRepoFn        func(ctx context.Context, ref RepoRef) error
	CreatePullRequestFn func(ctx context.Context, upstream RepoRef, in NewPullRequest) (string, error)

	// Calls records every invocation in order.
	Calls []Call
}

// RepoState is the scripted state of a single repository.
type RepoState struct {
	// Exists reports whether RepoExists should return true.
	Exists bool
	// DefaultBranch is returned by DefaultBranch when Err is nil.
	DefaultBranch string
	// Err, when non-nil, is returned by RepoExists and DefaultBranch to simulate
	// a transport or API error (as opposed to a not-found result).
	Err error
}

// ExistResult is one scripted outcome for a RepoExists call in a sequence.
type ExistResult struct {
	Exists bool
	Err    error
}

// Call records a single invocation against the Mock.
type Call struct {
	// Method is one of "RepoExists", "DefaultBranch", "CreateFork",
	// "ListOrgRepos", "DeleteRepo", or "CreatePullRequest".
	Method string
	// Ref is the repository reference passed to the call. For CreateFork and
	// CreatePullRequest it is the upstream reference; for ListOrgRepos only
	// Ref.Owner (the org) is set.
	Ref RepoRef
	// ForkName is the requested fork name; populated for CreateFork only.
	ForkName string
	// PullRequest is the pull request input; populated for CreatePullRequest
	// only.
	PullRequest NewPullRequest
}

// Ensure the mock satisfies the interface at compile time.
var _ GitHubClient = (*Mock)(nil)

// NewMock returns a ready-to-use Mock with sensible defaults.
func NewMock() *Mock {
	return &Mock{
		Repos:       make(map[string]RepoState),
		existSeq:    make(map[string][]ExistResult),
		ForkOwner:   "octocat",
		ForkAppears: true,
	}
}

// SetRepo configures the static scripted state for ref. It is safe for
// concurrent use.
func (m *Mock) SetRepo(ref RepoRef, state RepoState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Repos == nil {
		m.Repos = make(map[string]RepoState)
	}
	m.Repos[ref.String()] = state
}

// QueueExists appends a sequence of RepoExists results for ref. Each subsequent
// RepoExists call for ref consumes one result in order; once the queue is
// exhausted the Mock falls back to the static state from SetRepo/Repos.
func (m *Mock) QueueExists(ref RepoRef, results ...ExistResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.existSeq == nil {
		m.existSeq = make(map[string][]ExistResult)
	}
	m.existSeq[ref.String()] = append(m.existSeq[ref.String()], results...)
}

// RepoExists records the call and returns the scripted result for ref.
func (m *Mock) RepoExists(ctx context.Context, ref RepoRef) (bool, error) {
	m.record(Call{Method: "RepoExists", Ref: ref})

	if m.RepoExistsFn != nil {
		return m.RepoExistsFn(ctx, ref)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	key := ref.String()
	if seq := m.existSeq[key]; len(seq) > 0 {
		next := seq[0]
		m.existSeq[key] = seq[1:]
		return next.Exists, next.Err
	}

	state := m.Repos[key]
	if state.Err != nil {
		return false, state.Err
	}
	return state.Exists, nil
}

// DefaultBranch records the call and returns the scripted default branch for
// ref.
func (m *Mock) DefaultBranch(ctx context.Context, ref RepoRef) (string, error) {
	m.record(Call{Method: "DefaultBranch", Ref: ref})

	if m.DefaultBranchFn != nil {
		return m.DefaultBranchFn(ctx, ref)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.Repos[ref.String()]
	if state.Err != nil {
		return "", state.Err
	}
	return state.DefaultBranch, nil
}

// CreateFork records the call and returns a scripted fork reference. When
// CreateForkErr is set it is returned without creating a fork. Otherwise the
// fork reference is {ForkOwner, forkName-or-upstream-name} and, when ForkAppears
// is true, the fork is registered as existing so subsequent RepoExists calls
// resolve it.
func (m *Mock) CreateFork(ctx context.Context, upstream RepoRef, forkName string) (RepoRef, error) {
	m.record(Call{Method: "CreateFork", Ref: upstream, ForkName: forkName})

	if m.CreateForkFn != nil {
		return m.CreateForkFn(ctx, upstream, forkName)
	}

	if m.CreateForkErr != nil {
		return RepoRef{}, m.CreateForkErr
	}

	name := forkName
	if name == "" {
		name = upstream.Name
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	fork := RepoRef{Owner: m.ForkOwner, Name: name}
	if m.ForkAppears {
		if m.Repos == nil {
			m.Repos = make(map[string]RepoState)
		}
		existing := m.Repos[fork.String()]
		existing.Exists = true
		m.Repos[fork.String()] = existing
	}
	return fork, nil
}

// SetOrgRepos configures the repository names ListOrgRepos returns for org.
func (m *Mock) SetOrgRepos(org string, names ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OrgRepos == nil {
		m.OrgRepos = make(map[string][]string)
	}
	m.OrgRepos[org] = append([]string(nil), names...)
}

// ListOrgRepos records the call and returns the scripted repository names for
// org (or ListOrgReposErr when set).
func (m *Mock) ListOrgRepos(ctx context.Context, org string) ([]string, error) {
	m.record(Call{Method: "ListOrgRepos", Ref: RepoRef{Owner: org}})

	if m.ListOrgReposFn != nil {
		return m.ListOrgReposFn(ctx, org)
	}
	if m.ListOrgReposErr != nil {
		return nil, m.ListOrgReposErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.OrgRepos[org]...), nil
}

// DeleteRepo records the call and, by default, removes the repository from the
// scripted state so a subsequent RepoExists resolves it as not-found. When
// DeleteRepoErr is set it is returned without modifying state.
func (m *Mock) DeleteRepo(ctx context.Context, ref RepoRef) error {
	m.record(Call{Method: "DeleteRepo", Ref: ref})

	if m.DeleteRepoFn != nil {
		return m.DeleteRepoFn(ctx, ref)
	}
	if m.DeleteRepoErr != nil {
		return m.DeleteRepoErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Repos, ref.String())
	return nil
}

// CreatePullRequest records the call and returns the scripted PR URL (or
// CreatePullRequestErr when set). When PullRequestURL is empty a synthetic URL
// derived from the upstream reference and head branch is returned.
func (m *Mock) CreatePullRequest(ctx context.Context, upstream RepoRef, in NewPullRequest) (string, error) {
	m.record(Call{Method: "CreatePullRequest", Ref: upstream, PullRequest: in})

	if m.CreatePullRequestFn != nil {
		return m.CreatePullRequestFn(ctx, upstream, in)
	}
	if m.CreatePullRequestErr != nil {
		return "", m.CreatePullRequestErr
	}
	if m.PullRequestURL != "" {
		return m.PullRequestURL, nil
	}
	return "https://github.com/" + upstream.String() + "/pull/1", nil
}

// record appends a call under the mutex.
func (m *Mock) record(c Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, c)
}

// CallsFor returns the recorded calls for the named method, preserving order.
func (m *Mock) CallsFor(method string) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// CallCount returns how many times the named method was invoked.
func (m *Mock) CallCount(method string) int {
	return len(m.CallsFor(method))
}
