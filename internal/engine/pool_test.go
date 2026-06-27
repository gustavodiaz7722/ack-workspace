package engine

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// makeTasks builds n tasks that each return a successful Result for a repo named
// "repo-<i>". The repo names are intentionally generated out of sorted order to
// exercise the deterministic sort.
func makeTasks(n int) []Task {
	tasks := make([]Task, n)
	for i := 0; i < n; i++ {
		repo := fmt.Sprintf("repo-%02d", n-i) // reverse order on purpose
		tasks[i] = func(_ context.Context) workspace.Result {
			return workspace.Result{Repo: repo, Outcome: workspace.OutcomeCreated}
		}
	}
	return tasks
}

// TestRunBoundedConcurrency asserts that the observed maximum number of tasks
// running simultaneously never exceeds the configured limit.
//
// Validates: Requirements 7.1, 7.2
func TestRunBoundedConcurrency(t *testing.T) {
	for _, limit := range []int{1, 2, 4, 8} {
		limit := limit
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			var inFlight int32
			var maxInFlight int32

			const numTasks = 64
			tasks := make([]Task, numTasks)
			for i := 0; i < numTasks; i++ {
				repo := fmt.Sprintf("repo-%02d", i)
				tasks[i] = func(_ context.Context) workspace.Result {
					cur := atomic.AddInt32(&inFlight, 1)
					// Track the high-water mark of concurrent executions.
					for {
						prev := atomic.LoadInt32(&maxInFlight)
						if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
							break
						}
					}
					time.Sleep(2 * time.Millisecond)
					atomic.AddInt32(&inFlight, -1)
					return workspace.Result{Repo: repo, Outcome: workspace.OutcomeCreated}
				}
			}

			results := Run(context.Background(), limit, tasks)

			if got := atomic.LoadInt32(&maxInFlight); got > int32(limit) {
				t.Fatalf("observed max in-flight %d exceeds limit %d", got, limit)
			}
			if len(results) != numTasks {
				t.Fatalf("expected %d results, got %d", numTasks, len(results))
			}
		})
	}
}

// TestRunContinueOnError asserts that a task returning a failed Result does not
// stop the other tasks: every task still runs and contributes a Result.
//
// Validates: Requirement 7.4
func TestRunContinueOnError(t *testing.T) {
	const numTasks = 10
	var executed int32

	tasks := make([]Task, numTasks)
	for i := 0; i < numTasks; i++ {
		i := i
		repo := fmt.Sprintf("repo-%02d", i)
		tasks[i] = func(_ context.Context) workspace.Result {
			atomic.AddInt32(&executed, 1)
			if i == 3 { // one task fails
				return workspace.Result{
					Repo:    repo,
					Outcome: workspace.OutcomeFailed,
					Reason:  "boom",
				}
			}
			return workspace.Result{Repo: repo, Outcome: workspace.OutcomeCreated}
		}
	}

	results := Run(context.Background(), 4, tasks)

	if got := atomic.LoadInt32(&executed); got != numTasks {
		t.Fatalf("expected all %d tasks to run, only %d ran", numTasks, got)
	}
	if len(results) != numTasks {
		t.Fatalf("expected %d results, got %d", numTasks, len(results))
	}

	failed := 0
	for _, r := range results {
		if r.Outcome == workspace.OutcomeFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("expected exactly 1 failed result, got %d", failed)
	}
}

// TestRunCompleteAndSorted asserts the result set is complete (one Result per
// task) and sorted deterministically by repo name.
//
// Validates: Requirements 7.4
func TestRunCompleteAndSorted(t *testing.T) {
	const numTasks = 20
	tasks := makeTasks(numTasks)

	results := Run(context.Background(), 4, tasks)

	if len(results) != numTasks {
		t.Fatalf("expected %d results, got %d", numTasks, len(results))
	}

	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.Repo
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("results are not sorted by repo name: %v", names)
	}
}

// TestRunEmptyTasks asserts Run handles an empty task slice gracefully.
func TestRunEmptyTasks(t *testing.T) {
	results := Run(context.Background(), 4, nil)
	if len(results) != 0 {
		t.Fatalf("expected 0 results for empty task slice, got %d", len(results))
	}
}

// TestRunNonPositiveConcurrency asserts a concurrency of <= 0 is defensively
// treated as 1: every task still runs and at most one runs at a time.
func TestRunNonPositiveConcurrency(t *testing.T) {
	var inFlight int32
	var maxInFlight int32

	const numTasks = 8
	tasks := make([]Task, numTasks)
	for i := 0; i < numTasks; i++ {
		repo := fmt.Sprintf("repo-%02d", i)
		tasks[i] = func(_ context.Context) workspace.Result {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return workspace.Result{Repo: repo, Outcome: workspace.OutcomeCreated}
		}
	}

	results := Run(context.Background(), 0, tasks)

	if got := atomic.LoadInt32(&maxInFlight); got > 1 {
		t.Fatalf("with concurrency<=0 treated as 1, observed max in-flight %d", got)
	}
	if len(results) != numTasks {
		t.Fatalf("expected %d results, got %d", numTasks, len(results))
	}
}
