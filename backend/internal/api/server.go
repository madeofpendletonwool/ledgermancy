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

	// aiLimiter is the only limiter here keyed on the USER rather than the
	// address, because it is the only one guarding a budget that is billed per
	// account. See aiRequestsPerWindow and aiRateKey.
	aiLimiter *ratelimit.Limiter
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

	// The AI budget. Every other limit here defends a secret; this one defends
	// a BILL. One POST /api/chat is up to maxToolIterations calls against a
	// metered key, so leaving it on generalLimiter meant a single logged-in
	// member could spend 300 turns a minute — roughly 2,400 upstream calls —
	// through the same bucket as GET /api/preferences.
	//
	// 60 turns an hour is far above what a household conversation reaches (a
	// long session is a handful of questions) and far below what a runaway
	// client or a bored teenager costs. The worst case it admits is 60 ×
	// maxToolIterations = 480 upstream calls per user per hour, which is a bill
	// an operator can reason about.
	aiRequestsPerWindow = 60
	aiWindow            = time.Hour
)

// Request budgets.
//
// Two of them, and the reason there are two is that they cannot be nested: a
// context deadline can only ever SHORTEN its parent's, never extend it. So the
// AI routes are mounted as a sibling of the ordinary ones rather than inside
// them — putting a longer Timeout under a shorter one is a silent no-op, which
// is exactly how these numbers got out of step in the first place.
const (
	// defaultRouteTimeout is the budget for every route that does not call the
	// model. Generous for a database read and nowhere near a model round-trip,
	// which is the point.
	defaultRouteTimeout = 30 * time.Second

	// aiRouteTimeout is the budget for POST /api/chat, the one route that drives
	// the model↔tool loop.
	//
	// THIS NUMBER IS NOT FREE TO CHOOSE. It has to cover the worst case the loop
	// can actually produce:
	//
	//	maxToolIterations (9, chat_handlers.go)
	//	  × ai.RequestTimeout (150s, internal/ai/client.go)  = 1350s of model time
	//	+ aiToolBudget                                       =   60s of our own
	//	                                                     = 1410s
	//
	// 1500s leaves a minute and a half of headroom on top. It is a CEILING, not
	// a target — a real turn answers in seconds, and the defences against a turn
	// that does not are maxToolIterations and aiLimiter, not this. Cutting it
	// lower would only reintroduce the original bug: a budget the loop below it
	// can exceed.
	//
	// Moved 600s → 660s with maxToolIterations 8 → 9 (the find_tools escape
	// hatch spends an iteration), then 660s → 1500s when ai.RequestTimeout went
	// 60s → 150s to fit a reasoning model's output budget (see chatMaxTokens).
	// THE WORST CASE GREW; THE TYPICAL TURN DID NOT. Every iteration must burn
	// its full timeout to reach this, which requires nine consecutive
	// near-timeout model calls in one turn.
	//
	// Note that nginx's proxy_read_timeout is NOT this number and does not need
	// to move with it: it bounds the gap BETWEEN reads on a streaming response,
	// and the longest gap this loop can produce is one ai.RequestTimeout — which
	// DID grow to 150s, so a proxy_read_timeout below that will now cut a
	// thinking model off mid-turn.
	//
	// TestAIRouteTimeoutFitsToolLoop fails if any of the three numbers moves
	// without the others.
	aiRouteTimeout = 1500 * time.Second

	// aiToolBudget is how much of aiRouteTimeout is reserved for OUR work rather
	// than the model's: the scoped queries executeChatTool runs between
	// iterations, plus serialising their results back into the prompt. Every one
	// is a local database read, so a minute across all nine is slack, not an
	// estimate.
	aiToolBudget = 60 * time.Second

	// HTTPServerWriteTimeout is the value main.go assigns to net/http's
	// http.Server.WriteTimeout. It is exported only so the timeout test can pin
	// it next to the budgets below.
	//
	// It is zero — DISABLED — and that is not a relaxation. WriteTimeout is a
	// hard ceiling the net/http server enforces BELOW the chi router, so a
	// handler's own context.WithTimeout cannot widen it: whichever fires first
	// wins, and WriteTimeout always wins against a longer route budget. That is
	// the exact bug this constant exists to prevent.
	//
	// The streaming chat route (POST /api/chat) writes its answer as Server-Sent
	// Events for the full duration of the model↔tool loop, which aiRouteTimeout
	// budgets at up to 660s. A server-level WriteTimeout of anything less than
	// that — the previous 60s, for example — tears the SSE connection down
	// mid-stream the moment it elapses, the response writer's context cancels,
	// the in-flight GLM stream read returns context.Canceled, and the browser is
	// left holding a half-finished HTTP/2 stream it can only report as
	// ERR_HTTP2_PROTOCOL_ERROR. The route's middleware.Timeout(aiRouteTimeout)
	// already cancels the handler's context on the same budget, which is the
	// correct layer to enforce it because it cooperates with the stream instead
	// of cutting it from underneath.
	//
	// Non-streaming routes keep their defence: defaultRouteTimeout via
	// middleware.Timeout cancels their context, and a handler that respects ctx
	// stops writing. Slowloris-style read attacks are bounded by
	// ReadHeaderTimeout, not WriteTimeout, so disabling this one does not weaken
	// that protection either.
	HTTPServerWriteTimeout time.Duration = 0
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
		aiLimiter:       ratelimit.New(aiRequestsPerWindow, aiWindow),
	}
}

// aiRateKey keys the AI budget on the authenticated user.
//
// The cost being limited is charged per ACCOUNT, not per address: a household
// behind one public IP should get one allowance each, and one account should not
// get a fresh allowance for every address it dials in from. The routes this is
// mounted on all sit behind Authenticate, so the identity is always there; the
// address fallback exists so a wiring mistake degrades to the old, weaker
// behaviour rather than to no limit at all.
func aiRateKey(r *http.Request) string {
	if identity, ok := auth.FromContext(r.Context()); ok {
		return "user:" + identity.UserID.String()
	}
	return "ip:" + ratelimit.ClientIP(r)
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
	r.Use(s.securityHeaders)
	r.Use(s.cors)

	// NO request timeout here, deliberately. There is no single budget that
	// suits both a database read and a chat turn, and a global one cannot be
	// widened further in: a nested context deadline only shortens its parent's.
	// Each subtree below declares its own — see defaultRouteTimeout and
	// aiRouteTimeout. Nothing is left unbudgeted.

	// Liveness/readiness. Deliberately outside /api and unauthenticated so
	// Docker's healthcheck can reach it.
	r.With(middleware.Timeout(defaultRouteTimeout)).Get("/healthz", s.handleHealth)

	// Plaid's webhook is mounted outside the /api group on purpose: Plaid is
	// not a browser, so it has neither a session nor a CSRF token. See the
	// handler for why that is safe.
	r.With(middleware.Timeout(defaultRouteTimeout)).
		Post("/webhooks/plaid", s.handlePlaidWebhook)

	r.Route("/api", func(r chi.Router) {
		r.Use(s.generalLimiter.Middleware)
		r.Use(auth.RequireCSRF)

		// ── The AI subtree ────────────────────────────────────────────────
		//
		// A SIBLING of the rest of /api rather than a member of it, because it
		// needs a budget LONGER than the default and a nested Timeout cannot
		// grant one. Two things are different in here and only two: the request
		// budget, and a per-user allowance on a metered API key.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(aiRouteTimeout))

			// Adult-only: it reads household data by design. aiLimiter goes
			// AFTER authenticate so there is an identity to key on — see
			// aiRateKey.
			r.Group(func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Use(s.aiLimiter.KeyedMiddleware(aiRateKey))
				r.Post("/chat", s.handleChat)
			})
		})

		// ── Everything else, on the ordinary budget ───────────────────────
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(defaultRouteTimeout))

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
					r.Get("/events", s.handleListAuthEvents)

					r.Get("/mfa", s.handleMFAStatus)

					// ── Credential management ─────────────────────────────
					//
					// Everything that mints or destroys a way IN to this
					// account, and the one group a personal API token may not
					// reach — auth.RequireSession, which demands the session
					// cookie.
					//
					// The reason is that a token which can manage credentials
					// is not one credential but a factory: a leaked read-write
					// token could mint replacements for itself and revoke the
					// ones the user would have used to lock it out. Requiring
					// the cookie means taking the account back always comes down
					// to the password.
					//
					// It is also a correctness fence, not only a policy one.
					// handleRevokeOtherSessions keys on Identity.TokenHash,
					// which names a session row and is empty for a token —
					// letting a token in would sign the user out everywhere.
					r.Group(func(r chi.Router) {
						r.Use(auth.RequireSession)

						r.Delete("/sessions/{sessionID}", s.handleRevokeSession)
						r.Post("/sessions/revoke-others", s.handleRevokeOtherSessions)

						// Personal API tokens: the credential a third-party
						// client authenticates with. Only POST carries
						// accountLimiter — minting one belongs in the same
						// budget as a password change, while listing and
						// revoking are the recovery path and must not be
						// throttled alongside it.
						r.Get("/tokens", s.handleListAPITokens)
						r.With(s.accountLimiter.Middleware).
							Post("/tokens", s.handleCreateAPIToken)
						r.Delete("/tokens/{tokenID}", s.handleRevokeAPIToken)

						// Changing a password or a second factor is account
						// takeover if guessed, so these carry their own budget
						// on top of the password/code each one already demands.
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

			// The in-app digest history. Separate from /digest above, which is the
			// "send one now" action rather than a resource: these are the stored
			// entries, and each one is scoped to the requesting USER inside the
			// queries — the adult-only group here is necessary but not sufficient,
			// exactly as it is for /payroll.
			r.Route("/digests", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListDigests)
				r.Get("/{digestID}", s.handleGetDigest)
				r.Post("/{digestID}/read", s.handleMarkDigestRead)
			})

			// Outgoing webhooks: the household's own outbound event bus.
			//
			// Adult-only rather than owner-only, and the line is worth stating.
			// Owner-only is for the INSTANCE (continuity, system status); this is
			// the household's data going to a host the household chose, and every
			// adult can already read every figure a webhook could carry. What
			// keeps it from being a data-exfiltration route for anyone else is
			// that it is off entirely unless the operator sets WEBHOOKS_ENABLED —
			// every handler checks, so a switched-off instance answers 503 here
			// rather than merely rendering no UI.
			r.Route("/webhooks", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListWebhooks)
				r.Post("/", s.handleCreateWebhook)
				// Static before the parameterised routes below so the trigger
				// vocabulary is never parsed as a webhook id.
				r.Get("/triggers", s.handleListWebhookTriggers)
				r.Put("/{webhookID}", s.handleUpdateWebhook)
				r.Delete("/{webhookID}", s.handleDeleteWebhook)
				r.Post("/{webhookID}/secret", s.handleRotateWebhookSecret)
				r.Post("/{webhookID}/test", s.handleTestWebhook)
				r.Get("/{webhookID}/messages", s.handleListWebhookMessages)
				r.Get("/{webhookID}/messages/{messageID}/attempts", s.handleListWebhookAttempts)
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

				// What the instance is doing right now: queue depth, per-item
				// sync freshness, and whether a worker is alive at all. Same
				// owner-only reasoning as continuity above — it describes the
				// deployment rather than the household.
				r.Get("/status", s.handleSystemStatus)
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
				// The deposit yield the cash-drag detector measures against
				// (doc 32). User-entered because Plaid does not serve it
				// reliably; a null CLEARS it, and an empty field means
				// unknown rather than zero.
				r.Put("/{accountID}/deposit-apy", s.handleSetDepositAPY)
				r.Get("/idle-cash", s.handleIdleCash)

			// Manual accounts (doc 30). Every mutation below refuses a
			// source='plaid' id — a linked account's identity and balance
			// belong to the institution, and an edit here would survive only
			// until the next sync silently reverted it.
			//
			// The read at the bottom is the exception: balance-history serves
			// BOTH sources. A manual account's rows are the user's writes; a
			// Plaid account's are the snapshot path's (MAD-119), recorded after
			// each sync and on the daily sweep because Plaid keeps no balance
			// history of its own. The list query scopes through account_access
			// for either, so the same endpoint draws both trends.
			r.Post("/", s.handleCreateManualAccount)
			r.Put("/{accountID}", s.handleUpdateManualAccount)
			r.Delete("/{accountID}", s.handleDeleteManualAccount)
			r.Put("/{accountID}/balance", s.handleSetManualBalance)
			r.Get("/{accountID}/balance-history", s.handleListBalanceHistory)
				r.Post("/{accountID}/holdings", s.handleUpsertManualHolding)
				r.Get("/{accountID}/investment-transactions", s.handleListAccountInvestmentTx)
				// Piggy banks drawing from one account, and the unassigned
				// balance left on it (account balance − every piggy bank's
				// derived balance). The latter is what stops a household
				// earmarking the same dollars across jars twice.
				r.Get("/{accountID}/piggy-banks", s.handleListAccountPiggyBanks)
				r.Get("/{accountID}/available-for-piggy", s.handleAccountAvailableForPiggy)
			})

			// Securities are reference data, not household data: a row states what
			// a ticker is, which is true for everyone and says nothing about who
			// holds it. Ownership lives in holdings, and those are scoped.
			r.Route("/securities", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListSecurities)
				r.Post("/", s.handleCreateManualSecurity)
			})

			r.Route("/investment-transactions", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Post("/", s.handleCreateManualInvestmentTx)
				r.Delete("/{txID}", s.handleDeleteManualInvestmentTx)
			})

			r.Route("/transactions", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListTransactions)
				r.Post("/", s.handleCreateManualTransaction)
				r.Post("/import", s.handleImportTransactions)
				// The vocabulary the `q` grammar accepts, for the search bar's
				// autocomplete. Static for a given binary; see search_handlers.go.
				r.Get("/search-operators", s.handleSearchOperators)
				// One action over the set of rows a user ticked on the list.
				// Static segments, so they can never be mistaken for the
				// {transactionID} routes below. Each resolves its id list
				// through the caller's own visibility before writing, and
				// applies the whole selection in one database transaction —
				// see bulk_transaction_handlers.go.
				r.Post("/bulk/tags", s.handleBulkTransactionTags)
				r.Post("/bulk/category", s.handleBulkTransactionCategory)
				r.Post("/bulk/flags", s.handleBulkTransactionFlags)
				r.Patch("/{transactionID}/category", s.handleRecategoriseTransaction)
				// How a row COUNTS, as opposed to what it says — so this accepts
				// Plaid-synced rows, unlike the manual-only editors below.
				r.Patch("/{transactionID}/flags", s.handleSetTransactionFlags)
				// What a row is FOR, as opposed to what kind of spending it is.
				// Accepts Plaid-synced rows for the same reason /flags does: a
				// synced hotel charge is exactly what "Summer Vacation" has to
				// land on. Replaces the row's whole tag set — see
				// setTransactionTagsRequest.
				r.Put("/{transactionID}/tags", s.handleSetTransactionTags)
				// How this row relates to ANOTHER row: a refund, a duplicate,
				// something it paid for. Reads return both directions of every
				// edge, phrased from this transaction's end. Accepts synced rows
				// for the same reason /flags and /tags do — a refund from Plaid
				// is exactly the row that has to point at the charge it cancels
				// — and never writes to either transaction. See
				// transaction_link_handlers.go.
				r.Get("/{transactionID}/links", s.handleListTransactionLinks)
				r.Post("/{transactionID}/links", s.handleCreateTransactionLink)
				r.Delete("/{transactionID}/links/{linkID}", s.handleDeleteTransactionLink)
				r.Put("/{transactionID}", s.handleUpdateManualTransaction)    // manual only
				r.Delete("/{transactionID}", s.handleDeleteManualTransaction) // manual only
			})

			// The vocabulary of relationships two transactions can stand in.
			// Household-scoped like tags and categories, with the same
			// household_id-NULL convention for the shipped rows: the three system
			// types are readable by every household and writable by none, so
			// `refund` — the type the netting view keys on — means one thing in
			// every deployment. There is deliberately no admin CRUD over them.
			r.Route("/link-types", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListLinkTypes)
				r.Post("/", s.handleCreateLinkType)
				r.Put("/{linkTypeID}", s.handleUpdateLinkType)
				r.Delete("/{linkTypeID}", s.handleDeleteLinkType)
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
				// The cached avatar image (MAD-38). Keyed by resolved merchant key
				// in the query string rather than in the path: a merchant key is
				// free-form text, and a path segment would have to survive escaping
				// on both sides for no benefit. 404 whenever there is nothing to
				// show, which the frontend treats as "use the monogram".
				r.Get("/logo", s.handleMerchantLogo)
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

			// Payroll: the pre-tax side of the ledger. Adult-only like every other
			// financial surface, but note that adult-only is NOT the whole access
			// story here — a paystub is private to the person whose pay it is, and
			// the group guard does nothing about one adult reading another's
			// salary. That is enforced per row, in SQL. See queries/payroll.sql.
			//
			// /parse and /parse-document read a PDF's text layer locally and return
			// a proposal. Neither sends anything to an AI provider; see
			// payroll_import_handlers.go before changing that.
			r.Route("/payroll", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)

				r.Get("/taxonomy", s.handlePayrollTaxonomy)
				r.Get("/years", s.handleListPaystubYears)
				r.Get("/summary", s.handlePayrollSummary)
				r.Get("/savings-rate", s.handlePayrollSavingsRate)
				r.Get("/tax-summary", s.handlePayrollTaxSummary)

				r.Post("/parse", s.handleParsePaystubUpload)
				r.Post("/parse-document", s.handleParsePaystubDocument)

				r.Get("/employers", s.handleListEmployers)
				r.Post("/employers", s.handleCreateEmployer)
				r.Put("/employers/{employerID}", s.handleUpdateEmployer)
				r.Delete("/employers/{employerID}", s.handleDeleteEmployer)

				r.Get("/paystubs", s.handleListPaystubs)
				r.Post("/paystubs", s.handleCreatePaystub)
				r.Get("/paystubs/{paystubID}", s.handleGetPaystub)
				r.Put("/paystubs/{paystubID}", s.handleUpdatePaystub)
				r.Delete("/paystubs/{paystubID}", s.handleDeletePaystub)
				r.Post("/paystubs/{paystubID}/confirm", s.handleConfirmPaystub)
				r.Patch("/paystubs/{paystubID}/sharing", s.handleSetPaystubSharing)
				// The match only ever proposes; the PUT is where a human's choice
				// is recorded.
				r.Get("/paystubs/{paystubID}/deposit-matches", s.handleMatchPaystubDeposit)
				r.Put("/paystubs/{paystubID}/deposit", s.handleLinkPaystubDeposit)
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

			// Tags: the second axis over a transaction, orthogonal to its
			// category. Household-scoped like categories — but note the split
			// enforced in queries/tags.sql: the TAG is household data, while the
			// TAGGED TRANSACTIONS behind its counts and totals stay under the
			// per-member visibility predicate, so labelling a charge on a private
			// account never makes that charge readable by the other member.
			r.Route("/tags", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListTags)
				r.Post("/", s.handleCreateTag)
				r.Put("/{tagID}", s.handleUpdateTag)
				r.Delete("/{tagID}", s.handleDeleteTag)
				// The envelope's contents: the tag, its derived total, and the
				// charges behind it in one round trip.
				r.Get("/{tagID}/transactions", s.handleListTagTransactions)
			})

			// Rules: user-editable IF-THEN over transactions. Household data
			// like tags and categories, and with the same split — the RULE
			// belongs to the household, while the transactions the two verbs
			// below reach stay under the per-member visibility predicate, so a
			// preview's match count can never describe a charge on the other
			// member's private account.
			r.Route("/rules", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListRules)
				r.Post("/", s.handleCreateRule)
				r.Put("/{ruleID}", s.handleUpdateRule)
				r.Delete("/{ruleID}", s.handleDeleteRule)
				// Dry run: what this rule would do to what is already stored.
				// Writes nothing, and shares its planner with the run below, so
				// it cannot promise something the run would not do.
				r.Post("/{ruleID}/test", s.handleTestRule)
				// The same walk, applied. Idempotent: pressing it a second time
				// changes nothing.
				r.Post("/{ruleID}/trigger", s.handleTriggerRule)
			})

			r.Route("/budgets", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleBudgetProgress)
				r.Get("/safe-to-spend", s.handleSafeToSpend)
				r.Post("/", s.handleCreateBudget)
				r.Post("/suggest", s.handleSuggestBudgets)
				r.Delete("/{budgetID}", s.handleDeleteBudget)
			})

			// Per-object change history. Read-only and visibility-scoped in SQL,
			// so a single authenticated endpoint serves every object kind; the
			// handler dispatches on object_kind to the right scoping query.
			r.Route("/audit", func(r chi.Router) {
				r.Use(authenticate)
				r.Get("/", s.handleListObjectChanges)
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
				r.Put("/{obligationID}/auto-post", s.handleSetObligationAutoPost)
				// Reminders (MAD-85): mark one occurrence paid, or clear that mark.
				// The Reminders view itself reads /api/insights, so there is no list
				// handler — only the write that records a member's confirmation.
				r.Post("/{obligationID}/satisfy", s.handleSatisfyObligation)
				r.Delete("/{obligationID}/satisfy", s.handleClearObligationSatisfied)
				r.Put("/{obligationID}/remind", s.handleSetObligationRemind)
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
					// Reminders opt-out toggle (MAD-85).
					r.Put("/{goalID}/remind", s.handleSetGoalRemind)
					r.Post("/{goalID}/contributions", s.handleCreateGoalContribution)
					r.Delete("/contributions/{contributionID}", s.handleDeleteGoalContribution)
				})
			})

			// Piggy banks: lightweight savings jars on an asset account. Reads
			// are household-scoped in SQL; writes are adult-only, matching every
			// other earmarking of household money (goals, budgets). A deposit or
			// withdraw only annotates part of the account balance — it never
			// moves real money — so the whole group stays read-mostly.
			r.Route("/piggy-banks", func(r chi.Router) {
				r.Use(authenticate)
				r.Get("/", s.handleListPiggyBanks)
				r.Get("/{piggyBankID}/events", s.handleListPiggyBankEvents)

				r.Group(func(r chi.Router) {
					r.Use(auth.RequireAdult)
					r.Post("/", s.handleCreatePiggyBank)
					r.Put("/{piggyBankID}", s.handleUpdatePiggyBank)
					r.Delete("/{piggyBankID}", s.handleDeletePiggyBank)
					r.Post("/{piggyBankID}/deposit", s.handleDepositPiggyBank)
					r.Post("/{piggyBankID}/withdraw", s.handleWithdrawPiggyBank)
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

			// The CPI-U deflator behind every real ("inflation-adjusted") figure:
			// what it covers, how fresh it is, and the household's own year set
			// against it. Read-only, unlike the savings-bond rate table beside it —
			// that one is editable because a household might legitimately correct a
			// row, and nothing about a published price index invites that.
			//
			// Every client that renders a real figure reads this first, because the
			// base period is not decoration: a real number without the month it is
			// denominated in is not a number anybody can use.
			r.Route("/inflation", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleInflation)
			})

			r.Route("/networth", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleNetWorth)
				// `real=1` adds inflation-adjusted figures beside the nominal ones.
				// Nominal stays the default; see inflation_handlers.go.
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
				// Manual positions only; a Plaid holding deleted here would come
				// straight back on the next sync.
				r.Delete("/{holdingID}", s.handleDeleteManualHolding)
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

			// The proactive advisor. Read-only by design and permanently so: it
			// presents computed tradeoffs and EXECUTES NOTHING. RequireAdult
			// because it reads the household's whole financial position — debts,
			// salary, retirement balances — into one response, and its settings are
			// household preferences written through /preferences.
			r.Route("/advisor", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleAdvisor)

				// The advisor SURFACE (doc 31). Still executes nothing: the
				// briefing is a read, a thread is a transcript, and an action
				// item is a note about a decision the household made.
				//
				// Threads and action items enforce household scope inside the
				// query on every read AND every write, not here — a route group
				// can only say who may call, and the question these have to answer
				// is whose row this is.
				r.Get("/briefing", s.handleBriefing)

				r.Get("/threads", s.handleListThreads)
				r.Post("/threads", s.handleCreateThread)
				r.Get("/threads/{threadID}", s.handleGetThread)
				r.Patch("/threads/{threadID}", s.handleRenameThread)
				r.Delete("/threads/{threadID}", s.handleDeleteThread)

				r.Get("/action-items", s.handleListActionItems)
				r.Post("/action-items", s.handleCreateActionItem)
				r.Patch("/action-items/{itemID}", s.handleUpdateActionItem)
			})

			// The household profile: the two columns doc 31 added, which the
			// allocator (32) and the guardrail rule (33) key on. Adult-only for
			// the same reason the advisor is — filing status is household tax
			// information.
			r.Route("/household/profile", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleGetProfile)
				r.Put("/", s.handleUpdateProfile)
			})

			// The allocation planner (doc 32). Adult-only and household-scoped
			// for the same reason the advisor is: it reads the household's whole
			// position — balances, debts, salary-derived headroom, filing status
			// — into one response.
			//
			// EXECUTES NOTHING, permanently. A plan is a projection; the user
			// acts on it. POST /plan runs and returns without writing, and the
			// only write in the group is a saved plan's own row.
			r.Route("/allocation", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/buckets", s.handleAllocationBuckets)
				r.Post("/plan", s.handleRunAllocation)
				r.Get("/asset-location", s.handleAssetLocation)

				r.Get("/plans", s.handleListPlans)
				r.Post("/plans", s.handleSavePlan)
				r.Get("/plans/{planID}", s.handleGetPlan)
				r.Delete("/plans/{planID}", s.handleDeletePlan)
			})

			// The likelihood layer (doc 33). Same scope and the same permanently
			// read-only posture as the allocator it runs over: a distribution is a
			// projection, and the household acts on it.
			//
			// The SIMULATION is gated behind RETIREMENT_MONTE_CARLO_ENABLED; the
			// ROUTES are not. With the gate off they return the deterministic
			// figure and name that in the basis — a 404 or a 503 would make the
			// panel a broken tile on every instance that has not opted in.
			//
			// Only POST /plans/{planID}/track writes, and what it writes is the
			// EXPECTED side of a snapshot. Actuals are read live every time drift
			// is computed, so correcting an old contribution corrects the history
			// rather than leaving a wrong figure frozen in a row.
			r.Route("/likelihood", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Post("/plan/{planID}", s.handleLikelihood)
				r.Post("/compare", s.handleCompare)
				r.Get("/plans/{planID}/track", s.handleTracking)
				r.Post("/plans/{planID}/track", s.handleRecordTracking)
			})

			r.Route("/manual-assets", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListManualAssets)
				r.Post("/", s.handleCreateManualAsset)
				r.Delete("/{assetID}", s.handleDeleteManualAsset)

				// Revaluation, depreciation and bonds (doc 26). Note which of these
				// write: only POST /valuations does. The suggestion endpoint
				// computes a proposal and returns it — net worth never moves on an
				// estimate the user has not accepted.
				r.Get("/{assetID}/detail", s.handleGetAssetDetail)
				r.Put("/{assetID}/detail", s.handleUpsertAssetDetail)
				r.Get("/{assetID}/valuations", s.handleListValuations)
				r.Post("/{assetID}/valuations", s.handleCreateValuation)
				r.Get("/{assetID}/suggestion", s.handleAssetSuggestion)
				r.Get("/{assetID}/bond", s.handleBondValue)
				r.Put("/{assetID}/loan", s.handleLinkAssetLoan)
			})

			// The published savings-bond rate table. Readable and editable on
			// purpose: a bundled table of numbers is only defensible if the user
			// can check it against treasurydirect.gov and correct a row.
			r.Route("/savings-bond-rates", func(r chi.Router) {
				r.Use(authenticate, auth.RequireAdult)
				r.Get("/", s.handleListBondRates)
				r.Put("/", s.handleUpsertBondRate)
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
				// The third breakdown axis. Same money rules as by-category and
				// /merchants, but the panels do not sum to the same total: a
				// transaction can carry several tags, or none.
				r.Get("/by-tag", s.handleSpendingByTag)
				r.Get("/by-day", s.handleSpendingByDay)
				r.Get("/trend", s.handleTrend)
				r.Get("/averages", s.handleCategoryAverages)
				// The category × month matrix behind the spending heatmap (item #8)
				// and the category-mix small multiples (item #12). One endpoint,
				// two renderings; the clients pivot differently.
				r.Get("/heatmap", s.handleSpendingHeatmap)
				// The cash-flow Sankey (item #13, MAD-33): income sources →
				// spending categories → leftover to savings, in one round trip.
				// Bundles the period summary with the income/spending-by-category
				// breakdowns so the chart's bands reconcile with the Spending page
				// tiles to the cent.
				r.Get("/cash-flow", s.handleCashFlow)
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
			// what to render without it, and it exposes no figures. POST /chat used
			// to sit beside it; it now lives in the AI subtree at the top of this
			// function, where it gets a budget its tool loop can actually finish in.
			r.Group(func(r chi.Router) {
				r.Use(authenticate)
				r.Get("/capabilities", s.handleCapabilities)
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
