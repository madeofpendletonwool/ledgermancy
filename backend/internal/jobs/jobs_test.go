package jobs

import (
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// River rejects an insert outright when UniqueOpts.ByState omits any of its
// required states, and the failure surfaces only at enqueue time as a logged
// error — which silently stopped every Plaid sync until it was caught. This
// pins the contract so a future edit cannot reintroduce it, for every job that
// sets UniqueOpts.
func TestUniqueStatesIncludeRiverRequired(t *testing.T) {
	cases := map[string]river.InsertOpts{
		"sync_item":      SyncItemArgs{}.InsertOpts(),
		"llm_categorise": LLMCategoriseArgs{}.InsertOpts(),
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			got := make(map[rivertype.JobState]bool)
			for _, s := range opts.UniqueOpts.ByState {
				got[s] = true
			}

			required := []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
			}
			for _, s := range required {
				if !got[s] {
					t.Errorf("UniqueOpts.ByState is missing River-required state %q", s)
				}
			}

			// Also intended: a backing-off job must not stack duplicates.
			if !got[rivertype.JobStateRetryable] {
				t.Error("UniqueOpts.ByState should include retryable")
			}
		})
	}
}
