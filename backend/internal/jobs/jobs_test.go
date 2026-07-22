package jobs

import (
	"testing"

	"github.com/riverqueue/river/rivertype"
)

// River rejects an insert outright when UniqueOpts.ByState omits any of its
// required states, and the failure surfaces only at enqueue time as a logged
// error — which silently stopped every Plaid sync until it was caught. This
// pins the contract so a future edit cannot reintroduce it.
func TestSyncItemUniqueStatesIncludeRiverRequired(t *testing.T) {
	got := make(map[rivertype.JobState]bool)
	args := SyncItemArgs{}
	for _, s := range args.InsertOpts().UniqueOpts.ByState {
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

	// Also intended: a backing-off item must not stack duplicate syncs.
	if !got[rivertype.JobStateRetryable] {
		t.Error("UniqueOpts.ByState should include retryable")
	}
}
