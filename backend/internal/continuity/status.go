package continuity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Recorder writes outcomes to backup_runs.
//
// Failures are recorded exactly as carefully as successes, which is the point:
// a run that failed and left no trace is indistinguishable from a run that
// never happened, and the continuity panel's whole job is to tell those apart.
// "Nothing has run for six days" and "it has run every night and failed every
// night" call for very different actions from the operator.
type Recorder struct {
	Queries *dbgen.Queries
}

// Run is one attempt's outcome.
type Run struct {
	Kind         string
	StartedAt    time.Time
	FinishedAt   time.Time
	SizeBytes    int64
	Destination  string
	ArtifactPath string
	Detail       string
	Err          error
}

// Record persists an attempt for a background job. It never returns an error: a
// backup that succeeded and could not be recorded is still a backup, and
// failing the job over the bookkeeping would turn a cosmetic problem into a
// missed nightly dump.
//
// Use TryRecord instead when a person is waiting on the answer — see the note
// there.
func (r *Recorder) Record(ctx context.Context, run Run) {
	if err := r.TryRecord(ctx, run); err != nil {
		slog.Error("record backup run", "error", err, "kind", run.Kind)
	}
}

// TryRecord persists an attempt and reports whether it stuck.
//
// The distinction from Record matters for exactly one caller: the key
// acknowledgement, where the row *is* the outcome rather than a note about one.
// Swallowing a write failure there would tell an operator their key backup was
// recorded when it was not — which is the precise species of false confidence
// this whole subsystem exists to remove.
func (r *Recorder) TryRecord(ctx context.Context, run Run) error {
	status := "success"
	detail := run.Detail
	if run.Err != nil {
		status = "failure"
		if detail == "" {
			detail = run.Err.Error()
		} else {
			detail = run.Err.Error() + "\n\n" + detail
		}
	}
	if run.FinishedAt.IsZero() {
		run.FinishedAt = time.Now()
	}

	params := dbgen.RecordBackupRunParams{
		Kind:       run.Kind,
		Status:     status,
		StartedAt:  run.StartedAt,
		FinishedAt: run.FinishedAt,
	}
	if run.SizeBytes > 0 {
		params.SizeBytes = &run.SizeBytes
	}
	if run.Destination != "" {
		params.Destination = &run.Destination
	}
	if run.ArtifactPath != "" {
		params.ArtifactPath = &run.ArtifactPath
	}
	if detail != "" {
		params.Detail = &detail
	}

	if _, err := r.Queries.RecordBackupRun(ctx, params); err != nil {
		return fmt.Errorf("record %s run: %w", run.Kind, err)
	}

	if run.Err != nil {
		slog.Error("continuity job failed", "kind", run.Kind, "error", run.Err)
	} else {
		slog.Info("continuity job complete",
			"kind", run.Kind,
			"size_bytes", run.SizeBytes,
			"duration_ms", run.FinishedAt.Sub(run.StartedAt).Milliseconds())
	}
	return nil
}

// --------------------------------------------------------------------------
// Panel status
// --------------------------------------------------------------------------

// Health is the traffic light for one line of the continuity panel.
type Health string

const (
	HealthGood     Health = "good"
	HealthWarn     Health = "warn"
	HealthBad      Health = "bad"
	HealthOff      Health = "off"   // deliberately not configured
	HealthNeverRun Health = "never" // configured, has never produced a result
)

// Thresholds are how old a successful run of each kind may be before the panel
// stops calling it healthy.
//
// The dump's window is short because a lost day is a lost day. The restore
// test's is long because it is expensive and because what it verifies changes
// with the schema rather than with the calendar — but it is not unlimited,
// because "we tested it once, in March" is how operators end up restoring from
// something that stopped working in April.
var thresholds = map[string]struct{ Good, Warn time.Duration }{
	KindDBDump:           {Good: 48 * time.Hour, Warn: 7 * 24 * time.Hour},
	KindDocumentsArchive: {Good: 48 * time.Hour, Warn: 7 * 24 * time.Hour},
	KindExport:           {Good: 7 * 24 * time.Hour, Warn: 30 * 24 * time.Hour},
	KindRestoreTest:      {Good: 14 * 24 * time.Hour, Warn: 45 * 24 * time.Hour},
	KindMirrorPush:       {Good: 48 * time.Hour, Warn: 7 * 24 * time.Hour},
	KindKeyAck:           {Good: 365 * 24 * time.Hour, Warn: 730 * 24 * time.Hour},
}

// Classify grades one kind's latest run.
//
// A failed run is bad regardless of age — a fresh failure is worse than a stale
// success, not better, and grading purely on recency would paint a nightly
// crash green for two days.
func ClassifyRun(kind string, status string, at time.Time, now time.Time) Health {
	if status == "failure" {
		return HealthBad
	}
	t, ok := thresholds[kind]
	if !ok {
		return HealthGood
	}
	age := now.Sub(at)
	switch {
	case age <= t.Good:
		return HealthGood
	case age <= t.Warn:
		return HealthWarn
	default:
		return HealthBad
	}
}

// Age renders a duration the way the panel says it out loud.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
