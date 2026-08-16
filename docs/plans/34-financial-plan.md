# 34 — The financial plan

**Status: SHIPPED.** Migration `00071_financial_plan.sql` is taken.

## Context

The app is excellent at **position** — every page computes where the household
*is*, in exact decimals, from live data. It had no home for **intent**: the
strategy the household actually means to follow, the reasoning behind the
targets, the decisions they made and later replaced. Goals hold targets without
the why; advisor threads hold conversations, not a plan; the Briefing reflects
data, not decisions.

"We keep the emergency fund at 3 months not 6 because the teaching income is
stable; surplus goes to the 529 until 2031, then the mortgage" is the sentence
that had nowhere to live — and it is the sentence that makes every advisor
answer better, because without it the advisor lectures a household about the
six-month rule it considered and rejected.

## AI vs deterministic split

- **Everything is deterministic storage.** Sections, decisions, the review
  stamp, the staleness nudge — all SQL and Go.
- **The advisor READS the plan** through the existing `advisor_briefing` tool
  (a digest, opened and bounded server-side). No new tool: the planning set is
  at the `maxToolsPerSet` cap and the briefing is a common tool in every set,
  so the plan rides along everywhere for free.
- **The advisor WRITES nothing, ever.** The chat's only path onto the plan is a
  `Save to plan` button on an assistant message, which posts a **proposed**
  decision (source `advisor`) that the household edits and confirms on the Plan
  page — the same suggestion-then-confirm shape as merchant merges and vehicle
  valuations. The model never computes (unchanged rule) and never writes
  unattended.

## Prerequisites

None unshipped. Builds on doc 31's advisor surface (briefing, sealed-message
conventions) and doc 21's `household_people` (per-person notes).

## Data model

Migration `00071_financial_plan.sql` (next free above `00070`):

- `plan_sections` — one row per slot: `kind` ∈ `strategy | income | estate |
  person | notes`, optional `person_id` (required iff `kind='person'`,
  enforced by CHECK), sealed `body BYTEA`, `updated_by`, timestamps. Unique on
  `(household_id, kind, COALESCE(person_id, sentinel))` via an expression
  index, which is also the upsert's conflict target.
- `plan_decisions` — the append-only log: `topic`, sealed `body`,
  `decided_at DATE` (backdating is legitimate — a plan is often written after
  the fact), `status ∈ confirmed | proposed`, `supersedes UUID` pointing at
  the decision this one replaces, `source ∈ manual | advisor`. **Active** is
  derived (no confirmed row points at me), never stored.
- `households.plan_reviewed_at` — the review stamp.

Three rules from the design, each load-bearing:

1. **Link, don't copy.** No figure columns anywhere. A section references live
   values by living beside them (EF target months stay in
   `projection_assumptions`, MAGI in `households`); a restated number is stale
   the day after the next sync.
2. **Append, don't overwrite.** A confirmed decision is immutable — the only
   UPDATEs the queries offer are `WHERE status='proposed'` by construction.
   Superseding is a new row pointing back, so the history survives its own
   replacement. A proposal may not supersede (the suggestion tray cannot edit
   the log), and only a confirmed decision may be superseded.
3. **Sealed like the transcripts.** Bodies are `BYTEA` under
   `ENCRYPTION_KEY`, same as `advisor_messages.content`; the portable export
   withholds them by type while `pg_dump` recovers them whole.

## Continuity

Both tables are classified `InExport` in `continuity/coverage.go` (sealed
bodies withheld from the portable export by type, like transcripts — the
restore runbook's asymmetry note covers them too).

## Backend

- Queries in `db/queries/plan.sql`; every query household-scoped.
- Handlers in `api/plan_handlers.go`; routes under `/api/plan` with
  `RequireAdult` (plan prose is household financial life in text). Cross-
  household reads are 404s, same convention as advisor threads.
- **Briefing digest**: `advisor.Briefing.Plan` carries the plan SEALED (the
  advisor package stays cipher-free); `Server.openPlanDigest` opens and bounds
  it (strategy 2000 chars, sections 800, persons 400, decisions 400, max 10
  active) and serves both `GET /api/advisor/briefing` and the
  `advisor_briefing` chat tool — one opener, so the strip and the chat quote
  one plan. A household with no plan gets an absent key, never an empty
  strategy. Digest fill is best-effort (like the attention feed).
- **plan_stale producer** (`insights/planreview.go`): raised when the newest
  of {review stamp, last section edit, last confirmed decision} is older than
  ~6 months; implements `Retractor` so stamping a review retracts the standing
  row. `has_content` counts sections and confirmed decisions only — a lone
  proposal is not a plan and must not be nagged.

## Frontend

- `/plan` route (`routes/Plan.tsx`), first item under the nav's Plan group
  beside Advisor.
- Sections as prose cards with upsert semantics; per-person notes ride the
  people list; decisions rendered as active / superseded / proposed with the
  superseded shown struck-through under their replacement; "Add a decision"
  can name what it replaces.
- Advisor chat: `Save to plan` on assistant messages → proposed decision,
  confirmed on the Plan page.
- `plan_stale` added to the Reminders page kinds.
- **`/api/plan` is deliberately NOT in the PWA offline allowlist**: plan
  bodies are decrypted on read, the same class as the document vault, and a
  cached copy would be the only plaintext of the household's strategy on
  disk. The plan is readable while on screen, not from a tunnel.

## Verification

`internal/api/plan_handlers_test.go` (real Postgres, real cipher): section
round-trip + sealed-at-rest + upsert-keeps-one-row; the person rule (400s and
the cross-household 404); decisions append-only (edit/delete of confirmed →
400 naming the alternative; supersede → old row flagged, new active; proposal
edit → confirm → discard); household scope on every path; review stamp; and
the briefing-digest agreement (strategy text identical through
`openPlanDigest` and `planDigestToolResult`; no-plan household → absent).
`internal/insights/planreview_test.go`: quiet with no plan / fresh plan,
raised once stale, retracted by the stamp. `go test ./...`, `npm run build`,
`tsc --noEmit`, `oxlint` all green.

## Out of scope

- No free-form wiki page-tree or rich-text editor (the vocabulary is closed so
  the briefing can digest it).
- No amounts stored anywhere in the plan (link, don't copy).
- No AI-written edits to confirmed content, ever.
- No printing/PDF story yet — the Financial Summary report remains the
  printable artifact; a position+intent print view can ride the report engine
  later if it is wanted.

## Shipped notes

- sqlc inferred the scalar-subquery touch columns (`SELECT … LIMIT 1` over a
  NOT NULL column) as NOT NULL, which would have failed scanning NULL on a
  household with sections but no decisions. Fixed by splitting them into
  `:one` queries whose "no content" arrives as `pgx.ErrNoRows` — the same
  trap class the repo has hit before with `min()`/`max()`.
- `ON CONFLICT` targeting an expression index (the COALESCE sentinel) works in
  Postgres and is what keeps "one strategy row" true without a sentinel FK.
- The briefing's `Plan` struct carries sealed bytes by design; do not "fix"
  this by handing the advisor package a cipher — the split is what keeps
  plaintext plan prose out of every layer except the API boundary.
