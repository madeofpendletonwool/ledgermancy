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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/notify"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ratelimit"
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

	// Documents is nil when the vault is switched off or its storage backend
	// could not be opened. The document handlers check for that and return 503,
	// exactly like the Plaid handlers — the rest of the app is unaffected.
	Documents *documents.Vault

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
	authMW := auth.Middleware{Queries: s.Queries, Secret: s.Config.SessionSecret}
	return s.routesWithAuth(authMW.Authenticate)
}

// routesWithAuth builds the router with an injectable authentication step.
//
// Split out so the role-enforcement test can mount a stub that injects a fixed
// identity and assert the guard on every route without a database. The
// production path always passes auth.Middleware.Authenticate.
func (s *Server) routesWithAuth(authenticate func(http.Handler) http.Handler) http.Handler {
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
				r.Use(authenticate)
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

		// ------------------------------------------------------------------
		// Child-accessible routes.
		//
		// Everything below the /me group is scoped to the CALLER's own person
		// and is the only surface a `child` login can reach beyond its own
		// account settings. Every route group after this one is adult-only,
		// enforced by auth.RequireAdult on the group rather than per handler —
		// a role checked on some routes and not others implies protection that
		// is not there.
		//
		// When adding a route, the question is not "should a child see this"
		// but "which group does it belong in". There is no third option.
		// ------------------------------------------------------------------
		r.Route("/me", func(r chi.Router) {
			r.Use(authenticate)
			r.Get("/person", s.handleGetMyPerson)
			r.Put("/person", s.handleUpdateMyPerson)
			r.Get("/allowance", s.handleGetMyAllowance)
			r.Get("/allowance/entries", s.handleListMyAllowanceEntries)
			// A child records their own spending. This is the one write a
			// child has, and it is deliberate: a ledger a kid cannot write to
			// teaches nothing. Credits are parent-only, enforced in the
			// handler by rejecting any kind other than 'spend'.
			r.Post("/allowance/entries", s.handleCreateMyAllowanceEntry)
			r.Get("/accounts", s.handleListMyAccounts)
			r.Get("/goals", s.handleListMyGoals)
		})

		r.Route("/household", func(r chi.Router) {
			r.Use(authenticate)

			// Household name is readable by anyone signed in — the child view
			// puts it in the header, and it is not a financial figure.
			r.Get("/", s.handleGetHousehold)

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAdult)
				r.Get("/members", s.handleListMembers)
				r.Get("/invites", s.handleListInvites)
				r.Post("/invites", s.handleCreateInvite)
				r.Delete("/invites/{inviteID}", s.handleDeleteInvite)

				// People: everyone the household's money can be about,
				// whether or not they can sign in.
				r.Get("/people", s.handleListPeople)
				r.Post("/people", s.handleCreatePerson)
				r.Put("/people/{personID}", s.handleUpdatePerson)
				r.Delete("/people/{personID}", s.handleDeletePerson)

				// Allowance schedules are a parent's to set, including for a
				// child who can sign in — hence adult-only rather than /me.
				r.Get("/people/{personID}/allowance", s.handleGetAllowance)
				r.Put("/people/{personID}/allowance", s.handleUpsertAllowance)
				r.Get("/people/{personID}/allowance/entries", s.handleListAllowanceEntries)
				r.Post("/people/{personID}/allowance/entries", s.handleCreateAllowanceEntry)
				r.Delete("/allowance/entries/{entryID}", s.handleDeleteAllowanceEntry)
			})

			// Changing who can do what is the owner's alone.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAdult)
				r.Use(auth.RequireOwner)
				r.Put("/members/{userID}/role", s.handleSetMemberRole)
			})
		})

		// Preferences are mixed by design: a user-scoped preference is the
		// caller's own, a household-scoped one is a household setting. The
		// group is open and handleUpsertPreferences refuses a household-scoped
		// write from a child — the one place a per-handler check is right,
		// because the resource itself is split rather than the routes.
		r.Route("/preferences", func(r chi.Router) {
			r.Use(authenticate)
			r.Get("/", s.handleGetPreferences)
			r.Put("/", s.handleUpsertPreferences)
		})

		r.Route("/notifications", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Post("/test", s.handleTestNotification)
		})

		r.Route("/digest", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Post("/test", s.handleSendDigestNow)
		})

		// Operator surface. This is the instance's recovery posture, not a
		// household's data: it names paths on the host, reports on the backup
		// subsystem, and can trigger a full database dump. Owner-only,
		// enforced on the group rather than per handler, for the same reason
		// the adult-only groups are.
		r.Route("/admin", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult, auth.RequireOwner)
			r.Get("/continuity", s.handleContinuityStatus)
			r.Post("/continuity/key-ack", s.handleContinuityKeyAck)
			r.Post("/continuity/run", s.handleContinuityRun)
			r.Get("/continuity/export", s.handleContinuityExport)
		})

		r.Route("/plaid", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Post("/link-token", s.handleCreateLinkToken)
			r.Post("/exchange", s.handleExchangePublicToken)
			r.Get("/items", s.handleListItems)
			r.Post("/items/{itemID}/sync", s.handleSyncItem)
			r.Post("/items/{itemID}/reconnected", s.handleItemReconnected)
			r.Patch("/items/{itemID}/sharing", s.handleSetItemSharing)
			r.Delete("/items/{itemID}", s.handleDeleteItem)
		})

		r.Route("/accounts", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListAccounts)
			// The rate and monthly payment for a debt the institution reports
			// none for — which is most of them. Adult-only, matching every other
			// account mutation: a child must not set the figures the household's
			// payoff plans are computed from.
			r.Put("/{accountID}/terms", s.handleSetAccountTerms)
		})

		r.Route("/transactions", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListTransactions)
			r.Post("/", s.handleCreateManualTransaction)
			r.Post("/import", s.handleImportTransactions)
			r.Patch("/{transactionID}/category", s.handleRecategoriseTransaction)
			// How a row COUNTS, as opposed to what it says — so this accepts
			// Plaid-synced rows, unlike the manual-only editors below.
			r.Patch("/{transactionID}/flags", s.handleSetTransactionFlags)
			r.Put("/{transactionID}", s.handleUpdateManualTransaction)    // manual only
			r.Delete("/{transactionID}", s.handleDeleteManualTransaction) // manual only
		})

		// Canonical merchants: the review queue for proposed merges, plus manual
		// merge/split/rename. Everything the suggestion job writes is inert until
		// something here confirms it.
		r.Route("/merchants", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListMerchants)
			r.Get("/keys", s.handleListMerchantKeys)
			// Ahead of the /{merchantID} routes below, so chi does not read
			// "detail" as a merchant id.
			r.Get("/detail", s.handleMerchantDetail)
			r.Post("/merge", s.handleMergeMerchants)
			r.Post("/split", s.handleSplitMerchant)
			r.Post("/scan", s.handleScanMerchants)
			r.Post("/{merchantID}/reject", s.handleRejectMerchantSuggestion)
			r.Patch("/{merchantID}", s.handleRenameMerchant)
		})

		// The encrypted document vault. Every route here is household-scoped,
		// including the download — a document id is never on its own sufficient
		// to fetch a blob. See document_handlers.go.
		r.Route("/documents", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListDocuments)
			r.Post("/", s.handleUploadDocument)
			r.Get("/storage", s.handleDocumentStorage)
			r.Get("/attached", s.handleAttachedDocuments)
			r.Get("/counts", s.handleDocumentCounts)
			r.Get("/{documentID}", s.handleGetDocument)
			r.Put("/{documentID}", s.handleUpdateDocument)
			r.Delete("/{documentID}", s.handleDeleteDocument)
			r.Get("/{documentID}/download", s.handleDownloadDocument)
			r.Post("/{documentID}/extract", s.handleExtractDocument)
			// Re-runs the match against an already-read receipt. Deterministic
			// SQL only: no decryption, no upload, no model call — which is what
			// lets a receipt scanned before its charge posted find it later.
			r.Get("/{documentID}/matches", s.handleDocumentMatches)
			r.Post("/{documentID}/links", s.handleLinkDocument)
			r.Delete("/{documentID}/links/{linkID}", s.handleUnlinkDocument)
		})

		r.Route("/categories", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListCategories)
			r.Post("/", s.handleCreateCategory)
			r.Put("/{categoryID}", s.handleUpdateCategory)
			r.Delete("/{categoryID}", s.handleDeleteCategory)
			// The per-category breakdown, the counterpart of /merchants/detail.
			// A category id is a UUID, so it can travel as a path segment — a raw
			// merchant descriptor cannot, which is why that one takes a query
			// parameter instead.
			r.Get("/{categoryID}/detail", s.handleCategoryDetail)
		})

		r.Route("/budgets", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleBudgetProgress)
			r.Get("/safe-to-spend", s.handleSafeToSpend)
			r.Post("/", s.handleCreateBudget)
			r.Post("/suggest", s.handleSuggestBudgets)
			r.Delete("/{budgetID}", s.handleDeleteBudget)
		})

		// The bill calendar. /upcoming expands cadences into occurrences and
		// /projection carries balances forward through them; both are derived,
		// so neither is a second source of truth for what is due.
		r.Route("/obligations", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListObligations)
			r.Post("/", s.handleCreateObligation)
			r.Get("/upcoming", s.handleUpcomingObligations)
			r.Get("/projection", s.handleObligationProjection)
			r.Put("/{obligationID}", s.handleUpdateObligation)
			r.Delete("/{obligationID}", s.handleDeleteObligation)
		})

		// Goals are the one mixed group. Reads are visibility-scoped in SQL
		// (ListGoals takes all_person_goals, set from the caller's role), so a
		// child listing goals gets their own and nothing else. Writes are
		// adult-only: setting a child's target is a parent's action.
		r.Route("/goals", func(r chi.Router) {
			r.Use(authenticate)
			r.Get("/", s.handleListGoals)
			r.Get("/{goalID}/contributions", s.handleListGoalContributions)

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAdult)
				r.Post("/", s.handleCreateGoal)
				r.Post("/parse", s.handleParseGoal)
				r.Put("/{goalID}", s.handleUpdateGoal)
				r.Delete("/{goalID}", s.handleArchiveGoal)
				r.Post("/{goalID}/contributions", s.handleCreateGoalContribution)
				r.Delete("/contributions/{contributionID}", s.handleDeleteGoalContribution)
			})
		})

		// Bill split and the reimbursement ledger. Adult-only throughout: a
		// split is a claim on another member's money.
		r.Route("/splits", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListSplitTransactions)
			r.Get("/ledger", s.handleHouseholdLedger)
			r.Get("/transactions/{transactionID}", s.handleGetTransactionSplits)
			r.Put("/transactions/{transactionID}", s.handleSetTransactionSplits)
			r.Delete("/transactions/{transactionID}", s.handleClearTransactionSplits)
			r.Post("/{splitID}/settle", s.handleSettleSplit)
			r.Delete("/{splitID}/settle", s.handleUnsettleSplit)
		})

		r.Route("/networth", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleNetWorth)
			r.Get("/history", s.handleNetWorthHistory)
			r.Post("/snapshot", s.handleSnapshotNow)
			r.Get("/projection", s.handleProjection)
			r.Get("/by-person", s.handleNetWorthByPerson)
		})

		// Retirement. Sits beside /networth rather than inside it: the
		// straight-line model there is a net-worth illustration, and this is an
		// account-aware retirement engine. Neither replaces the other.
		//
		// /retirement always returns the assumptions alongside the curve, so a
		// client cannot render a projection without the inputs that produced it.
		r.Route("/projections", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/assumptions", s.handleGetAssumptions)
			r.Put("/assumptions", s.handleSaveAssumptions)
			r.Get("/contributions", s.handleListContributions)
			r.Put("/contributions/{accountID}", s.handleSaveContribution)
			r.Delete("/contributions/{accountID}", s.handleDeleteContribution)
			r.Get("/retirement", s.handleRetirementProjection)
		})

		r.Route("/holdings", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListHoldings)
		})

		// The Investments surface. Every read is scoped the same way as the
		// other reporting endpoints; the one write is the tax-treatment
		// confirmation, which is a user decision and never inferred.
		r.Route("/investments", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleInvestmentOverview)
			r.Get("/performance", s.handleInvestmentPerformance)
			r.Get("/benchmarks", s.handleInvestmentBenchmarks)
			r.Get("/allocation", s.handleInvestmentAllocation)
			r.Get("/holdings", s.handleInvestmentHoldings)
			r.Get("/fees", s.handleInvestmentFees)
			r.Get("/dividends", s.handleInvestmentDividends)
			r.Patch("/accounts/{accountID}/tax-treatment", s.handleSetAccountTaxTreatment)
			r.Patch("/accounts/{accountID}/beneficiary", s.handleSetAccountBeneficiary)
		})

		r.Route("/liabilities", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListLiabilities)
		})

		r.Route("/manual-assets", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListManualAssets)
			r.Post("/", s.handleCreateManualAsset)
			r.Delete("/{assetID}", s.handleDeleteManualAsset)
		})

		r.Route("/export", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/transactions.csv", s.handleExportTransactions)
			r.Get("/categories.csv", s.handleExportCategorySummary)
			r.Get("/net-worth.csv", s.handleExportNetWorth)
			r.Get("/holdings.csv", s.handleExportHoldings)
		})

		r.Route("/reports", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/summary", s.handleSummary)
			r.Get("/by-category", s.handleSpendingByCategory)
			r.Get("/by-day", s.handleSpendingByDay)
			r.Get("/trend", s.handleTrend)
			r.Get("/averages", s.handleCategoryAverages)
			// The category × month matrix behind the spending heatmap (item #8)
			// and the category-mix small multiples (item #12). One endpoint,
			// two renderings; the clients pivot differently.
			r.Get("/heatmap", s.handleSpendingHeatmap)
			r.Get("/merchants", s.handleTopMerchants)
			// The whole merchant list for the explorer, where /merchants is the
			// top-N card. Separate rather than a mode of the same endpoint: this
			// one carries a per-row prior period and an envelope, and the
			// Dashboard's card wants neither.
			r.Get("/merchant-explorer", s.handleMerchantExplorer)
			r.Get("/recurring", s.handleRecurring)
			r.Post("/recurring/suppress", s.handleSuppressRecurring)
			r.Delete("/recurring/suppress", s.handleUnsuppressRecurring)
			r.Get("/recurring/suppressed", s.handleListSuppressedRecurring)
			r.Get("/monthly-summary", s.handleGetMonthlySummary)
			r.Post("/monthly-summary", s.handleGenerateMonthlySummary)
		})

		// /capabilities stays open to every login: the frontend cannot decide
		// what to render without it, and it exposes no figures. /chat is
		// adult-only — it reads household data by design.
		r.Group(func(r chi.Router) {
			r.Use(authenticate)
			r.Get("/capabilities", s.handleCapabilities)
		})
		r.Group(func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Post("/chat", s.handleChat)
		})

		r.Route("/insights", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
			r.Get("/", s.handleListInsights)
			// Anomaly suppression (anomaly_handlers.go). Declared above the
			// {insightID} routes so this file stays diffable against
			// role_enforcement_test.go by eye; chi resolves static segments
			// before params regardless of order.
			r.Get("/anomaly/suppressed", s.handleListSuppressedAnomalies)
			r.Post("/anomaly/suppress", s.handleSuppressAnomaly)
			r.Delete("/anomaly/suppress", s.handleUnsuppressAnomaly)
			r.Post("/{insightID}/read", s.handleMarkInsightRead)
			r.Post("/{insightID}/dismiss", s.handleDismissInsight)
			r.Post("/{insightID}/normal", s.handleMarkInsightNormal)
		})

		r.Route("/alerts", func(r chi.Router) {
			r.Use(authenticate, auth.RequireAdult)
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
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
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
