// Command worker runs Ledgermancy's background jobs: Plaid syncs today, and
// alert evaluation and net-worth snapshots as those phases land.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/crypto"
	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/jobs"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
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

	riverClient, err := jobs.NewWorkerClient(pool, syncer, aiClient)
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
