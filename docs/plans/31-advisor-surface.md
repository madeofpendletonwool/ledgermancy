# 31 — Advisor surface (Assistant → Advisor)

*(Wave 6 — the first of the four advisor docs. Visual companion:
[advisor-overview.html](advisor-overview.html). Extends doc 24.)*

## Context

The Assistant route (`frontend/src/routes/Assistant.tsx`) is a reactive chatbot
with 12 transaction-flavoured tools. Every one answers a past-tense question.
A real advisor meeting is structurally different: it opens with a briefing,
works across horizons, and keeps history. Meanwhile the backend already ships
every engine a real advisor uses — `reporting/safetospend.go`,
`goals/payoff.go`, `goals/goals.go`, `networth/retirement.go`, the
`obligations` projection, `reporting/returns.go`, `networth/limits.go` — and
none of them are reachable from the chat.

This doc is the cheapest, highest-leverage step in the wave: **rename the
route, expose ~12 existing engines as chat tools, and build the page shell
around the chat.** No new math. No new tables beyond conversation/action-item
persistence and a few household-profile columns. Doc 24 adds the ranker, doc
32 the multi-bucket allocator, doc 33 the likelihood layer — all layered on
the tools and surface this doc establishes.

## AI vs deterministic split

Same rule `chat_handlers.go:32-54` already enforces: **tools compute, the
model narrates, arithmetic is never model-generated.** Every new tool returns
finished `StringFixed(2)` strings; the model quotes them verbatim. The
Briefing is composed by deterministic code, not the model. If AI is disabled
(`ErrDisabled`), the page renders the Briefing, Horizon view, and plain tool
output with no narration and no error — the feature must work with no key
configured, like everything else.

## Prerequisites

- **[15-fire-projections.md](15-fire-projections.md)** — shipped. The
  retirement tools wrap `ProjectRetirement`.
- **[13-bill-calendar.md](13-bill-calendar.md)** — shipped. The
  `project_balance` / `upcoming_obligations` tools wrap its handlers.
- **[24-proactive-advisor.md](24-proactive-advisor.md)** — same wave, ships
  alongside. The Options panel on the page consumes doc 24's ranked output.
  This doc exposes the *engines*; doc 24 exposes the *ranker*.

## Data model

**Reserved migration: `00048_advisor_surface.sql`.** (Latest reserved is
`00047` for doc 30; latest applied is `00046`. Confirm both before writing.)

### Household profile columns

Profile only what an engine consumes. Kids and dependents are already in
`household_people` (`00034:17-40`, with `birthdate` and `is_dependent`);
target age and assumptions are already in `projection_assumptions`. This doc
adds the three fields the tax/limit/guardrail engines need and nothing
speculative:

```sql
ALTER TABLE households
    ADD COLUMN state               TEXT,          -- 2-letter; 529 state caps, tax brackets (doc 23)
    ADD COLUMN filing_status       TEXT
        CHECK (filing_status IN ('single','married_joint','married_separate','hoh')),
    ADD COLUMN risk_drawdown_floor NUMERIC(5,2);  -- percent. Personal layer of doc 33's guardrail rule.
```

### Conversation persistence

The chat is currently stateless (the client holds `ChatTurn[]`). A real
advisor relationship is cumulative. Saved threads are household-scoped; the
`is_shared` flag follows the existing account-visibility convention.

```sql
CREATE TABLE advisor_threads (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    user_id      UUID REFERENCES users (id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    is_shared    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX advisor_threads_household_idx ON advisor_threads (household_id);

CREATE TABLE advisor_messages (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id  UUID NOT NULL REFERENCES advisor_threads (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('user','assistant')),
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX advisor_messages_thread_idx ON advisor_messages (thread_id, created_at);
```

### Action items

Advisor proposes; user accepts; it is tracked. **Never executed** —
transfers/payments are permanently out of scope. Doc 33 extends this with
plan-vs-actual reconciliation for allocation plans.

```sql
CREATE TABLE advisor_action_items (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    detail       TEXT,
    source       TEXT NOT NULL CHECK (source IN ('option','allocation','thread','manual')),
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','done','dismissed')),
    due_date     DATE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
CREATE INDEX advisor_action_items_household_idx ON advisor_action_items (household_id, status);
```

## Continuity

| Table / column | Category | Rationale |
|---|---|---|
| `advisor_threads` | `InExport` | User-authored; not re-derivable |
| `advisor_messages` | `InExport` | Conversation history |
| `advisor_action_items` | `InExport` | User decisions |
| `households.state` / `filing_status` / `risk_drawdown_floor` | (existing table) | `households` is already `InExport`; money column `risk_drawdown_floor` is a percent, cast to text in export as usual |

No new blob stores or volumes.

## Backend

### New chat tools — `chat_handlers.go`

Add these tool definitions alongside the existing 12. Each wraps an existing
engine, each scoped `WHERE u.household_id = $1 AND (i.user_id = $2 OR
i.is_shared)` (or the manual-account equivalent `COALESCE(a.user_id,
i.user_id)` from doc 30). All money returned as `StringFixed(2)`.

| Tool | Wraps | Notes |
|---|---|---|
| `safe_to_spend` | `reporting.BuildSafeToSpend` | Reuse verbatim — **advisor slack must equal the Budgets page** |
| `project_balance` | `obligations` day-by-day projection | "Will I overdraft before {date}?" |
| `upcoming_obligations` | `obligation_handlers.go` | 14-day window |
| `debt_summary` | `liabilities` + `account_terms` | New query; APR, balance, min, utilisation |
| `debt_payoff` | `goals.ComputePayoff` + extra-payment param | The `$200/mo extra → dies when` question |
| `goal_status` | `goals.Compute` over all goals | on-track / behind / shortfall |
| `goal_solve` | `goals.Compute` solve-for-monthly | "how much monthly to hit it by {date}" |
| `retirement_projection` | `networth.ProjectRetirement` | FI age, nest egg, on-track |
| `retirement_solve` | bisection already in `retirement.go` | "savings rate to retire at {age}" |
| `investment_performance` | `reporting.BuildPortfolioPerformance` | TWR/MWR, time-bounded |
| `asset_allocation`, `fees_summary` | `reporting/investments.go` | allocation, fee-drag disclosure |
| `contribution_room` | `networth.Limits` + YTD | Partial until doc 23 ships in the same cycle |
| `advisor_briefing` | composed digest across engines | new composition function (see below) |

`debt_strategy` (avalanche/snowball) and `advisor_options` (the ranker) are
doc 24's, not this doc's.

### `advisor_briefing` — `internal/advisor/briefing.go`

New function, deterministic, no AI. Gathers into one struct: net worth
(`ComputeNetWorth`), this month's slack (`BuildSafeToSpend`), FI age
(`ProjectRetirement`), earliest debt-free date (min over `ComputePayoff`),
emergency-fund runway (months of trailing spend covered), and the top three
"worth your attention" items (reuse the insight feed's highest-priority
undismissed rows). The model may phrase the one-paragraph summary; the
fields are finished before the model sees them.

### Handlers — `internal/api/advisor_handlers.go`

```
GET    /api/advisor/briefing              handleBriefing
GET    /api/advisor/threads               handleListThreads
POST   /api/advisor/threads               handleCreateThread
GET    /api/advisor/threads/{id}          handleGetThread          // messages + turns
DELETE /api/advisor/threads/{id}          handleDeleteThread       // household-scoped
GET    /api/advisor/action-items          handleListActionItems
POST   /api/advisor/action-items          handleCreateActionItem
PATCH  /api/advisor/action-items/{id}     handleUpdateActionItem   // status only
PUT    /api/household/profile             handleUpdateProfile      // state/filing_status/risk_floor
```

All mounted on the `authenticate` + `RequireAdult` group. Threads and action
items enforce household scope on every read/write.

### `POST /api/chat` — thread awareness

Extend the existing chat handler to optionally take a `thread_id`; if
present, persist `user` and `assistant` turns to `advisor_messages` after
the stream completes (never mid-stream). If absent, today's stateless
behaviour is unchanged.

## Frontend

Rename `Assistant.tsx` → `Advisor.tsx`, route `/assistant` → `/advisor`
(redirect the old path). The page becomes a layout with the chat as one
region:

- **Briefing strip** — the four headline numbers + top-3 attention items.
  Rendered from `GET /api/advisor/briefing`; pure, deterministic.
- **Options panel** — consumes doc 24's ranked output; appears only when
  slack clears threshold. "Show the arithmetic" expandable per option. Each
  option's accept button writes an `advisor_action_item` with
  `source='option'`.
- **Horizon view** — short / medium / long tabs over `goal_status` + the
  obligation calendar. Each item: target vs projected.
- **Assumptions panel** — always visible, editable; reuses the doc 15
  assumptions endpoints plus the new profile fields.
- **The chat** — retained, now with the ~12 new tools. Empty-state
  suggestions become state-aware (debt present → "pay off my cards faster?").
- **Threads sidebar** — saved conversations; "new thread", rename, delete.
- **Action items tray** — open/done/dismissed; surfaces in the periodic
  advisor report too.

The capabilities gate stays (`capabilities.ai_enabled`); with AI disabled
the Briefing, Horizon, and Options render with plain figures and no chat.

## AI notes

Extend `chatSystemPrompt` (`chat_handlers.go:32-54`) with the new tool list
and a clause: when the user asks "what should I do with $X," the model must
call `advisor_options` (doc 24) or `allocation_plan` (doc 32) rather than
inventing options. The prohibition on model-generated arithmetic is
unchanged and load-bearing.

The beneficiary currency nudge is a deterministic insight producer: a Roth
or 401(k) with `beneficiary_person_id IS NULL` or a beneficiary under 18
yields one insight per account per quarter, dismissible.

## Verification

Decimal-exact, table-driven, mirroring `chat_handlers_test.go`.

- **Every new tool** returns household-scoped rows only; a private account
  for user A is invisible to user B in every tool result.
- **`safe_to_spend` agreement:** the value returned by the chat tool equals
  `GET /api/budgets/safe-to-spend` for the same household and month, to the
  cent. This is the doc-24 agreement test, inherited here.
- **Briefing composition** against a fixture household: assert each field
  matches the underlying engine's direct output (no double-counting).
- **Threads:** create, list, fetch (with messages), delete; a thread from
  another household 404s; `is_shared=false` threads are invisible to the
  spouse.
- **Action items:** create from an option accept, mark done; status
  transitions enforced; household scope on every mutation.
- **`ErrDisabled` path:** with `AI_API_KEY` blank, `GET /api/advisor/briefing`
  and the Options/Horizon panels render with no narration and no error.
- **Build:** `cd backend && go build ./... && go vet ./... && go test -p 1 ./...`
  with `TEST_DATABASE_URL`. Frontend: `tsc -b && vite build && oxlint`.

## Out of scope

- The ranker itself (doc 24) and the multi-bucket allocator (doc 32).
- Plan-vs-actual reconciliation (doc 33) — action items here are tracked,
  not reconciled against a saved plan.
- Executing anything. No transfers, no payments, ever.
- Scenario modelling (doc 28, wave 7). The Scenario launchpad tile is a
  placeholder until 28 ships.
