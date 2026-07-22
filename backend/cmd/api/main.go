// Command api serves the Ledgermancy HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/apex42group/ledgermancy/backend/internal/api"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/crypto"
	"github.com/apex42group/ledgermancy/backend/internal/db"
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

	// Shut down cleanly on Ctrl-C or the SIGTERM Docker sends on stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	slog.Info("connected to database")

	// The api owns schema migrations; the worker waits on it.
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := jobs.Migrate(ctx, pool); err != nil {
		return err
	}
	slog.Info("migrations applied")

	cipher, err := crypto.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}

	server := api.NewServer(cfg, pool, cipher)

	// Plaid is optional: without credentials the app runs normally and the
	// Plaid endpoints report 503 rather than the process failing to start.
	if cfg.Plaid.ClientID != "" && cfg.Plaid.Secret != "" {
		plaidClient, err := plaid.New(cfg.Plaid)
		if err != nil {
			return err
		}
		riverClient, err := jobs.NewInsertOnlyClient(pool)
		if err != nil {
			return err
		}

		server.Plaid = plaidClient
		server.Jobs = riverClient
		server.Syncer = &plaid.Syncer{
			Pool:    pool,
			Queries: server.Queries,
			Client:  plaidClient,
			Cipher:  cipher,
		}
		slog.Info("plaid enabled", "env", cfg.Plaid.Env, "products", cfg.Plaid.Products)
	} else {
		slog.Warn("plaid not configured; link endpoints will return 503")
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.AppEnv,
			"ai_enabled", cfg.AI.Enabled())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// Give in-flight requests a chance to finish before dropping connections.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
