package workspace

import (
	"errors"
	"testing"
)

func TestSummaryCount(t *testing.T) {
	tests := []struct {
		name    string
		summary Summary
		want    map[Outcome]int
	}{
		{
			name:    "empty result set",
			summary: Summary{},
			want:    map[Outcome]int{OutcomeCreated: 0, OutcomeSkipped: 0, OutcomeFailed: 0},
		},
		{
			name: "all success",
			summary: Summary{Results: []Result{
				{Repo: "runtime", Outcome: OutcomeCreated},
				{Repo: "code-generator", Outcome: OutcomeCreated},
				{Repo: "test-infra", Outcome: OutcomeCreated},
			}},
			want: map[Outcome]int{OutcomeCreated: 3, OutcomeSkipped: 0, OutcomeFailed: 0},
		},
		{
			name: "mixed outcomes",
			summary: Summary{Results: []Result{
				{Repo: "runtime", Outcome: OutcomeCreated},
				{Repo: "code-generator", Outcome: OutcomeSkipped, Reason: "already present"},
				{Repo: "test-infra", Outcome: OutcomeFailed, Reason: "clone failed", Err: errors.New("boom")},
				{Repo: "s3-controller", Outcome: OutcomeSkipped, Reason: "already present"},
			}},
			want: map[Outcome]int{OutcomeCreated: 1, OutcomeSkipped: 2, OutcomeFailed: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for outcome, want := range tt.want {
				if got := tt.summary.Count(outcome); got != want {
					t.Errorf("Count(%q) = %d, want %d", outcome, got, want)
				}
			}
		})
	}
}

func TestSummaryHasFailures(t *testing.T) {
	tests := []struct {
		name    string
		summary Summary
		want    bool
	}{
		{
			name:    "empty result set has no failures",
			summary: Summary{},
			want:    false,
		},
		{
			name: "all success has no failures",
			summary: Summary{Results: []Result{
				{Repo: "runtime", Outcome: OutcomeCreated},
				{Repo: "code-generator", Outcome: OutcomeSkipped},
			}},
			want: false,
		},
		{
			name: "mixed with a failure has failures",
			summary: Summary{Results: []Result{
				{Repo: "runtime", Outcome: OutcomeCreated},
				{Repo: "test-infra", Outcome: OutcomeFailed, Err: errors.New("boom")},
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.summary.HasFailures(); got != tt.want {
				t.Errorf("HasFailures() = %v, want %v", got, tt.want)
			}
		})
	}
}
