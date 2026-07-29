# Development

How the repo is laid out, how to build and test it, and the load-bearing
invariants and traps that a contributor needs to know.

## Repository layout

```
backend/
  cmd/api/        HTTP server
  cmd/worker/     background jobs (Plaid sync, alerts, net-worth snapshots)
  internal/
    config/       environment configuration
    auth/         argon2id hashing, sessions, middleware
    db/           pgx pool, sqlc output, migrations, queries
    plaid/        Plaid client, sync modules, webhooks
    categorize/   rules engine, merchant cache, LLM fallback
    reporting/    spending, savings rate, net worth, projections
    ai/           Anthropic-compatible client
    api/          routers, handlers, DTOs
frontend/         React + Vite app
docs/             this MkDocs site
```

## Working environment

The whole app — Postgres, api, worker, **and** the nginx frontend — runs via
compose, so the normal way to bring it up is identical to
[Getting started](getting-started.md):

```bash
docker compose up -d --build   # browse http://localhost:8081
```

### Frontend hot-reload (optional)

If you're iterating on frontend code and want Vite's instant hot-module
replacement, run the backend services in compose and the Vite dev server on the
host. The Vite server proxies `/api` to the api's loopback port so the browser
still sees one origin:

```bash
docker compose up -d postgres api worker   # skip the `frontend` service
cd frontend && npm install && npm run dev  # http://localhost:5173
```

- Migrations run automatically on api startup (goose, versioned by filename).
- `FRONTEND_ORIGIN` only matters when the frontend is served from a different
  origin than the API; in the hot-reload path it defaults to
  `http://localhost:5173`, which matches.

### Verifying your work

```bash
# Backend
go build ./... && go vet ./... && go test ./...

# Frontend — project-references build, NOT just tsc --noEmit
cd frontend && npm run build
```

!!! warning "`tsc --noEmit` is not sufficient"
    The project-references build catches things `tsc --noEmit` misses. Always
    run `npm run build`.

### Testing the service worker

The PWA is disabled in `npm run dev` on purpose — a service worker caching
modules that Vite is trying to hot-replace produces bugs that do not exist in a
real build. Exercise it against the production output instead:

```bash
cd frontend && npm run build && npm run preview
```

Then, in devtools: **Application → Service Workers** to confirm it activated,
and **Network → Offline** followed by a reload to check the shell renders, the
offline banner names a time, and write controls are disabled.

Two things worth knowing before changing anything here:

- The set of API paths that may be cached is an **allowlist** in
  `frontend/src/sw.ts`. A new read-only endpoint gets no offline copy until it
  is added there, which is the intended default.
- Anything that ends a session must clear the worker's caches as well as the
  query cache (`clearApiCache` in `frontend/src/lib/offline.ts`). Skipping it
  leaves the previous user's figures readable on a shared device.

## Testing the API with a session

Registration is invite-only after the first user, so rather than creating junk
accounts, mint a session directly: insert into `sessions` with
`token_hash = HMAC-SHA256(SESSION_SECRET, token)` and use that token as the
`ledgermancy_session` cookie. `SESSION_SECRET` is base64 in `.env` and is
decoded to raw bytes before HMAC.

## Invariants — do not break these

These are load-bearing. Each one is a bug that was already found and fixed:

- **Money is never a float.** `NUMERIC(20,4)` in Postgres, `shopspring/decimal`
  in Go, decimal **strings** over the wire. The single float is Plaid's SDK
  boundary, isolated in `amountToDecimal` with a test pinning it. **Never sum
  money in JavaScript** — every total is computed in SQL.
- **Transfers are excluded from income and spending**, and **credit-card
  payments are transfers**. Counting a card payment as spending double-counts
  every dollar spent on credit.
- **A manual category is sticky.** `category_source = 'manual'` is preserved by
  the sync upsert. The LLM must never overwrite one.
- **Visibility is always scoped**: own items ∪ household items where
  `is_shared`. Every new query needs the same `WHERE u.household_id = $1 AND
  (i.user_id = $2 OR i.is_shared)` shape. A chatbot tool that forgets this leaks
  a spouse's private account.
- **The app must work with AI disabled.** Blank `AI_API_KEY` = no AI features,
  everything else unchanged.

## Traps that will cost you hours

- **sqlc infers `min()`/`max()` over a NOT NULL column as NOT NULL**, but they
  are NULL when no rows match, and scanning fails. A `::date` cast makes it
  worse; removing the cast yields `interface{}`. Hand-write those queries — see
  `itemHistorySpans` in `internal/api/plaid_handlers.go` for the pattern.
- **sqlc time overrides use a `stdtime` alias on purpose.** See the comment in
  `backend/sqlc.yaml`. Changing it to `package: "time"` produces a duplicate
  import and will not compile.
- **River rejects an insert if `UniqueOpts.ByState` omits any required state**
  (available, pending, running, scheduled) and only logs it — jobs silently
  never run. Start from `rivertype.UniqueOptsByStateDefault()`. There's a
  regression test in `internal/jobs/jobs_test.go`.
- **Postgres `DATE` serialises as midnight UTC.** Formatting it with
  `new Date(iso)` renders the previous day west of UTC and moves month-boundary
  transactions into the wrong month. Use the parser in
  `frontend/src/lib/money.ts`.
- **Before writing any chart code, validate the palette.** The brand colours in
  `BRAND.md` **fail** a colourblind-safety check — two are indistinguishable to
  normal vision. Chart tokens live in `frontend/src/components/charts/tokens.ts`
  and are already validated. Do not use brand colours in charts or chart colours
  in the logo.

## Design rules

- **Money is never a float.** (See invariants.)
- **Plaid owns raw data; we own enrichment.** The untouched Plaid payload is
  kept in a `raw` JSONB column so any derived value can be recomputed when logic
  changes.
- **Deterministic before AI.** Categorization tries manual overrides → user
  rules → cached merchant map → Plaid's own categories, and only then falls back
  to an LLM, caching that result so it is never paid for twice.
- **AI is optional.** Leave `AI_API_KEY` blank and everything except the
  AI-specific features works exactly the same.

## Money on the wire

Money crosses the wire as decimal **strings**, never JSON numbers, so the
backend's exact `NUMERIC` values are not dragged through a float on the way out.
Formatting one value for display is fine; **never sum them in JavaScript**.
Every total the UI shows must be computed server-side, where the arithmetic is
exact.

Transaction dates are calendar dates and are formatted from their date parts
rather than through `new Date(iso)` — see `frontend/src/lib/money.ts`. Passing a
midnight-UTC date to the browser's formatter renders the previous day in any
timezone west of UTC, which silently moves month-boundary transactions into the
wrong month.

## Working with real Plaid data

If you're pointed at production/Trial with real accounts, **be careful with
anything destructive** — do not unlink items or truncate tables. Ask first. See
the [Deployment](deployment.md) guide for the access model and data retention.

## Docs

These docs are MkDocs Material. The source is in `docs/`, config in
`mkdocs.yml`, Python deps in `docs/requirements.txt`. Preview locally:

```bash
pip install -r docs/requirements.txt   # or use a venv
mkdocs serve
```

A GitHub Actions workflow builds and publishes to Pages on push to `main`. See
[`.github/workflows/docs.yml`](https://github.com/madeofpendletonwool/ledgermancy/blob/main/.github/workflows/docs.yml).
