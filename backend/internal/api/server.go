package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/crypto"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/jobs"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
)

// Server holds the dependencies every handler needs.
//
// Plaid, Syncer and Jobs are nil when Plaid is not configured; the Plaid
// handlers check for that and return 503 rather than panicking, so the rest of
// the app runs perfectly well without credentials.
type Server struct {
	Config  config.Config
	Pool    *pgxpool.Pool
	Queries *dbgen.Queries
	Cipher  *crypto.Cipher
	Plaid   *plaid.Client
	Syncer  *plaid.Syncer
	Jobs    *river.Client[pgx.Tx]
}

// NewServer builds a Server from an open connection pool.
func NewServer(cfg config.Config, pool *pgxpool.Pool, cipher *crypto.Cipher) *Server {
	return &Server{
		Config:  cfg,
		Pool:    pool,
		Queries: dbgen.New(pool),
		Cipher:  cipher,
	}
}

// enqueueSync schedules a background sync for an item.
func (s *Server) enqueueSync(itemID uuid.UUID) {
	jobs.EnqueueSync(context.Background(), s.Jobs, itemID)
}

// Routes returns the fully-wired HTTP handler.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(s.cors)

	// Liveness/readiness. Deliberately outside /api and unauthenticated so
	// Docker's healthcheck can reach it.
	r.Get("/healthz", s.handleHealth)

	// Plaid's webhook is mounted outside the /api group on purpose: Plaid is
	// not a browser, so it has neither a session nor a CSRF token. See the
	// handler for why that is safe.
	r.Post("/webhooks/plaid", s.handlePlaidWebhook)

	authMW := auth.Middleware{Queries: s.Queries, Secret: s.Config.SessionSecret}

	r.Route("/api", func(r chi.Router) {
		r.Use(auth.RequireCSRF)

		r.Route("/auth", func(r chi.Router) {
			// Bootstraps the CSRF cookie for clients that do not have one yet.
			r.Get("/csrf", s.handleCSRFToken)
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)

			r.Group(func(r chi.Router) {
				r.Use(authMW.Authenticate)
				r.Get("/me", s.handleMe)
			})
		})

		r.Route("/household", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleGetHousehold)
			r.Get("/members", s.handleListMembers)
			r.Post("/invites", s.handleCreateInvite)
			r.Get("/invites", s.handleListInvites)
			r.Delete("/invites/{inviteID}", s.handleDeleteInvite)
		})

		r.Route("/plaid", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Post("/link-token", s.handleCreateLinkToken)
			r.Post("/exchange", s.handleExchangePublicToken)
			r.Get("/items", s.handleListItems)
			r.Post("/items/{itemID}/sync", s.handleSyncItem)
			r.Patch("/items/{itemID}/sharing", s.handleSetItemSharing)
			r.Delete("/items/{itemID}", s.handleDeleteItem)
		})

		r.Route("/accounts", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListAccounts)
		})

		r.Route("/transactions", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListTransactions)
			r.Patch("/{transactionID}/category", s.handleRecategoriseTransaction)
		})

		r.Route("/categories", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListCategories)
		})

		r.Route("/budgets", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleBudgetProgress)
			r.Post("/", s.handleCreateBudget)
			r.Delete("/{budgetID}", s.handleDeleteBudget)
		})

		r.Route("/networth", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleNetWorth)
			r.Get("/history", s.handleNetWorthHistory)
			r.Post("/snapshot", s.handleSnapshotNow)
			r.Get("/projection", s.handleProjection)
		})

		r.Route("/holdings", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListHoldings)
		})

		r.Route("/liabilities", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListLiabilities)
		})

		r.Route("/manual-assets", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListManualAssets)
			r.Post("/", s.handleCreateManualAsset)
			r.Delete("/{assetID}", s.handleDeleteManualAsset)
		})

		r.Route("/export", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/transactions.csv", s.handleExportTransactions)
			r.Get("/categories.csv", s.handleExportCategorySummary)
			r.Get("/net-worth.csv", s.handleExportNetWorth)
		})

		r.Route("/reports", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/summary", s.handleSummary)
			r.Get("/by-category", s.handleSpendingByCategory)
			r.Get("/trend", s.handleTrend)
			r.Get("/averages", s.handleCategoryAverages)
			r.Get("/merchants", s.handleTopMerchants)
		})
	})

	return r
}

type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

// handleHealth reports process and database health. It returns 503 when the
// database is unreachable so orchestrators stop routing traffic here.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.Pool.Ping(ctx); err != nil {
		slog.Error("health check: database unreachable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable,
			healthResponse{Status: "degraded", Database: "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Database: "ok"})
}

// cors allows the Vite dev server (and the deployed frontend) to call the API
// with cookies. The origin is an exact match from config — never a wildcard,
// which browsers refuse to combine with credentials anyway.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && origin == s.Config.FrontendOrigin {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Content-Type, "+auth.CSRFHeaderName)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger records one structured line per request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

func (s *Server) cookieOptions() auth.CookieOptions {
	return auth.CookieOptions{Secure: s.Config.IsProduction()}
}

// internalError logs the real cause and returns a generic message, so internal
// details never reach the client.
func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
