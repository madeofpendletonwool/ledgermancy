# 31 — Advisor surface (Assistant → Advisor)

*(Wave 6 — the first of the four advisor docs. Visual companion:
[advisor-overview.html](advisor-overview.html). Extends doc 24.)*

> **Shipped.** What follows is the plan as written. The route is renamed
> (`frontend/src/routes/Advisor.tsx`), the engines are exposed as chat tools, the
> Briefing/Horizon/Assumptions/Threads shell is built, and conversation + action
> items persist. Migration `00054_advisor_surface.sql` is taken. See
> **[Shipped notes](#shipped-notes)** at the end before touching this area.
>
> The short version: shipped as `00054`, not the `00052` the Data model names
> (goose strict ordering — see the notes); `advisor_messages.content` and
> `tool_trace` are `BYTEA` sealed under `ENCRYPTION_KEY`, so the portable export
> withholds them by type while `pg_dump` recovers them whole; the tool budget is
> real and shipped as `chat_toolsets.go` (the spending/planning/modelling split);
> and doc 23 landed first, so `contribution_room` returns **real** YTD deferrals
> from confirmed paystubs rather than the unverified `null` the plan hedged on.

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
around the chat.** Deterministic charts render inline in a chat turn, driven
by the tool results the turn already computed (see *Dynamic charts in chat*
below). No new math. No new tables beyond conversation/action-item
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

**Shipped as `00054_advisor_surface.sql`**, not the `00052` this section
originally named. By the time doc 31 was built, `00052_cpi_series.sql` (doc 27)
and `00053_manual_accounts.sql` (doc 30) had both landed, and goose runs in
strict-ordering mode — a `00052` arriving after `00053` is applied would refuse
to start every instance that had run them. `00054` is what the README's
reservation table already assigns to this doc; that table, not this line, was
correct. The rule the wave keeps relearning: confirm the number is both free
**and** above the highest applied version before writing, by looking at the
migrations directory rather than at any reservation.

### Household profile columns

Profile only what an engine consumes. Kids and dependents are already in
`household_people` (`00034:17-40`, with `birthdate` and `is_dependent`);
target age and assumptions are already in `projection_assumptions`.

**A column with no consumer is not a profile, it is a form field.** The first
draft added three columns "the tax/limit/guardrail engines need" and then no
doc in the wave read two of them. Each column below now names the engine that
consumes it, in this wave, or it is not here:

```sql
ALTER TABLE households
    -- Consumed by doc 32's Roth/IRA eligibility check: the MAGI phase-out
    -- thresholds are keyed by filing status, and without it the allocator
    -- cannot tell "capped at $7,500" from "not allowed to contribute at all".
    ADD COLUMN filing_status       TEXT
        CHECK (filing_status IN ('single','married_joint','married_separate','hoh')),
    -- Consumed by doc 33's guardrail rule as the personal drawdown limit.
    ADD COLUMN risk_drawdown_floor NUMERIC(5,2);  -- percent
```

**`households.state` is deliberately NOT added here.** It was justified by
"529 state caps, tax brackets" and neither exists: `AnnualLimitFor` honestly
declines to cap a 529 rather than inventing a state limit (`limits.go:110-118`),
and no wave-6 doc computes a state bracket. Whichever doc first builds a
state-dependent engine — doc 23's tax-prep summary is the likeliest — adds the
column with it. Adding it now means a column nobody writes, nobody validates,
and nobody can drop later without wondering what depended on it.

**Federal brackets are also not added here**, and doc 32's asset-location
suggestions are scoped accordingly — see that doc's note. A marginal-bracket
table belongs in code beside `limits.go` (same shape: versioned by tax year,
`ok=false` for an unconfigured year), owned by whichever doc first needs a
bracket to compute something. This doc supplies the filing status it will key
on and stops there.

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
    -- Encrypted with the existing AES-GCM cipher, as the document vault does.
    -- See "Transcripts are sensitive" below.
    content    BYTEA NOT NULL,
    -- The tool calls and results behind an assistant turn, so a reloaded
    -- thread carries the provenance of every figure in it. Same cipher.
    -- NULL for a user turn. See "A transcript without tool results" below.
    tool_trace BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX advisor_messages_thread_idx ON advisor_messages (thread_id, created_at);
```

**A transcript without tool results breaks the one rule the chat has.**
`chatSystemPrompt` (`chat_handlers.go:32-58`) requires that every number the
model states come verbatim from a tool result. Persisting only `role` and prose
honors that for a fresh turn and quietly breaks it on reload: the history is
full of figures with no provenance, and the model will re-read its own
six-week-old "$4,120 safe to spend" as though it were current and sourced.
Storing `tool_trace` beside the turn is what makes the reload honest, and lets
the UI grey a stale figure instead of reprinting it.

The replay rule: **figures in history are context, never current.** On reload,
prepend a system line naming the thread's last activity date and stating that
any figure in the transcript is as-of that date and must be re-fetched with a
tool before being restated. That is cheaper and far more robust than trying to
redact the history.

**Transcripts are sensitive, and more so than the app's other text.** A thread
is a household narrating its money in natural language — salary, debts, whether
they can afford the house. The repo already encrypts Plaid tokens and every
document byte under `ENCRYPTION_KEY`. Reuse `documents.Vault`'s cipher rather
than sealing anything by hand (doc 18's rule: callers hand it plaintext and get
a sealed value back, and nothing outside the package knows). `BYTEA` also means
the continuity export withholds these columns by type automatically, which is
the right default for a transcript — see Continuity below.

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
| `advisor_messages` | `InExport` | Conversation history. `content` and `tool_trace` are `BYTEA`, so the portable export **withholds them by type** — the rows travel, the sealed text does not. That is deliberate: the export is a plaintext JSON file a user may email themselves, and a full advisor transcript is the last thing that should ride in it. The encrypted bytes are still in the `pg_dump`, so a restore recovers them intact under the same `ENCRYPTION_KEY`. |
| `advisor_action_items` | `InExport` | User decisions |
| `households.filing_status` / `risk_drawdown_floor` | (existing table) | `households` is already `InExport`; `risk_drawdown_floor` is a `NUMERIC` percent and is cast to text in export as usual |

No new blob stores or volumes.

Note the asymmetry this creates and keep it in the restore runbook: a portable
export restores a household's threads with empty bodies, a `pg_dump` restore
brings them back whole. Doc 16's restore test opens a document end to end for
exactly this reason; add an advisor message to that assertion so the dump, the
cipher and `ENCRYPTION_KEY` are proven to agree on this table too.

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
| `contribution_room` | `networth.Limits` + `AnnualLimitFor` + YTD | Reports **cap and eligibility separately** — see below. Partial until doc 23 ships (YTD deferrals) |
| `advisor_briefing` | composed digest across engines | new composition function (see below) |

`debt_strategy` (avalanche/snowball) and `advisor_options` (the ranker) are
doc 24's, not this doc's.

**`contribution_room` must not conflate a cap with permission.**
`AnnualLimitFor` returns "the annual cap that applies to this account type at
this age" and nothing else — it has no idea whether the household is *allowed*
to contribute. A Roth IRA has a MAGI phase-out; above it the correct headroom is
$0, not $7,500. An HSA requires HDHP coverage. So the tool returns three fields
per account group, not one:

- `annual_limit` — from `AnnualLimitFor`, or `null` where it returns `ok=false`
  (taxable, 529, trust: no federal annual cap, and inventing one is worse).
- `used_ytd` — real deferrals once doc 23 lands; until then `null`, and the
  response says the headroom is **unverified**, never zero-by-default.
  *(Shipped better than written: doc 23 landed before this one, so the tool
  wraps `payroll.Year.ContributionHeadroom` and returns REAL year-to-date
  deferrals from confirmed paystubs. The honesty requirement survives intact as
  `used_ytd_verified`, which is false when no confirmed stub is on file — a
  household with no stubs has unmeasured deferrals, not zero ones, and the
  difference is the whole point of the field.)*
- `eligibility` — `eligible` / `phased_out` / `ineligible` / `unknown`.

Doc 32 owns the phase-out table that makes `eligibility` more than `unknown`;
this doc ships the field and returns `unknown` for now. Shipping the shape from
day one is the point: a client that renders "you have $7,500 of room" has no
place to put "…but you may not be allowed to use it" if the field arrives later.

### `advisor_briefing` — `internal/advisor/briefing.go`

New function, deterministic, no AI. Gathers into one struct: net worth
(`networth.Compute`), this month's slack (`BuildSafeToSpend`), FI age
(`ProjectRetirement`), the **debt-free date**, emergency-fund runway (months of
trailing spend covered), and the top three "worth your attention" items (reuse
the insight feed's highest-priority undismissed rows). The model may phrase the
one-paragraph summary; the fields are finished before the model sees them.

**The debt-free date is the MAX over `ComputePayoff`, not the min.** The first
draft said "earliest debt-free date (min over `ComputePayoff`)" — the min is
the date the *first* debt dies, which is a different and much more flattering
number. A household is debt-free when the last one does. Two consequences worth
asserting: a debt whose payoff is "never" (`ComputePayoff` reports that for a
payment at or below the interest, and for anything past a 100-year horizon —
doc 19's note) makes the whole household's debt-free date "never", not the max
of the finite ones; and a debt with an unresolvable APR cannot be projected, so
the briefing says how many debts it could not include rather than quietly
maxing over the rest.

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
PUT    /api/household/profile             handleUpdateProfile      // filing_status/risk_drawdown_floor
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

Extend `chatSystemPrompt` (`chat_handlers.go:32-58`) with the new tool list
and a clause: when the user asks "what should I do with $X," the model must
call `advisor_options` (doc 24) or `allocation_plan` (doc 32) rather than
inventing options. The prohibition on model-generated arithmetic is
unchanged and load-bearing.

### The tool budget is a real constraint, and this wave blows through it

`chat_handlers.go` ships **12** tools today against `maxToolIterations = 6`
(`chat_handlers.go:23`) and `maxChatMessages = 40`. This wave adds roughly 13
here, 2 in doc 24, 4 in doc 32 and 3 in doc 33 — about **34 tool definitions
sent on every single chat request**. Two things break at that size and neither
is hypothetical:

- **Tool selection degrades.** Thirty-four similarly-named finance tools is a
  harder retrieval problem than twelve, and the failure mode is a plausible
  wrong tool rather than an error.
- **Six iterations stops being enough.** "What should I do with $30k" plausibly
  needs `advisor_briefing` → `safe_to_spend` → `debt_summary` →
  `contribution_room` → `allocation_plan` → `plan_likelihood`. That is exactly
  6, leaving no room for a correction, and the cap exists for a good reason —
  it bounds cost and stops a spinning model.

The decision for this wave: **group the tools and send one group per request.**
`chatToolDefs()` becomes `chatToolDefs(set)` over three sets — `spending`
(today's 12), `planning` (this doc's engines + doc 24's ranker), `modelling`
(doc 32's allocator + doc 33's likelihood tools) — with the briefing and
`safe_to_spend` in all three because everything starts there. The set is chosen
by a cheap deterministic classifier over the user's message (keyword → set,
defaulting to `spending`), never by the model, and the chosen set is named in
the response so a wrong pick is visible rather than mysterious.

Raise `maxToolIterations` to 8 at the same time, and state the reason in the
comment: the advisor genuinely chains more lookups than the spending assistant
did. Both numbers get a test that fails when a set exceeds ~15 definitions, so
the next wave hits a build failure rather than a quality cliff.

The beneficiary currency nudge is a deterministic insight producer: a Roth
or 401(k) with `beneficiary_person_id IS NULL` or a beneficiary under 18
yields one insight per account per quarter, dismissible.

## Dynamic charts in chat

A question like "what have we spent vs saved over the last 6 months, and
what's our average leftover" is answered today as a Markdown table — the
`monthly_trend` tool (`chat_handlers.go:633`) already returns the series, and
the model renders it as prose. The Spending page visualizes the *same* series
as a chart (`TrendChart.tsx`). The chat should too. This section adds
**deterministic charts rendered inline in an assistant turn**, driven by the
tool results the turn already computed.

**The gap is the delivery channel, not the data or the renderer.** `POST
/api/chat` is a text-only SSE stream today — `{"delta"}`, `{"done"}`,
`{"error"}` (`chat_handlers.go:96-128`). Tool results are JSON strings handed
*only to the model* as `ToolResultBlock` text (`chat_handlers.go:177`); the
client only ever sees the model's Markdown (`Assistant.tsx:212`). To render a
chart we add one structured frame and a client-side mapper. No new math, no
new tables.

### The rule this must honor

The wave's load-bearing rule — **AI never computes; tools compute, the model
narrates** — extends to charts unchanged. Concretely:

- **The model never picks a chart type, never shapes data, and never labels an
  axis.** Chart selection is a deterministic map from *tool name* to *chart
  component*, hardcoded on the client. A wrong tool pick renders the wrong
  chart — the same visible, debuggable failure mode as today's wrong-tool-pick
  rendering wrong prose.
- **Reference lines are server-computed scalars.** "Average leftover" is not
  derived on the client from the series; the tool returns it as a field (one
  `AVG` in SQL, or a Go computation in decimal), exactly the way
  `StringFixed(2)` totals are returned today. A chart's average / budget /
  target line is a finished figure, never a client- or model-derived one.
- **A chart must reconcile with the page and the prose.** The TrendChart in
  chat, the TrendChart on the Spending page, and the model's stated figures
  all derive from the same tool/engine — the doc-24/doc-31 "two surfaces must
  agree" rule, now extended to a third. This is the property most "AI charts"
  products lose (the LLM-generated chart silently drifts from the dashboard);
  here the architecture forbids the drift by construction.

### Backend — one new SSE frame

Alongside the existing `delta` / `done` / `error` frames, emit a structured
frame per tool result so the client can render it:

```
{"tool": "monthly_trend", "result": [ {month, income, spending, leftover, ...} ]}
```

This is emitted from inside `runChat` (`chat_handlers.go:135`) at the point a
tool result is already in hand — the same value that becomes the
`ToolResultBlock` is also forwarded to the client. The model's narration
stream is unchanged. The frame rides the same SSE response; the existing
`X-Accel-Buffering: no` flush discipline applies. Persisting it is free: it is
already part of the turn's `tool_trace` (the Data model above stores the tool
calls and results behind an assistant turn), so a reloaded thread re-renders
its charts alongside its prose.

### Frontend — a tool→chart mapper, chat-sized

A single `<ChartAttachment>` component receives the `{tool, result}` frame and
dispatches to an existing chart component from `frontend/src/components/charts/`.
The mapping is exhaustive and deterministic; an unmapped tool renders no chart
(the prose still stands). The v1 set reuses components that already ship:

| Tool | Chart component | Notes |
|---|---|---|
| `monthly_trend` | `TrendChart` (or `MonthlyBars`) | Add a server-computed `avg_leftover` reference line for the "average leftover" question. |
| `spending_by_day` | `DayBars` / `SpendingHeatmap` | A month's daily spend. |
| `spend_by_category`, `category_averages` | `CategoryBars` | Largest-first; the same component the Spending page uses. |
| `top_merchants` | `MerchantPareto` | |
| `net_worth` | `NetWorthComposition` | |
| `budget_status` | `CategoryBars` | Over/under coloring. |

The chart components are page-sized today (`TrendChart` is 760×260); a chat
bubble is narrower. Two acceptable answers, picked per component: render at
page width and let the existing `.chart-scroll` overflow pattern
(`index.css:352`) scroll inside the bubble, or ship a chat-sized variant for
the two or three most common charts. Either keeps the validated chart palette
(`charts/tokens.ts`) and scale helpers (`charts/scale.ts`) unchanged.

**Streaming:** a chart cannot draw token-by-token. The clean pattern is to
stream the prose as today and mount the chart the moment its tool-result frame
lands — which is *before* the final prose, because tool calls complete earlier
in the `runChat` loop. The chart appears while the answer composes, which
reads as native.

### Tool-set classifier emits a chart hint (the natural completion)

The AI-notes classifier above (keyword → tool set) already decides which tools
a turn sees. Extend it to also emit a `chart` hint per tool, so "spent vs
saved over the last 6 months" deterministically routes to `monthly_trend`
**and** tags `chart: trend_with_avg`. The chart then renders whether the model
mentions it or not, and — like every other advisor surface — renders with no
AI key configured. This is the endpoint, not the v1: ship the structured frame
and mapper first (they work without the classifier), then wire the hint.

### No new migration

This section adds no tables or columns. It reuses this doc's
`00052_advisor_surface.sql` and, for persistence, the `advisor_messages.tool_trace`
column the Data model already defines. The structured frame is a transport
detail; nothing about it is queryable.

## Verification

Decimal-exact, table-driven, mirroring `chat_handlers_test.go`.

- **Every new tool** returns household-scoped rows only; a private account
  for user A is invisible to user B in every tool result.
- **`safe_to_spend` agreement:** the value returned by the chat tool equals
  `GET /api/budgets/safe-to-spend` for the same household and month, to the
  cent. This is the doc-24 agreement test, inherited here.
- **Briefing composition** against a fixture household: assert each field
  matches the underlying engine's direct output (no double-counting).
- **Debt-free date is the max.** A household with three debts paying off in
  8, 30 and 61 months reports 61 — the test fails on 8. A fourth debt that
  never pays off makes the whole date "never", and a debt with no resolvable
  APR is reported as excluded with a count.
- **`contribution_room` separates cap from permission:** a taxable account
  returns `annual_limit: null` (not zero); with doc 23 absent, `used_ytd` is
  `null` and the response is marked unverified rather than implying full
  headroom; `eligibility` is present and `unknown` in this doc's cycle.
- **Tool sets:** each set is under the definition cap; `safe_to_spend` and
  `advisor_briefing` appear in every set; the classifier is deterministic
  (same message → same set, twice) and the chosen set is echoed in the response.
- **Threads:** create, list, fetch (with messages), delete; a thread from
  another household 404s; `is_shared=false` threads are invisible to the
  spouse.
- **Transcript encryption and provenance:** `advisor_messages.content` is
  unreadable in a raw `SELECT` and round-trips through the vault cipher; a
  reloaded thread carries `tool_trace` for every assistant turn; the portable
  export contains the thread rows with the body columns withheld, while a
  `pg_dump` restore recovers them intact (extend doc 16's restore assertion).
- **Action items:** create from an option accept, mark done; status
  transitions enforced; household scope on every mutation.
- **`ErrDisabled` path:** with `AI_API_KEY` blank, `GET /api/advisor/briefing`
  and the Options/Horizon panels render with no narration and no error.
- **Charts render from tool results, not model output:** the structured frame
  carries the same JSON the model received; a `monthly_trend` turn renders a
  TrendChart whose points equal the `/api/reports/trend` series for the same
  window. The `avg_leftover` reference line equals the decimal mean of the
  series computed server-side — assert it matches to the cent.
- **`ErrDisabled` charts:** with `AI_API_KEY` blank, a deterministic question
  that maps to a charted tool still renders the chart (the tool-result frame
  flows without narration); the classifier `chart` hint is not required for a
  chart to appear.
- **Build:** `cd backend && go build ./... && go vet ./... && go test -p 1 ./...`
  with `TEST_DATABASE_URL`. Frontend: `tsc -b && vite build && oxlint`.

## Out of scope

- The ranker itself (doc 24) and the multi-bucket allocator (doc 32).
- Plan-vs-actual reconciliation (doc 33) — action items here are tracked,
  not reconciled against a saved plan.
- Executing anything. No transfers, no payments, ever.
- Scenario modelling (doc 28, wave 7). The Scenario launchpad tile is a
  placeholder until 28 ships.
- **Model-authored chart specs.** The model does not generate Vega-Lite,
  ECharts, or any free-form chart JSON. Chart selection is a deterministic
  tool→component map (see *Dynamic charts in chat*); a constrained spec that
  only references a tool result by id collapses back into that map, so it is
  not added as a separate path.

## Shipped notes

The route is `frontend/src/routes/Advisor.tsx`; the backend surface is
`api/advisor_surface_handlers.go` + `api/advisor_handlers.go`, the chat tools are
`api/chat_tools_advisor.go`, the Briefing is `internal/advisor/briefing.go`, and
the tool-set split is `api/chat_toolsets.go`. Migration `00054_advisor_surface.sql`
is taken. Four things reach outside the plan text.

### 1. `00054`, not `00052` — and the inline correction already in this doc holds

The Data model's reservation note (above) is correct and stays: this doc shipped
as `00054` because `00052_cpi_series.sql` (doc 27) and `00053_manual_accounts.sql`
(doc 30) had both landed first, and goose refuses a migration below the current
version. The README's reservation table already assigned `00054` to this doc;
the table, not the inline number, was right. The general lesson — confirm a
number is both free *and* above the highest applied version by reading the
migrations directory — is recorded there.

### 2. Transcripts are sealed, and the export asymmetry shipped as designed

`advisor_messages.content` and `tool_trace` are `BYTEA` under the same AES-GCM
cipher as Plaid tokens and document bytes. That makes the portable continuity
export withhold them **by type** (the rows travel, the sealed bodies do not),
while `pg_dump` recovers them whole under the same `ENCRYPTION_KEY`. All three
new tables are classified `InExport` in `continuity/coverage.go`. This is the
one place a portable-export restore and a dump restore disagree on purpose, and
the restore runbook should call it out.

### 3. The tool budget shipped as `chat_toolsets.go`

The spending / planning / modelling tool-set split — and the decision to send
one set per request rather than all ~34 definitions on every turn — landed as
`api/chat_toolsets.go` (+ `chat_toolsets_test.go`), with `safe_to_spend` and
`advisor_briefing` in every set and the chosen set echoed in the response. The
classifier is deterministic (keyword → set, defaulting to spending), never the
model. `maxToolIterations` is raised for the advisor chains.

### 4. `contribution_room` is verified, not hedged

The plan hedged `used_ytd` as `null` "until doc 23 ships." Doc 23 shipped first,
so the tool wraps `payroll.Year.ContributionHeadroom` and returns **real**
year-to-date deferrals from confirmed paystubs — with `used_ytd_verified` false
when no confirmed stub is on file, because a household with no stubs has
unmeasured deferrals, not zero ones. `eligibility` ships the field and returns
`unknown` here; doc 32's phase-out table makes it more than that.
