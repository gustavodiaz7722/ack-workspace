// Package engine provides the bounded-concurrency worker pool and result
// aggregation used by all batch commands. At most `concurrency` tasks run
// simultaneously; every task runs (continue-on-error) and all results are
// returned, sorted deterministically by repository name.
package engine

import (
	"context"
	"sort"
	"sync"

	"github.com/aws-controllers-k8s/ack-workspace/internal/workspace"
)

// Task processes a single repository and returns its terminal Result. A Task
// must capture its own failure into the returned Result rather than panicking
// or signalling an error out-of-band; the engine never cancels sibling tasks on
// a task's failure (continue-on-error).
type Task func(ctx context.Context) workspace.Result

// Run executes tasks with at most `concurrency` of them running simultaneously.
//
// It always runs every task (continue-on-error): a failure captured by one task
// never prevents another task from running. Run collects a Result from every
// task and returns the full set sorted deterministically by repository name
// (Result.Repo), so output is stable regardless of completion order.
//
// A concurrency value of zero or less is treated as 1 (defensive; the accepted
// 1..32 range is validated during flag/config resolution elsewhere).
//
// Validates: Requirements 7.1, 7.2, 7.4
func Run(ctx context.Context, concurrency int, tasks []Task) []workspace.Result {
	if concurrency <= 0 {
		concurrency = 1
	}

	results := make([]workspace.Result, len(tasks))
	// Bounded semaphore: at most `concurrency` tokens are held at once, so no
	// more than `concurrency` tasks execute simultaneously.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, t Task) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = t(ctx)
		}(i, task)
	}

	wg.Wait()

	sort.SliceStable(results, func(a, b int) bool {
		return results[a].Repo < results[b].Repo
	})
	return results
}
