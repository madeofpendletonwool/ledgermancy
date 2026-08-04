// Command worker runs Ledgermancy's background jobs: Plaid syncs today, and
// alert evaluation and net-worth snapshots as those phases land.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/continuity"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/mailer"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/notify"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Migrations are owned by the api process, so the worker only connects.
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}

	// Without Plaid credentials there is nothing to sync. The queue still
	// starts, so later phases' jobs work regardless.
	var syncer *plaid.Syncer
	if cfg.Plaid.ClientID != "" && cfg.Plaid.Secret != "" {
		plaidClient, err := plaid.New(cfg.Plaid)
		if err != nil {
			return err
		}
		syncer = &plaid.Syncer{
			Pool:    pool,
			Queries: dbgen.New(pool),
			Client:  plaidClient,
			Cipher:  cipher,
		}
		slog.Info("plaid enabled", "env", cfg.Plaid.Env)
	} else {
		slog.Warn("plaid not configured; sync jobs are disabled")
	}

	// Always constructed; a blank API key yields a disabled client and the
	// categorisation jobs are simply not registered.
	aiClient := ai.New(cfg.AI)

	// Always constructed too; delivery is gated per-user inside the notifier, so
	// there is nothing to branch on here.
	notifier := notify.New(cfg.NTFY, dbgen.New(pool))

	// The emailed digest, constructed the same way and for the same reason: with
	// no SMTP_HOST it reports Enabled() == false and every send is a no-op, so
	// the app keeps its "sends no email" posture without a branch here.
	mail := mailer.New(cfg.SMTP)
	if mail.Enabled() {
		slog.Info("smtp configured; the emailed digest is available to members who opt in",
			"host", cfg.SMTP.Host, "security", cfg.SMTP.Security)
	}

	backupDeps, err := buildBackupDeps(cfg, pool, cipher)
	if err != nil {
		return err
	}

	// The one outbound host this app contacts that is neither Plaid nor the AI
	// provider, other than the benchmark fetch. Logged on the way up so an
	// operator can see it in the boot output rather than only in .env.
	if cfg.MerchantLogos.Ready(cfg.AI) {
		slog.Info("merchant logos enabled; merchant names are resolved to domains "+
			"via the AI provider and logos fetched from img.logo.dev",
			"size", cfg.MerchantLogos.Size)
	}

	riverClient, err := jobs.NewWorkerClient(pool, syncer, aiClient, notifier, mail, cfg.FrontendOrigin, cfg.Benchmarks, cfg.MerchantLogos, backupDeps)
	if err != nil {
		return err
	}

	if err := riverClient.Start(ctx); err != nil {
		return err
	}
	slog.Info("worker started", "env", cfg.AppEnv, "ai_enabled", cfg.AI.Enabled())

	<-ctx.Done()
	slog.Info("worker shutting down")

	// Let in-flight jobs finish rather than killing a sync mid-page.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return riverClient.Stop(shutdownCtx)
}

// buildBackupDeps assembles the continuity workers' dependencies.
//
// The document store is opened read-only: the archive job reads every blob and
// must never be able to write to the vault it is copying. That pairs with the
// `:ro` mount in docker-compose.yml — belt and braces, because the two protect
// against different mistakes (a wrong mount, and a wrong call).
//
// A failure here is fatal to startup rather than logged. Backups default on, so
// a worker that cannot construct them is a deployment whose operator believes
// they have backups and does not — and that belief is the exact thing this
// subsystem exists to prevent.
func buildBackupDeps(cfg config.Config, pool *pgxpool.Pool, cipher *crypto.Cipher) (jobs.BackupDeps, error) {
	if !cfg.Backup.Enabled {
		return jobs.BackupDeps{Cfg: cfg.Backup}, nil
	}

	queries := dbgen.New(pool)
	deps := jobs.BackupDeps{
		Cfg:      cfg.Backup,
		Dumper:   &continuity.Dumper{Pool: pool, DatabaseURL: cfg.DatabaseURL},
		Exporter: &continuity.Exporter{Pool: pool},
		Recorder: &continuity.Recorder{Queries: queries},
		Tester: &continuity.RestoreTester{
			Pool:        pool,
			Queries:     queries,
			DatabaseURL: cfg.DatabaseURL,
			Cipher:      cipher,
		},
	}

	if cfg.Backup.IncludeDocuments {
		store, err := documents.NewReadOnlyStorage(cfg.Documents)
		if err != nil {
			return jobs.BackupDeps{}, fmt.Errorf("open document store for backup: %w", err)
		}
		deps.Archiver = &continuity.Archiver{Queries: queries, Store: store}
		slog.Info("document vault will be archived with each backup", "store", store.Describe())
	}

	// Fail at boot on a pg_dump that cannot produce a restorable archive,
	// rather than at 3am on the first scheduled run. The check is the same one
	// the job runs; doing it here just moves the discovery to a deploy, where
	// somebody is watching.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := deps.Dumper.CheckVersions(ctx); err != nil {
		return jobs.BackupDeps{}, err
	}

	slog.Info("backups enabled",
		"dir", cfg.Backup.Dir,
		"mirror", cfg.Backup.MirrorDir,
		"interval", cfg.Backup.Interval,
		"restore_test_interval", cfg.Backup.RestoreTestInterval)
	return deps, nil
}
