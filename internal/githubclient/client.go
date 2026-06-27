// Package githubclient defines the GitHubClient interface and its go-github
// adapter and mock. It performs GitHub API operations: resolving repository
// existence and metadata, detecting existing forks, and creating forks.
package githubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

// RepoRef identifies a GitHub repository by its owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

// String renders the reference in the conventional "owner/name" form.
func (r RepoRef) String() string {
	return r.Owner + "/" + r.Name
}

// GitHubClient performs the GitHub API operations needed by ack-workspace:
// resolving repository existence and metadata, and creating forks.
type GitHubClient interface {
	// RepoExists reports whether owner/name resolves to an existing repository.
	// It distinguishes a "not found" result (false, nil) from transport or API
	// errors (false, err).
	RepoExists(ctx context.Context, ref RepoRef) (bool, error)
	// DefaultBranch returns the default branch name of the referenced repository
	// (for example, "main").
	DefaultBranch(ctx context.Context, ref RepoRef) (string, error)
	// CreateFork forks upstream into the authenticated user's account, renaming
	// the fork to forkName when it is non-empty. Because GitHub creates forks
	// asynchronously, CreateFork polls until the fork is queryable and returns a
	// *ForkTimeoutError if the fork does not become available within the
	// configured timeout, so callers can record the repository as failed rather
	// than surfacing a clone error.
	CreateFork(ctx context.Context, upstream RepoRef, forkName string) (RepoRef, error)
	// ListOrgRepos returns the names of the non-archived repositories in the
	// given organization, following pagination. Archived repositories are
	// excluded because they are not useful contributor targets.
	ListOrgRepos(ctx context.Context, org string) ([]string, error)
	// DeleteRepo permanently deletes the referenced repository. It is used to
	// delete a contributor's fork; callers must never pass an upstream
	// (organization) repository. Requires a token with the delete_repo scope.
	DeleteRepo(ctx context.Context, ref RepoRef) error
}

// ForkTimeoutError is returned by CreateFork when a newly requested fork does
// not become queryable within the configured polling timeout.
type ForkTimeoutError struct {
	// Fork is the reference that was being polled.
	Fork RepoRef
	// Waited is the total time spent waiting before giving up.
	Waited time.Duration
}

func (e *ForkTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s waiting for fork %s to become available", e.Waited, e.Fork)
}

const (
	// defaultPollInterval is the delay between fork-availability polls.
	defaultPollInterval = 2 * time.Second
	// defaultPollTimeout bounds how long CreateFork waits for a fork to appear.
	defaultPollTimeout = 30 * time.Second
)

// Adapter implements GitHubClient over the google/go-github REST client.
type Adapter struct {
	rest *github.Client

	pollInterval time.Duration
	pollTimeout  time.Duration

	// now and sleep are injectable to keep fork-polling tests fast and
	// deterministic.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Ensure the adapter satisfies the interface at compile time.
var _ GitHubClient = (*Adapter)(nil)

// Option customizes an Adapter. Polling parameters are exposed so tests can use
// small values.
type Option func(*Adapter)

// WithPollInterval sets the delay between fork-availability polls.
func WithPollInterval(d time.Duration) Option {
	return func(a *Adapter) {
		if d > 0 {
			a.pollInterval = d
		}
	}
}

// WithPollTimeout sets the upper bound on how long CreateFork waits for a fork
// to become queryable.
func WithPollTimeout(d time.Duration) Option {
	return func(a *Adapter) {
		if d > 0 {
			a.pollTimeout = d
		}
	}
}

// NewAdapter builds an Adapter authenticated with the supplied token using an
// oauth2 static token source.
func NewAdapter(ctx context.Context, token string, opts ...Option) *Adapter {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	return newAdapter(github.NewClient(tc), opts...)
}

// newAdapter wires defaults around an already-constructed go-github client. It
// is separated from NewAdapter so tests can inject a client backed by a stubbed
// HTTP transport.
func newAdapter(rest *github.Client, opts ...Option) *Adapter {
	a := &Adapter{
		rest:         rest,
		pollInterval: defaultPollInterval,
		pollTimeout:  defaultPollTimeout,
		now:          time.Now,
		sleep:        sleepContext,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// RepoExists reports whether the referenced repository exists, mapping a 404 to
// (false, nil) and any other failure to (false, err).
func (a *Adapter) RepoExists(ctx context.Context, ref RepoRef) (bool, error) {
	_, resp, err := a.rest.Repositories.Get(ctx, ref.Owner, ref.Name)
	if err != nil {
		if isNotFound(resp, err) {
			return false, nil
		}
		return false, fmt.Errorf("checking repository %s: %w", ref, err)
	}
	return true, nil
}

// ListOrgRepos returns the names of the non-archived repositories in org,
// following pagination until every page has been read. Archived repositories
// are skipped because they are not useful contributor targets.
func (a *Adapter) ListOrgRepos(ctx context.Context, org string) ([]string, error) {
	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var names []string
	for {
		repos, resp, err := a.rest.Repositories.ListByOrg(ctx, org, opts)
		if err != nil {
			return nil, fmt.Errorf("listing repositories for organization %q: %w", org, err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			names = append(names, r.GetName())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return names, nil
}

// DeleteRepo permanently deletes the referenced repository via the GitHub API.
func (a *Adapter) DeleteRepo(ctx context.Context, ref RepoRef) error {
	if _, err := a.rest.Repositories.Delete(ctx, ref.Owner, ref.Name); err != nil {
		return fmt.Errorf("deleting repository %s: %w", ref, err)
	}
	return nil
}

// DefaultBranch returns the default branch name of the referenced repository.
func (a *Adapter) DefaultBranch(ctx context.Context, ref RepoRef) (string, error) {
	repo, _, err := a.rest.Repositories.Get(ctx, ref.Owner, ref.Name)
	if err != nil {
		return "", fmt.Errorf("resolving default branch for %s: %w", ref, err)
	}
	return repo.GetDefaultBranch(), nil
}

// CreateFork issues the fork request and then polls for the fork to become
// queryable, returning a *ForkTimeoutError if it does not appear in time.
func (a *Adapter) CreateFork(ctx context.Context, upstream RepoRef, forkName string) (RepoRef, error) {
	opts := &github.RepositoryCreateForkOptions{}
	if forkName != "" {
		opts.Name = forkName
	}

	repo, _, err := a.rest.Repositories.CreateFork(ctx, upstream.Owner, upstream.Name, opts)
	// GitHub returns 202 Accepted (surfaced as *github.AcceptedError) while the
	// fork is created asynchronously; that is expected and not a failure.
	var acceptedErr *github.AcceptedError
	if err != nil && !errors.As(err, &acceptedErr) {
		return RepoRef{}, fmt.Errorf("creating fork of %s: %w", upstream, err)
	}

	fork, err := a.resolveForkRef(ctx, repo, forkName)
	if err != nil {
		return RepoRef{}, err
	}

	start := a.now()
	for {
		exists, err := a.RepoExists(ctx, fork)
		if err != nil {
			return RepoRef{}, err
		}
		if exists {
			return fork, nil
		}
		if a.now().Sub(start) >= a.pollTimeout {
			return RepoRef{}, &ForkTimeoutError{Fork: fork, Waited: a.pollTimeout}
		}
		if err := a.sleep(ctx, a.pollInterval); err != nil {
			return RepoRef{}, err
		}
	}
}

// resolveForkRef determines the owner/name of the new fork, preferring the
// repository returned by CreateFork and falling back to the authenticated user
// when the response did not include an owner.
func (a *Adapter) resolveForkRef(ctx context.Context, repo *github.Repository, forkName string) (RepoRef, error) {
	ref := RepoRef{}
	if repo != nil {
		ref.Owner = repo.GetOwner().GetLogin()
		ref.Name = repo.GetName()
	}
	if forkName != "" {
		ref.Name = forkName
	}
	if ref.Owner == "" {
		user, _, err := a.rest.Users.Get(ctx, "")
		if err != nil {
			return RepoRef{}, fmt.Errorf("resolving authenticated user for fork owner: %w", err)
		}
		ref.Owner = user.GetLogin()
	}
	return ref, nil
}

// isNotFound reports whether the error/response pair represents an HTTP 404.
func isNotFound(resp *github.Response, err error) bool {
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return true
	}
	var errResp *github.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		return errResp.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// sleepContext sleeps for d, returning early if the context is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
