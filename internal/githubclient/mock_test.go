package githubclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockRepoExistsStates(t *testing.T) {
	ctx := context.Background()
	apiErr := errors.New("api error")

	present := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}
	missing := RepoRef{Owner: "aws-controllers-k8s", Name: "absent"}
	broken := RepoRef{Owner: "aws-controllers-k8s", Name: "broken"}

	m := NewMock()
	m.SetRepo(present, RepoState{Exists: true, DefaultBranch: "main"})
	m.SetRepo(broken, RepoState{Err: apiErr})

	// exists
	if ok, err := m.RepoExists(ctx, present); err != nil || !ok {
		t.Fatalf("present: got (%v, %v), want (true, nil)", ok, err)
	}
	// not-found (unconfigured key)
	if ok, err := m.RepoExists(ctx, missing); err != nil || ok {
		t.Fatalf("missing: got (%v, %v), want (false, nil)", ok, err)
	}
	// API error
	if ok, err := m.RepoExists(ctx, broken); !errors.Is(err, apiErr) || ok {
		t.Fatalf("broken: got (%v, %v), want (false, apiErr)", ok, err)
	}

	if got := m.CallCount("RepoExists"); got != 3 {
		t.Fatalf("RepoExists call count = %d, want 3", got)
	}
}

func TestMockQueueExistsSequence(t *testing.T) {
	ctx := context.Background()
	fork := RepoRef{Owner: "octocat", Name: "ack-runtime"}

	m := NewMock()
	// fork-missing on the first poll, fork-present on the second.
	m.QueueExists(fork,
		ExistResult{Exists: false},
		ExistResult{Exists: true},
	)
	// after the queue drains, fall back to static state.
	m.SetRepo(fork, RepoState{Exists: true})

	if ok, _ := m.RepoExists(ctx, fork); ok {
		t.Fatalf("first poll: want fork missing")
	}
	if ok, _ := m.RepoExists(ctx, fork); !ok {
		t.Fatalf("second poll: want fork present")
	}
	if ok, _ := m.RepoExists(ctx, fork); !ok {
		t.Fatalf("third poll: want static fallback present")
	}
}

func TestMockDefaultBranch(t *testing.T) {
	ctx := context.Background()
	ref := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	m := NewMock()
	m.SetRepo(ref, RepoState{Exists: true, DefaultBranch: "main"})

	got, err := m.DefaultBranch(ctx, ref)
	if err != nil || got != "main" {
		t.Fatalf("DefaultBranch = (%q, %v), want (main, nil)", got, err)
	}
}

func TestMockCreateForkPresent(t *testing.T) {
	ctx := context.Background()
	upstream := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	m := NewMock() // ForkAppears defaults to true
	fork, err := m.CreateFork(ctx, upstream, "ack-runtime")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := RepoRef{Owner: "octocat", Name: "ack-runtime"}
	if fork != want {
		t.Fatalf("fork = %+v, want %+v", fork, want)
	}
	// The created fork is now queryable.
	if ok, _ := m.RepoExists(ctx, fork); !ok {
		t.Fatalf("created fork should be present afterward")
	}

	calls := m.CallsFor("CreateFork")
	if len(calls) != 1 || calls[0].Ref != upstream || calls[0].ForkName != "ack-runtime" {
		t.Fatalf("recorded CreateFork call = %+v, want upstream=%v forkName=ack-runtime", calls, upstream)
	}
}

func TestMockCreateForkMissing(t *testing.T) {
	ctx := context.Background()
	upstream := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	m := NewMock()
	m.ForkAppears = false

	fork, err := m.CreateFork(ctx, upstream, "ack-runtime")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok, _ := m.RepoExists(ctx, fork); ok {
		t.Fatalf("fork-missing scenario: fork should not be queryable")
	}
}

func TestMockCreateForkError(t *testing.T) {
	ctx := context.Background()
	upstream := RepoRef{Owner: "aws-controllers-k8s", Name: "runtime"}

	// Arbitrary create failure.
	createErr := errors.New("fork failed")
	m := NewMock()
	m.CreateForkErr = createErr
	if _, err := m.CreateFork(ctx, upstream, "ack-runtime"); !errors.Is(err, createErr) {
		t.Fatalf("got %v, want %v", err, createErr)
	}

	// Fork-create timeout via a typed error.
	timeout := &ForkTimeoutError{Fork: RepoRef{Owner: "octocat", Name: "ack-runtime"}, Waited: 30 * time.Second}
	m2 := NewMock()
	m2.CreateForkErr = timeout
	_, err := m2.CreateFork(ctx, upstream, "ack-runtime")
	var timeoutErr *ForkTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("got %v, want *ForkTimeoutError", err)
	}
}

func TestMockListOrgRepos(t *testing.T) {
	ctx := context.Background()
	const org = "aws-controllers-k8s"

	m := NewMock()
	m.SetOrgRepos(org, "runtime", "s3-controller", "sns-controller")

	got, err := m.ListOrgRepos(ctx, org)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"runtime", "s3-controller", "sns-controller"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// An unconfigured org yields an empty list with no error.
	if repos, err := m.ListOrgRepos(ctx, "other"); err != nil || len(repos) != 0 {
		t.Errorf("expected empty list for unconfigured org, got %v err=%v", repos, err)
	}

	// The call is recorded with the org in Ref.Owner.
	calls := m.CallsFor("ListOrgRepos")
	if len(calls) != 2 {
		t.Fatalf("expected 2 ListOrgRepos calls recorded, got %d", len(calls))
	}
	if calls[0].Ref.Owner != org {
		t.Errorf("recorded org = %q, want %q", calls[0].Ref.Owner, org)
	}
}

func TestMockListOrgReposError(t *testing.T) {
	m := NewMock()
	m.ListOrgReposErr = errors.New("boom")
	if _, err := m.ListOrgRepos(context.Background(), "aws-controllers-k8s"); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestMockDeleteRepo(t *testing.T) {
	ctx := context.Background()
	ref := RepoRef{Owner: "octocat", Name: "ack-s3-controller"}

	m := NewMock()
	m.SetRepo(ref, RepoState{Exists: true})

	if err := m.DeleteRepo(ctx, ref); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// After deletion the repo resolves as not-found.
	if exists, _ := m.RepoExists(ctx, ref); exists {
		t.Errorf("expected repo to be gone after DeleteRepo")
	}
	if m.CallCount("DeleteRepo") != 1 {
		t.Errorf("expected 1 DeleteRepo call, got %d", m.CallCount("DeleteRepo"))
	}
}

func TestMockDeleteRepoError(t *testing.T) {
	m := NewMock()
	m.DeleteRepoErr = errors.New("forbidden")
	if err := m.DeleteRepo(context.Background(), RepoRef{Owner: "octocat", Name: "x"}); err == nil {
		t.Fatal("expected an error, got nil")
	}
}
