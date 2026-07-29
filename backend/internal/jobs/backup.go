package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/continuity"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Continuity jobs: the scheduled dump, vault archive, portable export, mirror
// copy, and the restore test that makes the rest mean something.
//
// These run in the worker rather than in a sidecar container. The deciding
// reason is not tidiness — it is that the restore test has to compare row
// counts against the live database, read the coverage registry to know which
// tables to compare, and write its findings to backup_runs. All three of those
// are Go, so a shell sidecar would end up calling back into the app anyway,
// while adding a container to keep running, patched, and monitored. A
// self-hosted deployment does not need a second thing that can silently die.

// BackupArgs runs one full backup cycle: dump, archive, export, then mirror and
// prune.
//
// One job rather than four, because they are one unit to an operator: a cycle
// where the dump succeeded and the archive did not is not "three quarters
// backed up", it is a restore that will be missing every document. Each step
// still records its own backup_runs row, so the panel reports them separately.
type BackupArgs struct{}

func (BackupArgs) Kind() string { return "backup" }

// InsertOpts collapses overlapping requests. A dump can take minutes on a large
// database, and the "run now" button plus a periodic tick landing together must
// not start two pg_dumps against the same server.
func (BackupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: 30 * time.Minute,
		},
	}
}

// BackupWorker performs the cycle. Every field it needs to do real work may be
// nil in a deployment with backups switched off; the worker is simply not
// registered in that case, so it never has to branch.
type BackupWorker struct {
	river.WorkerDefaults[BackupArgs]
	Cfg      config.BackupConfig
	Dumper   *continuity.Dumper
	Archiver *continuity.Archiver
	Exporter *continuity.Exporter
	Recorder *continuity.Recorder
}

// Timeout bounds a cycle that dumps and re-reads the whole database. Generous
// because the alternative — a timeout that kills a legitimate dump on a large
// install — produces a nightly failure that looks like corruption.
func (w *BackupWorker) Timeout(*river.Job[BackupArgs]) time.Duration {
	return 45 * time.Minute
}

func (w *BackupWorker) Work(ctx context.Context, job *river.Job[BackupArgs]) error {
	// Each step records its own outcome and none of them aborts the cycle. A
	// failed archive must not cost the dump that already succeeded — partial
	// coverage recorded honestly is worth much more than an all-or-nothing job
	// that leaves nothing behind and no explanation.
	w.dump(ctx)
	if w.Cfg.IncludeDocuments && w.Archiver != nil {
		w.archive(ctx)
	}
	w.export(ctx)
	w.prune()
	return nil
}

func (w *BackupWorker) dump(ctx context.Context) {
	started := time.Now()
	artefact, err := w.Dumper.Dump(ctx, w.Cfg.Dir)
	w.Recorder.Record(ctx, continuity.Run{
		Kind:         continuity.KindDBDump,
		StartedAt:    started,
		FinishedAt:   time.Now(),
		SizeBytes:    artefact.Size,
		Destination:  w.Cfg.Dir,
		ArtifactPath: artefact.Path,
		Err:          err,
	})
	if err == nil {
		w.mirror(ctx, artefact)
	}
}

func (w *BackupWorker) archive(ctx context.Context) {
	started := time.Now()
	result, err := w.Archiver.Archive(ctx, w.Cfg.Dir)

	detail := fmt.Sprintf("%d document blobs archived", result.Blobs)
	if result.Missing > 0 {
		// Surfaced rather than buried: the database referencing blobs the store
		// does not have is a real problem, and the backup is the place it
		// becomes visible.
		detail += fmt.Sprintf("; %d referenced blobs were missing from storage and are NOT in this archive", result.Missing)
	}

	w.Recorder.Record(ctx, continuity.Run{
		Kind:         continuity.KindDocumentsArchive,
		StartedAt:    started,
		FinishedAt:   time.Now(),
		SizeBytes:    result.Artefact.Size,
		Destination:  w.Cfg.Dir,
		ArtifactPath: result.Artefact.Path,
		Detail:       detail,
		Err:          err,
	})
	if err == nil {
		w.mirror(ctx, result.Artefact)
	}
}

func (w *BackupWorker) export(ctx context.Context) {
	started := time.Now()
	artefact, err := w.Exporter.Write(ctx, w.Cfg.Dir)
	w.Recorder.Record(ctx, continuity.Run{
		Kind:         continuity.KindExport,
		StartedAt:    started,
		FinishedAt:   time.Now(),
		SizeBytes:    artefact.Size,
		Destination:  w.Cfg.Dir,
		ArtifactPath: artefact.Path,
		Err:          err,
	})
	if err == nil {
		w.mirror(ctx, artefact)
	}
}

// mirror copies one artefact to the second destination, when configured.
//
// Copied per artefact rather than as a directory sync at the end of the cycle,
// so a mirror that is unreachable costs one artefact's record and not the whole
// run — and so the mirror never contains a file the primary does not.
func (w *BackupWorker) mirror(ctx context.Context, artefact continuity.Artefact) {
	if w.Cfg.MirrorDir == "" {
		return
	}
	started := time.Now()
	path, size, err := continuity.CopyFile(artefact.Path, continuity.Dir(w.Cfg.MirrorDir, artefact.Kind))
	w.Recorder.Record(ctx, continuity.Run{
		Kind:         continuity.KindMirrorPush,
		StartedAt:    started,
		FinishedAt:   time.Now(),
		SizeBytes:    size,
		Destination:  w.Cfg.MirrorDir,
		ArtifactPath: path,
		Detail:       fmt.Sprintf("mirrored %s", artefact.Kind),
		Err:          err,
	})
}

// prune applies retention to both destinations.
//
// Deliberately last. Pruning before writing the new artefact would, on a full
// disk, delete an old backup to make room for one that then fails — trading a
// good backup for no backup.
func (w *BackupWorker) prune() {
	policy := continuity.Policy{
		Daily:   w.Cfg.KeepDaily,
		Weekly:  w.Cfg.KeepWeekly,
		Monthly: w.Cfg.KeepMonthly,
	}
	roots := []string{w.Cfg.Dir}
	if w.Cfg.MirrorDir != "" {
		roots = append(roots, w.Cfg.MirrorDir)
	}

	for _, root := range roots {
		for _, kind := range []string{
			continuity.KindDBDump,
			continuity.KindDocumentsArchive,
			continuity.KindExport,
		} {
			removed, err := continuity.Apply(root, kind, policy)
			if err != nil {
				slog.Warn("prune backups", "error", err, "root", root, "kind", kind)
			}
			if removed > 0 {
				slog.Info("pruned backups", "root", root, "kind", kind, "removed", removed)
			}
		}
	}
}

// --------------------------------------------------------------------------
// Restore test
// --------------------------------------------------------------------------

// RestoreTestArgs restores the newest dump into a scratch database and verifies
// it. This is the only job here that produces evidence rather than artefacts.
type RestoreTestArgs struct{}

func (RestoreTestArgs) Kind() string { return "restore_test" }

func (RestoreTestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Hour,
		},
	}
}

type RestoreTestWorker struct {
	river.WorkerDefaults[RestoreTestArgs]
	Cfg      config.BackupConfig
	Tester   *continuity.RestoreTester
	Recorder *continuity.Recorder
}

func (w *RestoreTestWorker) Timeout(*river.Job[RestoreTestArgs]) time.Duration {
	return 45 * time.Minute
}

func (w *RestoreTestWorker) Work(ctx context.Context, job *river.Job[RestoreTestArgs]) error {
	started := time.Now()
	report, err := w.Tester.Run(ctx, w.Cfg.Dir, w.Cfg.Dir)

	w.Recorder.Record(ctx, continuity.Run{
		Kind:        continuity.KindRestoreTest,
		StartedAt:   started,
		FinishedAt:  time.Now(),
		Destination: w.Cfg.Dir,
		Detail:      report.String(),
		Err:         err,
	})

	// The outcome is recorded, so returning the error would only make River
	// retry a 20-minute restore that will fail identically. A failing restore
	// test is a standing condition for the operator to act on, not a transient
	// fault to back off from.
	return nil
}

// --------------------------------------------------------------------------
// Registration
// --------------------------------------------------------------------------

// registerBackupJobs wires the continuity workers and their schedules.
//
// Everything here is skipped when backups are switched off, so an operator who
// deliberately opted out has no backup workers registered at all and no
// enqueued job can run by accident — the same shape as the benchmark fetcher.
func registerBackupJobs(
	workers *river.Workers,
	periodic *[]*river.PeriodicJob,
	deps BackupDeps,
) error {
	if !deps.Cfg.Enabled {
		slog.Warn("backups are disabled; this deployment has no automated recovery path",
			"set", "BACKUP_ENABLED=true")
		return nil
	}

	if err := river.AddWorkerSafely(workers, &BackupWorker{
		Cfg:      deps.Cfg,
		Dumper:   deps.Dumper,
		Archiver: deps.Archiver,
		Exporter: deps.Exporter,
		Recorder: deps.Recorder,
	}); err != nil {
		return fmt.Errorf("register backup worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, &RestoreTestWorker{
		Cfg:      deps.Cfg,
		Tester:   deps.Tester,
		Recorder: deps.Recorder,
	}); err != nil {
		return fmt.Errorf("register restore test worker: %w", err)
	}

	// RunOnStart for the backup: a fresh deploy, or one that has been down,
	// should have a current backup within minutes rather than waiting out a
	// full interval. Dumps are idempotent in the only sense that matters —
	// running an extra one costs a file that retention will collect.
	*periodic = append(*periodic, river.NewPeriodicJob(
		river.PeriodicInterval(deps.Cfg.Interval),
		func() (river.JobArgs, *river.InsertOpts) { return BackupArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	// NOT RunOnStart. The restore test restores the entire database into a
	// scratch copy, so firing it on every worker restart would turn a crash
	// loop into sustained heavy load on the database it is meant to protect.
	*periodic = append(*periodic, river.NewPeriodicJob(
		river.PeriodicInterval(deps.Cfg.RestoreTestInterval),
		func() (river.JobArgs, *river.InsertOpts) { return RestoreTestArgs{}, nil },
		nil,
	))

	return nil
}

// BackupDeps groups what the continuity workers need, so NewWorkerClient's
// signature does not grow five more parameters.
type BackupDeps struct {
	Cfg      config.BackupConfig
	Dumper   *continuity.Dumper
	Archiver *continuity.Archiver
	Exporter *continuity.Exporter
	Tester   *continuity.RestoreTester
	Recorder *continuity.Recorder
}

// EnqueueBackup runs a backup cycle now — the "back up now" button. It returns
// the error, unlike the fire-and-forget enqueues, because the operator is
// standing in front of the result.
func EnqueueBackup(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if client == nil {
		return fmt.Errorf("background jobs are not available")
	}
	if _, err := client.Insert(ctx, BackupArgs{}, nil); err != nil {
		return fmt.Errorf("enqueue backup: %w", err)
	}
	return nil
}

// EnqueueRestoreTest runs a restore test now. Same reasoning as EnqueueBackup,
// and more useful: an operator who has just changed something about their
// backups wants to know it still restores without waiting a week to find out.
func EnqueueRestoreTest(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if client == nil {
		return fmt.Errorf("background jobs are not available")
	}
	if _, err := client.Insert(ctx, RestoreTestArgs{}, nil); err != nil {
		return fmt.Errorf("enqueue restore test: %w", err)
	}
	return nil
}

// RecordKeyAcknowledgement stores an operator's confirmation that they have
// stored ENCRYPTION_KEY somewhere safe.
//
// The app cannot verify the claim and does not pretend to. Asking is the point:
// the key is the one component of a restore that lives nowhere in this system,
// and an operator who has never been asked the question has usually never
// answered it either.
//
// TryRecord rather than Record, so a failed write reaches the person who just
// clicked the button. Here the row is the outcome, not a note about one.
func RecordKeyAcknowledgement(ctx context.Context, queries *dbgen.Queries, who string) error {
	now := time.Now()
	rec := &continuity.Recorder{Queries: queries}
	return rec.TryRecord(ctx, continuity.Run{
		Kind:       continuity.KindKeyAck,
		StartedAt:  now,
		FinishedAt: now,
		Detail:     fmt.Sprintf("ENCRYPTION_KEY backup confirmed by %s", who),
	})
}
