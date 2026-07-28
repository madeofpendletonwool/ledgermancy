# 24 — Proactive cash-flow advisor

*(TODO.md "Next major initiatives" #3.)*

## Context

The assistant answers questions when asked. The most useful money advice is
unsolicited: "you have $400 of slack this month — here are three things worth
doing with it." Nothing in the app nudges toward a good decision with spare cash,
and by the time docs 13 and 15 land it will hold every input needed to compute
one.

The pieces already exist and are unconnected:

- `reporting/safetospend.go` computes discretionary slack.
- `goals.Compute` knows what each goal needs monthly.
- `liabilities` carries `apr` and `balance` per debt (`00004:81-102`).
- The insight feed and push path are wired.
- `chat_handlers.go` already enforces the pattern this doc needs: tools compute,
  the model narrates, arithmetic is never model-generated.

What is missing is the ranking pass that turns "you have slack" into "here is the
highest-value thing to do with it, and here is why."

## AI vs deterministic split

This doc is where the split matters most, because the output *looks* like advice.

**Deterministic:** slack, every option, every projected outcome, and the ranking.
An option's value — interest avoided, months shaved off a goal, FI-age movement —
is computed in exact decimal server-side.

**AI:** presentation only. The model receives a ranked list of finished figures
and writes prose. It never invents an option, never reorders, never computes.
`chat_handlers.go`'s system prompt already forbids model-generated arithmetic —
extend those rules rather than writing new ones.

The distinction to hold: the app presents **computed tradeoffs**, it does not
**recommend**. "Paying the 22% card returns a guaranteed 22%; investing at an
assumed 7% real return does not" is a computation. "You should pay off your card"
is advice, and the app does not give it.

## Prerequisites

- **[13-bill-calendar.md](13-bill-calendar.md)** — hard. Slack computed without
  known upcoming obligations is the optimistic-in-the-first-half-of-the-month
  number doc 13 exists to fix, and an advisor built on it would confidently
  recommend spending money already committed to rent.
- **[15-fire-projections.md](15-fire-projections.md)** — hard for the
  retirement-flavoured options ("this moves FI age forward by N months").
- Insight spine (doc 04) — shipped.

Doc 25 depends on this one for advisor entries in the digest.

## Data model

**No new tables required.** Options are computed on demand from existing state.

If option dismissal needs persisting ("stop suggesting I pay down this card"),
reuse the insight feed's dismissal rather than adding a table — the advisor
surfaces *through* insights, so it inherits dismissal for free.

## Backend

New package `backend/internal/advisor/`, driven by a weekly job in
`backend/internal/jobs/`.

### Slack

```
slack = (income so far + projected income)
      − (fixed obligations, from doc 13)
      − (budgeted discretionary)
      − (scheduled goal contributions)
```

Reuse `BuildSafeToSpend` rather than reimplementing — it already handles the
no-double-counting rules (`safetospend.go:24-27`) and this must agree with what
the Budgets page shows. **Two surfaces disagreeing about slack is worse than
neither existing.**

Only run when slack clears a meaningful threshold. An advisor that fires on $12
trains the user to ignore it.

### Options, each deterministic

- **Highest-APR debt paydown.** Interest avoided over the remaining term, from
  `liabilities.apr` (percentage, not fraction — `00004:86`). The guaranteed-return
  framing is honest and the strongest thing the app can say.
- **Goal acceleration.** "$X moves the emergency fund to complete in M months
  instead of N", straight from `goals.Compute`.
- **Retirement contribution.** Via doc 15's projection: FI-age movement. With
  doc 23, also contribution-limit headroom.
- **Emergency-fund adequacy.** Months of expenses covered against trailing spend.

Rank by a documented, deterministic rule — guaranteed return first (debt APR),
then limit-expiring opportunities (401k headroom with pay periods remaining),
then goals. **Write the ranking rule in a comment.** A user will eventually ask
why one option outranked another and the answer must not be "the model decided."

### Narration

Hand the ranked list, with all figures as `StringFixed(2)` strings, to
`ai.Client`. Extend `chat_handlers.go`'s existing prohibition on invented
arithmetic. If AI is disabled (`ErrDisabled`), render the options as a plain list
— **the feature must work with no key configured**, like everything else in the
app.

### Surfacing

- A proactive insight when slack is significant, riding the existing feed and
  push path.
- A Dashboard advisor panel above a threshold.
- A digest entry (doc 25).

## Frontend

- **Dashboard advisor panel** — appears only when there is something to say.
  Each option shows its computed outcome, not just a suggestion.
- **Show the arithmetic.** An expandable "how this was calculated" per option:
  the slack components, the APR, the term. This is what separates the feature
  from a horoscope, and it is consistent with the projection assumptions being
  visible in doc 15.
- Dismiss per option, persisted through insight dismissal.
- A sensitivity/threshold setting.

## Verification

- `go test -p 1 ./...`.
- **Agreement test:** advisor slack equals the Budgets page's safe-to-spend for
  the same household and date. Assert the exact decimal. This is the single most
  important test in the doc.
- Ranking is deterministic and stable: same inputs, same order, twice.
- Interest-avoided figures against hand-computed fixtures. Include the
  APR-percentage-vs-fraction trap.
- Zero or negative slack produces no options and no insight — never a "you have
  -$300 to allocate" suggestion.
- `ErrDisabled` path renders the plain list with no narration and no error.
- A household with no debts, no goals, and no retirement accounts degrades to
  silence rather than an empty panel.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Executing anything. No transfers, no payments, ever.
- Tax-optimised withdrawal or contribution ordering (needs doc 23's tax data;
  belongs there or in 28).
- The what-if scenario engine (doc 28). This doc becomes its *discovery* layer —
  "want me to model this?" — but does not build the engine.
- Investment selection or security-level recommendations. Out of scope
  permanently; that is regulated advice.
