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

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/crypto"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/jobs"
	"github.com/apex42group/ledgermancy/backend/internal/notify"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
	"github.com/apex42group/ledgermancy/backend/internal/ratelimit"
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
	AI      *ai.Client
	Notify  notify.Notifier

	// Rate limiters, held on the Server so successful logins can reset the
	// caller's counter rather than punishing someone who mistyped once.
	loginLimiter    *ratelimit.Limiter
	registerLimiter *ratelimit.Limiter
	accountLimiter  *ratelimit.Limiter
	generalLimiter  *ratelimit.Limiter
}

// Rate limits. Login and registration are the endpoints worth guessing at, so
// they get tight budgets; the general limit is a blunt backstop that a normal
// session never approaches.
const (
	// Everyone in a household shares one public address, so this budget is
	// shared between them. It is set above what two people fumbling passwords
	// would hit, because the precise per-account defence is the durable
	// exponential backoff in handleLogin — this limit only needs to stop
	// automated volume, and 20 tries per 15 minutes is nowhere near enough to
	// threaten a 12-character password.
	loginAttemptsPerWindow = 20
	loginWindow            = 15 * time.Minute

	registerAttemptsPerWindow = 5
	registerWindow            = time.Hour

	// Covers password changes and MFA enrolment: authenticated, but each one
	// is a step towards taking an account over if guessed.
	accountAttemptsPerWindow = 20
	accountWindow            = time.Hour

	generalRequestsPerWindow = 300
	generalWindow            = time.Minute
)

// NewServer builds a Server from an open connection pool. The AI client is
// always constructed; when no API key is configured it is simply disabled, so
// handlers gate on s.AI.Enabled() rather than a nil check.
func NewServer(cfg config.Config, pool *pgxpool.Pool, cipher *crypto.Cipher) *Server {
	queries := dbgen.New(pool)
	return &Server{
		Config:          cfg,
		Pool:            pool,
		Queries:         queries,
		Cipher:          cipher,
		AI:              ai.New(cfg.AI),
		Notify:          notify.New(cfg.NTFY, queries),
		loginLimiter:    ratelimit.New(loginAttemptsPerWindow, loginWindow),
		registerLimiter: ratelimit.New(registerAttemptsPerWindow, registerWindow),
		accountLimiter:  ratelimit.New(accountAttemptsPerWindow, accountWindow),
		generalLimiter:  ratelimit.New(generalRequestsPerWindow, generalWindow),
	}
}

// enqueueSync schedules a background sync for an item.
func (s *Server) enqueueSync(itemID uuid.UUID) {
	jobs.EnqueueSync(context.Background(), s.Jobs, itemID)
}

// enqueueAlertEval schedules an immediate alert evaluation for a household, so
// a just-changed alert surfaces without waiting for the periodic sweep. Nil
// client (no queue configured) is tolerated.
func (s *Server) enqueueAlertEval(householdID uuid.UUID) {
	jobs.EnqueueAlertEval(context.Background(), s.Jobs, householdID)
}

// Routes returns the fully-wired HTTP handler.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)

	// RealIP rewrites RemoteAddr from True-Client-IP / X-Real-IP /
	// X-Forwarded-For — headers any client can send. Mounting it
	// unconditionally would mean an attacker picks their own apparent address
	// and every IP-based rate limit below becomes decorative. It goes on only
	// when the operator has declared a sanitising proxy really is in front.
	if s.Config.TrustProxyHeaders {
		r.Use(middleware.RealIP)
	}

	r.Use(middleware.Recoverer)
	r.Use(requestLogger)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(s.securityHeaders)
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
		r.Use(s.generalLimiter.Middleware)
		r.Use(auth.RequireCSRF)

		r.Route("/auth", func(r chi.Router) {
			// Bootstraps the CSRF cookie for clients that do not have one yet.
			r.Get("/csrf", s.handleCSRFToken)
			r.Post("/logout", s.handleLogout)

			// Unauthenticated and guessable: the two places where knowing a
			// secret gets you in. Both are throttled per IP, and login is
			// additionally backed by durable per-account lockout.
			r.Group(func(r chi.Router) {
				r.Use(s.loginLimiter.Middleware)
				r.Post("/login", s.handleLogin)
				r.Post("/mfa/verify", s.handleMFAVerify)
			})

			r.Group(func(r chi.Router) {
				r.Use(s.registerLimiter.Middleware)
				r.Post("/register", s.handleRegister)
			})

			r.Group(func(r chi.Router) {
				r.Use(authMW.Authenticate)
				r.Get("/me", s.handleMe)

				r.Get("/sessions", s.handleListSessions)
				r.Delete("/sessions/{sessionID}", s.handleRevokeSession)
				r.Post("/sessions/revoke-others", s.handleRevokeOtherSessions)
				r.Get("/events", s.handleListAuthEvents)

				r.Get("/mfa", s.handleMFAStatus)

				// Changing a password or a second factor is account takeover
				// if guessed, so these carry their own budget on top of the
				// password/code each one already demands.
				r.Group(func(r chi.Router) {
					r.Use(s.accountLimiter.Middleware)
					r.Post("/password", s.handleChangePassword)
					r.Post("/mfa/setup", s.handleMFASetup)
					r.Post("/mfa/activate", s.handleMFAActivate)
					r.Post("/mfa/disable", s.handleMFADisable)
					r.Post("/mfa/recovery-codes", s.handleMFARecoveryCodes)
				})
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

		r.Route("/preferences", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleGetPreferences)
			r.Put("/", s.handleUpsertPreferences)
		})

		r.Route("/notifications", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Post("/test", s.handleTestNotification)
		})

		r.Route("/digest", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Post("/test", s.handleSendDigestNow)
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
			r.Post("/", s.handleCreateManualTransaction)
			r.Post("/import", s.handleImportTransactions)
			r.Patch("/{transactionID}/category", s.handleRecategoriseTransaction)
			r.Put("/{transactionID}", s.handleUpdateManualTransaction)    // manual only
			r.Delete("/{transactionID}", s.handleDeleteManualTransaction) // manual only
		})

		r.Route("/categories", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListCategories)
			r.Post("/", s.handleCreateCategory)
			r.Put("/{categoryID}", s.handleUpdateCategory)
			r.Delete("/{categoryID}", s.handleDeleteCategory)
		})

		r.Route("/budgets", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleBudgetProgress)
			r.Post("/", s.handleCreateBudget)
			r.Post("/suggest", s.handleSuggestBudgets)
			r.Delete("/{budgetID}", s.handleDeleteBudget)
		})

		r.Route("/goals", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListGoals)
			r.Post("/", s.handleCreateGoal)
			r.Post("/parse", s.handleParseGoal)
			r.Put("/{goalID}", s.handleUpdateGoal)
			r.Delete("/{goalID}", s.handleArchiveGoal)
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
			r.Get("/by-day", s.handleSpendingByDay)
			r.Get("/trend", s.handleTrend)
			r.Get("/averages", s.handleCategoryAverages)
			r.Get("/merchants", s.handleTopMerchants)
			r.Get("/recurring", s.handleRecurring)
			r.Get("/monthly-summary", s.handleGetMonthlySummary)
			r.Post("/monthly-summary", s.handleGenerateMonthlySummary)
		})

		r.Group(func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/capabilities", s.handleCapabilities)
			r.Post("/chat", s.handleChat)
		})

		r.Route("/insights", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListInsights)
			r.Post("/{insightID}/read", s.handleMarkInsightRead)
			r.Post("/{insightID}/dismiss", s.handleDismissInsight)
		})

		r.Route("/alerts", func(r chi.Router) {
			r.Use(authMW.Authenticate)
			r.Get("/", s.handleListAlerts)
			r.Post("/", s.handleCreateAlert)
			r.Post("/parse", s.handleParseAlert)
			r.Put("/{alertID}", s.handleUpdateAlert)
			r.Delete("/{alertID}", s.handleDeleteAlert)
			r.Get("/events", s.handleListAlertEvents)
			r.Get("/events/unread-count", s.handleUnreadAlertCount)
			r.Post("/events/read-all", s.handleMarkAllAlertEventsRead)
			r.Post("/events/{eventID}/read", s.handleMarkAlertEventRead)
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
