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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/api"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
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

	// The document vault is optional in the same way Plaid is. Its storage
	// backend is opened here rather than lazily on first upload so a
	// misconfigured volume or bucket is a loud line in the startup log instead
	// of a surprise the first time someone tries to file a receipt.
	if cfg.Documents.Enabled {
		store, err := documents.NewStorage(cfg.Documents)
		if err != nil {
			// Not fatal: the rest of the app is entirely usable without a vault,
			// and refusing to boot over it would take the whole ledger down.
			slog.Error("document vault disabled; storage backend could not be opened",
				"backend", cfg.Documents.Backend, "error", err)
		} else {
			server.Documents = documents.New(cfg.Documents, cipher, store)
			slog.Info("document vault enabled",
				"backend", store.Describe(),
				"max_file_bytes", cfg.Documents.MaxFileBytes,
				"quota_bytes", cfg.Documents.QuotaBytes,
				"ocr_enabled", cfg.Documents.OCREnabled && cfg.AI.Enabled())
		}
	} else {
		slog.Info("document vault disabled by configuration")
	}

	// The queue client is wired regardless of Plaid: alert evaluation and other
	// non-Plaid jobs need to be enqueueable even when no institutions are linked.
	riverClient, err := jobs.NewInsertOnlyClient(pool)
	if err != nil {
		return err
	}
	server.Jobs = riverClient

	// Plaid is optional: without credentials the app runs normally and the
	// Plaid endpoints report 503 rather than the process failing to start.
	if cfg.Plaid.ClientID != "" && cfg.Plaid.Secret != "" {
		plaidClient, err := plaid.New(cfg.Plaid)
		if err != nil {
			return err
		}

		server.Plaid = plaidClient
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
		WriteTimeout:      api.HTTPServerWriteTimeout,
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
