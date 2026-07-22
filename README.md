# Ledgermancy

A self-hosted, multi-user personal finance hub. Pulls accounts and transactions from
Plaid into Postgres, categorizes spending, and turns it into the numbers you actually
need: monthly spend by category, income vs. outflow, savings rate, net worth over time,
and a year-plus of history in one place.

## Status

**Phases 1–5 complete.** Auth, households, Plaid
ingest, categorisation, the spending dashboard, net worth (investments, liabilities,
manual assets, snapshots, projections), and the exportable **Financial Summary**
report are all running and verified against the Plaid sandbox.

Only the optional phases remain: AI enrichment (6) and the chatbot (7).

### The financial summary report

**Report** in the nav produces a one-click **Financial Summary** covering the trailing
twelve months: current position, cash flow, savings rate, per-category averages and
annual totals, month-by-month history, debt with rates, and a labelled projection.

Export is **Print / Save as PDF** plus three CSVs (transactions, category summary,
net-worth history). There is deliberately no server-side PDF renderer: that would
mean either headless Chrome in the container (~300 MB) or a Go PDF library with
hand-placed layout. A print-styled page gives a better document for a two-person app,
and what is on screen is exactly what prints.

The CSV transaction amounts are **sign-flipped** from Plaid's convention so negative
means money out — summing the column in a spreadsheet gives the right answer without
a formula. The `is_transfer` and `is_income` flags are included so a spreadsheet can
reproduce the app's totals rather than guess which rows to exclude.

### Plaid products are per-institution

`PLAID_PRODUCTS` sets what *new* links request; each item stores its own list, and
the Investments and Liabilities sync modules are no-ops for items not linked with
them. So an institution connected for transactions alone is completely unaffected by
either module — verified by linking one of each.

Keep `PLAID_PRODUCTS=transactions` unless you specifically want more: Plaid narrows
the institution list to banks supporting *every* requested product, so asking for all
three hides banks that would otherwise work.

### Why net worth is snapshotted

Balances are a point-in-time value — Plaid reports what an account holds today and
keeps no history, so yesterday's net worth is unrecoverable once a balance moves.
The figure is therefore written down daily (after each sync, and on a schedule so
quiet households still get points). The trend can only start from the day the app
did, which the UI says plainly rather than implying older data exists.

### Projections are arithmetic, not forecasts

`internal/networth/project.go` is a plain compounding model: growth applies to the
invested balance *before* each month's contribution, debt paydown floors at zero, and
a negative surplus draws down assets. Every assumption is visible, adjustable, and
echoed back in the response alongside an `estimate: true` flag. Nothing here predicts
markets.

### How the spending numbers are defined

These rules decide every figure the app reports, and getting them wrong is how
finance apps quietly lie:

- **Plaid signs amounts positive = money out.** Spending sums positive rows; income
  is the negation of rows in income categories.
- **Transfers count as neither income nor spending.** Moving money between your own
  accounts is not either one.
- **Credit-card payments are transfers, not spending.** The purchases were already
  counted when they hit the card; counting the payment too would double every dollar
  spent on credit. `LOAN_PAYMENTS_CREDIT_CARD_PAYMENT` maps to a transfer category
  specifically to prevent this.
- **Monthly averages divide by elapsed months**, not months touched. An inclusive
  "+1" turns a trailing year into 13 months and understates every average by ~8%.
- **All arithmetic happens in Postgres in `NUMERIC`** — never in Go, never in JS.

### Categorisation order

Manual choice → household rule → merchant cache → Plaid's own category → (phase 6)
an LLM, whose answer is then cached. Steps 1–4 are deterministic and free, which is
why the AI layer stays optional. A manual choice is marked `category_source =
'manual'` and is preserved by the sync upsert, so Plaid can never overwrite it.

> **Sandbox history is short.** Plaid's sandbox institutions only generate about
> 90 days of transactions, so a sandbox link will not show a full year. That is a
> fixture limit, not a backfill limit — the sync loop pulls until `has_more` is
> false, which against a real institution in production means up to ~24 months.

## API (phase 1)

Every state-changing request needs the CSRF token echoed in an `X-CSRF-Token` header.

| Method | Path                          | Auth | Notes                                        |
| ------ | ----------------------------- | ---- | -------------------------------------------- |
| GET    | `/healthz`                    | –    | Process + database health                     |
| GET    | `/api/auth/csrf`              | –    | **Call first.** Issues the CSRF cookie/token |
| POST   | `/api/auth/register`          | –    | First user creates the household; the rest need an invite |
| POST   | `/api/auth/login`             | –    | Rotates the CSRF token on success             |
| POST   | `/api/auth/logout`            | –    | Deletes the session server-side               |
| GET    | `/api/auth/me`                | ✓    | Current user                                  |
| POST   | `/api/auth/mfa/verify`        | –    | Second login step; consumes the challenge cookie |
| GET    | `/api/auth/mfa`               | ✓    | Two-factor status and recovery codes left     |
| POST   | `/api/auth/mfa/setup`         | ✓    | Password required. Returns QR + base32 secret |
| POST   | `/api/auth/mfa/activate`      | ✓    | Confirms a code; returns recovery codes **once** |
| POST   | `/api/auth/mfa/disable`       | ✓    | Requires password **and** a current code      |
| POST   | `/api/auth/mfa/recovery-codes`| ✓    | Regenerates the set, invalidating the old one |
| POST   | `/api/auth/password`          | ✓    | Change password; signs out every other device |
| GET    | `/api/auth/sessions`          | ✓    | Active sessions with device and address       |
| DELETE | `/api/auth/sessions/{id}`     | ✓    | Revoke one device                             |
| POST   | `/api/auth/sessions/revoke-others` | ✓ | Sign out everywhere but here                |
| GET    | `/api/auth/events`            | ✓    | Last 50 security events on the account        |
| GET    | `/api/household/`             | ✓    | Current household                             |
| GET    | `/api/household/members`      | ✓    | Household members                             |
| POST   | `/api/household/invites`      | ✓    | Returns the invite token **once**             |
| GET    | `/api/household/invites`      | ✓    | Pending invites                               |
| DELETE | `/api/household/invites/{id}` | ✓    | Revoke an invite                              |
| POST   | `/api/plaid/link-token`       | ✓    | Token for opening Plaid Link                  |
| POST   | `/api/plaid/exchange`         | ✓    | Completes linking; starts the backfill        |
| GET    | `/api/plaid/items`            | ✓    | Linked institutions and their sync state      |
| POST   | `/api/plaid/items/{id}/sync`  | ✓    | Refresh now (routine syncs run in the worker) |
| PATCH  | `/api/plaid/items/{id}/sharing` | ✓  | Share an institution with the household       |
| DELETE | `/api/plaid/items/{id}`       | ✓    | Unlink (cascades to accounts + transactions)  |
| GET    | `/api/accounts`               | ✓    | Visible accounts with balances                |
| GET    | `/api/transactions`           | ✓    | `from`, `to`, `limit`, `offset`; defaults to a rolling year |
| POST   | `/webhooks/plaid`             | –    | Plaid push notifications (see below)          |

The webhook is deliberately outside authentication and CSRF — Plaid is not a browser
and carries no session. That is safe because the payload is treated purely as a hint:
the only action it can trigger is "re-sync this item", and the sync re-reads everything
from Plaid using our own stored access token. A forged webhook can cause a wasted sync,
never a data change.

Registration is invite-only after the first account: this is a private household
ledger, so an open sign-up form would only be an attack surface.

## Stack

| Layer      | Choice                                                          |
| ---------- | --------------------------------------------------------------- |
| Backend    | Go — chi, pgx, sqlc, goose, River (background jobs)              |
| Database   | PostgreSQL 17 — money as `NUMERIC(20,4)`, raw Plaid in `JSONB`   |
| Frontend   | React + Vite + TypeScript, Tailwind, shadcn/ui, Tremor, Framer Motion |
| Data       | Plaid — Transactions, plus optional Investments and Liabilities   |
| AI         | Any Anthropic Messages API-compatible endpoint (GLM, Claude, …)  |
| Deploy     | Docker Compose                                                   |

## Design rules

- **Money is never a float.** `NUMERIC(20,4)` in Postgres, `shopspring/decimal` in Go.
  Plaid returns JSON numbers; they are parsed straight into decimal, never through `float64`.
- **Plaid owns raw data; we own enrichment.** The untouched Plaid payload is kept in a
  `raw` JSONB column so any derived value can be recomputed when logic changes.
- **Deterministic before AI.** Categorization tries manual overrides → user rules →
  cached merchant map → Plaid's own categories, and only then falls back to an LLM,
  caching that result so it is never paid for twice.
- **AI is optional.** Leave `AI_API_KEY` blank and everything except the AI-specific
  features works exactly the same.

## Going live with real accounts

Everything below runs against Plaid's **Sandbox** (fake banks). To connect real
accounts you do **not** need full Plaid Production approval: since April 2026 a free,
auto-approved **Trial plan** gives real production data for up to 10 Items (an Item is
one login at one institution, not one account), including Transactions, Investments
and Liabilities.

**See [DEPLOYING.md](DEPLOYING.md)** for the whole path: Plaid Trial signup, server
deployment, TLS, webhooks, and backups.

> **One-way door worth knowing:** Plaid caps transaction history at 90 days by
> default and the window **cannot be changed after an Item is linked**. Ledgermancy
> requests the 730-day maximum at link time, but an institution linked by older code
> is stuck — unlink and relink to fix it.

## Getting started

```bash
cp .env.example .env

# Generate the two required secrets and paste them into .env
openssl rand -base64 32   # -> ENCRYPTION_KEY
openssl rand -base64 32   # -> SESSION_SECRET

docker compose up --build
```

The API listens on `http://localhost:8080`; `GET /healthz` reports database connectivity.
Schema migrations run automatically on API startup.

Then start the frontend in a second terminal:

```bash
cd frontend
npm install     # first time only
npm run dev
```

Open **http://localhost:5173**. The first account you create becomes the household;
everyone after that joins by invitation from the Household page. Once signed in, use
**Accounts → Connect an account** to link a bank through Plaid Link.

In sandbox, Plaid Link accepts the test credentials `user_good` / `pass_good`.

### A rule worth keeping

Money crosses the wire as decimal **strings**, never JSON numbers, so the backend's
exact `NUMERIC` values are not dragged through a float on the way out. Formatting one
value for display is fine; **never sum them in JavaScript**. Every total the UI shows
must be computed server-side, where the arithmetic is exact.

Transaction dates are calendar dates and are formatted from their date parts rather
than through `new Date(iso)` — see `frontend/src/lib/money.ts`. Passing a midnight-UTC
date to the browser's formatter renders the previous day in any timezone west of UTC,
which silently moves month-boundary transactions into the wrong month.

The Vite dev server proxies `/api` to the Go API, so the browser only ever sees one
origin. That keeps the session cookie same-origin and means CORS is not involved in
development at all — `FRONTEND_ORIGIN` in `.env` only matters when the frontend is
served from a different origin than the API.

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
```

## Build phases

1. **Foundation** — scaffold, compose, config, schema, auth, health endpoint.
2. **Transactions ingest** — Plaid Link, `/transactions/sync` with cursor, full
   historical backfill, webhooks, CSV import.
3. **Reporting core** — categorization, monthly rollups, spending dashboard,
   per-category averages, annual totals, savings rate.
4. **Net worth + modules** — Investments and Liabilities modules, manual assets,
   monthly net-worth snapshots, projections.
5. **Financial summary** — exportable PDF + CSV report.
6. **AI enrichment** — LLM categorization fallback, recurring detection, alerts.
7. **Chatbot** — tool-calling agent over your own financial data.

## Network exposure

Postgres is an internal service. The api and worker always reach it over the compose
network at `postgres:5432`; no host port is involved in normal operation.

| | Postgres | API |
| --- | --- | --- |
| Development (`docker compose up`) | `127.0.0.1:5433` — loopback only, for `psql` | `0.0.0.0:8080` |
| Production (with the prod overlay) | not published at all | `127.0.0.1:8080`, behind a reverse proxy |

```bash
# Production: database unreachable from the host entirely
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

# Get a psql shell without publishing a port
docker compose exec postgres psql -U ledgermancy -d ledgermancy
```

Two things here are easy to get wrong and are deliberate:

- The dev mapping is written `127.0.0.1:5433:5432`. Writing `5433:5432` binds `0.0.0.0`
  and exposes the database to the entire LAN.
- The prod overlay uses `ports: !override []`. Compose *merges* list fields across files,
  so a plain `ports: []` does nothing and the base binding survives.

Verify at any time with `docker compose ps` — the `Ports` column shows the real bind address.

## Security notes

- **Optional TOTP two-factor auth.** Standard authenticator apps (scan a QR, enter
  6 digits). The half-authenticated state between the password and the code lives in
  its own `mfa_challenges` table rather than as a flag on `sessions` — so a row in
  `sessions` continues to mean exactly one thing, *fully authenticated*, and a pending
  challenge cannot satisfy the auth middleware however it changes.
- TOTP secrets are encrypted at rest with the same AES-GCM key as Plaid tokens.
  Recovery codes are HMAC-hashed like session tokens (they are high-entropy randoms,
  so argon2 would buy nothing and cost ten 64 MiB verifications per attempt).
- Each accepted TOTP code's time-step is recorded, so a code cannot be replayed
  inside the 90-second window it stays valid for.
- Enabling two-factor, changing a password, or disabling two-factor all require the
  password again — holding a session is not authority to change the factors guarding
  the account. Disabling additionally requires a current code.
- **Rate limiting** on sign-in, registration, and account changes, keyed on the real
  client address, plus durable per-account exponential backoff that survives a
  restart. A locked account still returns the generic error, so lockout is not an
  oracle for which addresses exist.
- The API only believes `X-Forwarded-For`/`X-Forwarded-Proto` when
  `TRUST_PROXY_HEADERS` is set — the production overlay sets it, because there the
  bundled nginx strips client-supplied address headers and is the only route in.
- **Security headers** on every response: CSP (tuned for Plaid Link), HSTS behind
  TLS, `nosniff`, `DENY` framing, `no-referrer`, and `Cache-Control: no-store` so
  financial JSON and CSV exports never land in a cache.
- **Auth audit log** and an active-session list with per-device revoke, both on the
  Security page.
- Plaid access tokens are encrypted at rest (AES-GCM) and never returned to the browser.
- Sessions are server-side, in `httpOnly` + `SameSite=Strict` cookies — not localStorage.
  They expire after 30 days, or 7 days idle.
- Passwords are argon2id; login failures are indistinguishable between an unknown
  address and a wrong password, in both message and timing. Hashes made under weaker
  parameters are transparently upgraded on next sign-in.
- Registration is invite-only after the first account, and an invite is bound to the
  address it was issued for.
- `.env` is gitignored. Do not commit real Plaid credentials or secrets.
- `DATABASE_URL` uses `sslmode=disable`. That is fine while Postgres is a container on
  the same host — the traffic never leaves the local bridge network. If the database
  ever moves to another host (managed Postgres, a separate k8s node), switch to
  `sslmode=verify-full`.
