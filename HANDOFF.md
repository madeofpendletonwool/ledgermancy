# Handoff: Ledgermancy phases 6 and 7

Paste the section below to a fresh agent. Everything it needs is either in this
repo or reachable from it.

---

## The prompt

I'm continuing work on **Ledgermancy**, a self-hosted household finance app at
`~/Documents/github/ledgermancy` (public repo: `madeofpendletonwool/ledgermancy`).
Phases 1–5 are complete, deployed, and verified. I want you to build **phase 6
(AI enrichment)** and **phase 7 (chatbot)**.

### Read these first, in this order

1. `README.md` — architecture, the money rules, categorisation order, API surface
2. `DEPLOYING.md` — how it runs, Plaid access model, data retention
3. `backend/internal/categorize/categorize.go` — the doc comment at the top
   states the exact resolution order phase 6 plugs into
4. `backend/internal/config/config.go` — `AIConfig` is already wired
5. `BRAND.md` — colours, if you touch UI

The code is deliberately commented with *why*, not what. When something looks
oddly specific, the comment explains a bug that was already paid for — read it
before "simplifying" it.

### What already exists for you

- **AI config is done.** `AI_BASE_URL`, `AI_API_KEY`, `AI_MODEL` in `.env`,
  parsed into `config.AIConfig` with an `Enabled()` helper. `internal/ai/` is an
  empty directory waiting for the client.
- **The provider is an Anthropic Messages API-compatible endpoint** — I run
  **GLM** (`https://api.z.ai/api/anthropic`, model `glm-4.6`), not Claude. Build
  against the Anthropic wire format with a configurable base URL. Do not hardcode
  Anthropic, and do not pull in a vendor SDK that assumes `api.anthropic.com`.
- **The categorisation hook point is defined.** `categorize.Resolve()` runs
  manual → rule → merchant cache → Plaid category, then returns `ok=false`. That
  false is where the LLM goes. `SourceLLM` already exists as a constant.
- **`merchant_category_map` is the cache table**, with a `source` column
  (`manual`/`llm`/`rule`) and a unique key on `(household_id, merchant_key)`.
  `UpsertMerchantCategory` already refuses to overwrite a `manual` entry.
- **`alerts` and `alert_events` tables exist and are unused.** `alerts.config` is
  JSONB so new alert types need no migration.
- **River job queue is running** in `cmd/worker`. Adding a job means: define args
  + worker in `internal/jobs/jobs.go`, register in `internal/jobs/client.go`.

### Phase 6 — AI enrichment

1. `internal/ai` — a small client against the Anthropic Messages API with a
   configurable base URL. One `Complete(ctx, …)` style method plus tool-calling
   support (phase 7 needs it). Must degrade cleanly: if `AI_API_KEY` is blank the
   app runs exactly as it does now.
2. **LLM categorisation fallback** — only for transactions steps 1–4 could not
   resolve. Write every answer to `merchant_category_map` so a merchant is never
   sent twice. This is what keeps the cost near zero.
3. **Recurring/subscription detection** — feeds the fixed-vs-discretionary split
   that reports already read from `categories.is_fixed`.
4. **Alerts engine** — evaluate `alerts` rows after each sync and on a schedule,
   write `alert_events`, surface them in the UI. Types from the plan: big-spend,
   budget-threshold, unusual-merchant, low-leftover.
5. **Monthly natural-language summary** — optional, cache it.

### Phase 7 — chatbot

Use **tool-calling over read-only scoped queries, not RAG.** The model gets tools
like `spend_by_category(month, scope)`, `transactions_search(filters)`,
`budget_status(month)`, each backed by an existing sqlc query scoped to the
caller's household visibility. It composes an answer from real query results.
This is materially more accurate and auditable than embedding retrieval for
financial questions, and it reuses the reporting layer that is already correct.

**pgvector is optional and currently unavailable** — the `postgres:17-alpine`
image does not ship it (`SELECT ... pg_available_extensions` returns false). If
you want fuzzy "transactions like X", switch the image to `pgvector/pgvector:pg17`
in `docker-compose.yml` first. Do not make the chatbot depend on it.

### Invariants — do not break these

These are load-bearing. Each one is a bug that was already found and fixed:

- **Money is never a float.** `NUMERIC(20,4)` in Postgres, `shopspring/decimal`
  in Go, decimal **strings** over the wire. The single float is Plaid's SDK
  boundary, isolated in `amountToDecimal` with a test pinning it. Never sum money
  in JavaScript — every total is computed in SQL.
- **Transfers are excluded from income and spending**, and **credit-card payments
  are transfers**. Counting a card payment as spending double-counts every dollar
  spent on credit.
- **A manual category is sticky.** `category_source = 'manual'` is preserved by
  the sync upsert. The LLM must never overwrite one.
- **Visibility is always scoped**: own items ∪ household items where
  `is_shared`. Every new query needs the same `WHERE u.household_id = $1 AND
  (i.user_id = $2 OR i.is_shared)` shape. A chatbot tool that forgets this leaks
  a spouse's private account.
- **The app must work with AI disabled.** Blank `AI_API_KEY` = no AI features,
  everything else unchanged.

### Traps that will cost you hours

- **sqlc infers `min()`/`max()` over a NOT NULL column as NOT NULL**, but they
  are NULL when no rows match, and scanning fails. A `::date` cast makes it
  worse; removing the cast yields `interface{}`. Hand-write those queries — see
  `itemHistorySpans` in `internal/api/plaid_handlers.go` for the pattern.
- **sqlc time overrides use a `stdtime` alias on purpose.** See the comment in
  `backend/sqlc.yaml`. Changing it to `package: "time"` produces a duplicate
  import and will not compile.
- **River rejects an insert if `UniqueOpts.ByState` omits any required state**
  (available, pending, running, scheduled) and only logs it — jobs silently never
  run. Start from `rivertype.UniqueOptsByStateDefault()`. There is a regression
  test in `internal/jobs/jobs_test.go`.
- **Postgres `DATE` serialises as midnight UTC.** Formatting it with
  `new Date(iso)` renders the previous day west of UTC and moves month-boundary
  transactions into the wrong month. Use the parser in `frontend/src/lib/money.ts`.
- **Before writing any chart code, load the `dataviz` skill and run its palette
  validator.** The brand colours in `BRAND.md` **fail** it — two are
  indistinguishable to normal vision. Chart tokens live in
  `frontend/src/components/charts/tokens.ts` and are already validated.

### Working environment

- `docker compose up -d --build` — Postgres (loopback `:5433`), api (`:8080`),
  worker. `cd frontend && npm run dev` for Vite on `:5173`, which proxies `/api`
  so the browser sees one origin.
- Migrations run automatically on api startup (goose, versioned by filename).
- **Plaid is on production/Trial with real accounts.** Be careful with anything
  destructive — do not unlink items or truncate tables. Ask first.
- Testing the API needs a session. Registration is invite-only after the first
  user, so rather than creating junk accounts, mint a session directly: insert
  into `sessions` with `token_hash = HMAC-SHA256(SESSION_SECRET, token)` and use
  that token as the `ledgermancy_session` cookie. `SESSION_SECRET` is base64 in
  `.env` and is decoded to raw bytes before HMAC.
- Run `go build ./... && go vet ./... && go test ./...` and
  `npm run build` before claiming done. `tsc --noEmit` is **not** sufficient —
  the project-references build catches things it misses.

### The bar

Verify with real data and reconcile against an independent calculation — do not
assert that something works because the code looks right. Every phase so far was
checked by computing the same number a second way and comparing. When you change
a UI, screenshot it and actually look; several real bugs were only visible that
way. Report honestly what passed, what failed, and what you did not check.

---

## Is that enough context?

Yes. The repo carries its own reasoning — README, DEPLOYING, BRAND, and dense
"why" comments in the code were written partly for this handoff. The prompt above
adds the three things a fresh agent cannot infer from the source: the GLM
provider choice, the traps already paid for, and the verification standard.

The full original architecture plan, including the phase 6/7 scope, is at
`~/.claude/plans/ledgermancy-a-new-program-fluttering-wind.md` if deeper context
is ever wanted.
