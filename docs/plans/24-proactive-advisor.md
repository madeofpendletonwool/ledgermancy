# 24 — Proactive cash-flow advisor

*(TODO.md "Next major initiatives" #3.)*

> **Shipped.** What follows is the plan as written. It landed as
> `backend/internal/advisor/` (the ranker, the briefing, slack assembly, the
> match window, suppression, and the weekly job), with no migration — options are
> computed on demand from existing state, exactly as the Data model said. See
> **[Shipped notes](#shipped-notes)** at the end before touching this area.
>
> The short version: the ranker's ordering is the explicit **waterfall** the
> corrected plan specifies (starter EF → unclaimed employer match → debt above the
> hurdle → full EF → expiring tax-advantaged headroom → goals → below-hurdle as a
> stated tradeoff), not the first draft's "guaranteed return first"; the employer
> match is tier 2 off `networth.annualMatch`; and **slack is
> `reporting.BuildSafeToSpend`'s median-based figure** (`AmountAfterBills` when
> obligation coverage is positive), not the "income so far + projected income"
> TODO #3 still describes — TODO #3 predates this doc and is wrong on that point.

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

**Dismissal is per insight row, and that is not free.** The first draft assumed
per-option dismissal came along with the insight feed. It does not:
`insights.dismissed_at` is one nullable column on one row (`00011:24`), so one
insight carrying three ranked options dismisses all three at once, and one
insight per option floods a feed that has to stay readable.

The decision: **one insight per advisor run**, carrying the whole ranked list,
dismissed as a unit — that is the feed's grain and fighting it is not worth a
table. Per-option suppression ("stop suggesting I pay down this card") is a
*different* need with a different lifetime, and it is served by the existing
preferences store (doc 02) as a household-scoped list of suppressed option
keys, checked by the ranker before an option is emitted. An option key is
stable: `(kind, subject_id)`, e.g. `debt_paydown:<account_id>`. No new table
either way.

## Backend

New package `backend/internal/advisor/`, driven by a weekly job in
`backend/internal/jobs/`.

### Slack

Slack is **`reporting.BuildSafeToSpend`, called and not reimplemented.** Write
that first, because the formula this doc originally carried was not the one the
function computes, and an advisor built on the wrong formula fails its own
agreement test. What `BuildSafeToSpend` actually does (`safetospend.go:121`):

```
slack = median expected income        (MEDIAN of up to 6 prior FULL months,
                                       months with zero income skipped, not
                                       counted as zero)
      − trailing fixed costs          (per category, the MEDIAN month)
      − discretionary budgets         (budgeted envelopes on non-fixed categories)
      − scheduled goal contributions
```

Three properties of that implementation the advisor inherits and must not
quietly restate:

- **It is a median of history, not "income so far + projected income."** A
  bonus month does not raise slack and a thin month does not sink it. The
  figure describes a *typical* month.
- **One-time-flagged rows are excluded** (`ExcludeOneTime` on every query), so a
  transaction the household marked non-repeating never reaches the baseline.
- **It is scoped to shared visibility** (`shared := uuid.Nil`). A member's
  private accounts are invisible to it, so advisor slack is a household-shared
  figure and the UI must not imply it covers a private account.

`SafeToSpend` also carries `AmountAfterBills` — the bill-aware variant computed
from doc 13's obligations, with the per-category no-double-counting swap in
`buildBillAware`. **The advisor uses `AmountAfterBills` when
`ObligationCoverage > 0`, and `Amount` otherwise**, and says which it used. Do
not recompute either. **Two surfaces disagreeing about slack is worse than
neither existing.**

Only run when slack clears a meaningful threshold. An advisor that fires on $12
trains the user to ignore it.

### Slack is not "spare money," and the UI must not say it is

The Budgets page presents this identical number as *"after everything that's
already spoken for, how much is left to spend freely this month?"*
(`safetospend.go:16`). The advisor presents it as the input to "here is the
highest-value thing to do with it." Same figure, opposite instruction, two
pages — and a user who notices will trust neither.

The resolution is framing, not a second number: the advisor's copy says
**"if you don't spend this, here is what it would do"**, never "you have $400
available." The tradeoff is conditional on not spending it, which is true, is
computable, and does not contradict the Budgets page.

### Options, each deterministic

- **Unclaimed employer match.** The household is contributing below the rate
  that captures the full match, so money on the table is being left there. The
  inputs already exist: `AccountPlan.EmployerMatchPct` (a fraction OF SALARY),
  `AnnualSalary` and `EmployerMatchLimit`, with `networth.annualMatch`
  (`retirement.go:394`) already computing the annual figure. The option is
  "contributing $X more per month captures $Y more match this year," and the
  return on that $X is 50–100% *instantly* — larger than any APR in the
  household and larger than any assumed market return. **This option did not
  exist in the first draft of this doc and is the single highest-value thing
  the ranker can say.**

  Two honesty constraints. It is only computable where the household has told
  the app the match terms and a salary — `annualMatch` refuses to guess, and so
  does this. And it is **calendar-bounded**: the match is captured per pay
  period, so headroom unclaimed by December 31 is gone. That is what makes it
  limit-expiring rather than merely valuable, and it is why it outranks debt.
- **Highest-APR debt paydown.** Interest avoided over the remaining term, from
  `liabilities.apr` (a percentage, not a fraction — `00004:86`) and, where
  Plaid served nothing, `account_terms.apr` (same percentage convention,
  `00043:33`). The guaranteed-return framing is honest and the strongest thing
  the app can say about a debt.

  **`liabilities.apr` is NULL more often than the first draft assumed** — Plaid
  frequently does not serve it, which is the whole reason `account_terms`
  exists. Resolve `COALESCE(account_terms.apr, liabilities.apr)`; where both
  are NULL the debt is **listed without a rank and labelled "APR unknown —
  add it to compare this,"** never defaulted, never silently dropped, and never
  sorted as though its rate were zero. A debt sorted to the bottom because its
  rate is unknown is exactly the debt most likely to be the expensive one.
- **Goal acceleration.** "$X moves the emergency fund to complete in M months
  instead of N", straight from `goals.Compute`.
- **Retirement contribution.** Via doc 15's projection: FI-age movement. With
  doc 23, also contribution-limit headroom.
- **Emergency-fund adequacy.** Months of expenses covered against trailing spend.

### The ranking rule

**Write this rule verbatim in a comment at the top of the ranker.** A user will
eventually ask why one option outranked another and the answer must not be
"the model decided."

The first draft's rule was "guaranteed return first (debt APR), then
limit-expiring opportunities, then goals." Applied literally that is wrong in
two directions that matter: it pays down a 3.5% mortgage ahead of funding a
Roth, and it drains a household's only emergency fund into a credit card. A
guaranteed 3.5% is not automatically better than an assumed 7%, and liquidity
is not a return at all. The rule is therefore a **waterfall**, in this order:

```
 1. Starter emergency fund. Until the household holds one month of trailing
    fixed costs in liquid savings, that is the only option offered. No return
    beats not borrowing at card rates for the next flat tyre.
 2. Unclaimed employer match, while pay periods remain in the tax year.
    Instant guaranteed 50-100%, and it expires.
 3. Debt whose resolved APR exceeds ADVISOR_APR_HURDLE (default: the
    household's assumed real return from projection_assumptions, floored at
    6%). Above the hurdle a guaranteed return genuinely beats an assumed one.
 4. Full emergency fund to the household's target months.
 5. Tax-advantaged contribution headroom that expires this tax year.
 6. Goal acceleration, highest shortfall-against-target-date first.
 7. Debt below the hurdle, and taxable investing, presented side by side as a
    tradeoff rather than ranked against each other — the app does not have an
    opinion on 3.5% guaranteed versus 7% assumed, and pretending to would be
    the recommendation this doc says it does not make.
```

Ties inside a tier break by larger computed dollar value, then by account name,
so the order is total and stable.

**The hurdle is the honest part of this rule.** It is derived from the
household's own stated assumption rather than a constant, it is surfaced in the
"how this was calculated" panel beside the option, and steps 3 and 7 sit on
opposite sides of it. That is what lets the ranker order debt against investing
without claiming to know which is better in general.

### Ranking versus recommending

This doc states that the app "presents computed tradeoffs, it does not
recommend." A ranked list is closer to a recommendation than that sentence
admits, and the honest position is narrower: **the ordering is a published
rule, not a judgement.** The rule is visible, it is the same for every
household, it is justified per step in the copy above, and every figure that
places an option in it is computed. What the app still does not do is override
that rule for a particular user, or say "you should" about anything below tier
2. Steps 1–2 are the only ones stated as near-unconditional, and both are
defensible as arithmetic rather than opinion.

### Relationship to doc 33's guardrail rule

Doc 33 defines a *second* ranking rule, over multi-bucket allocation plans. The
two must not appear to contradict each other on the same page, and they answer
different questions:

- **This rule ranks single uses of one marginal dollar** — it is a waterfall
  over option kinds.
- **Doc 33's guardrail picks among whole plans the user is comparing** — it is
  a filter-then-sort over computed likelihoods.

The Advisor page states which is which. Where the Options panel's top item is
not funded by the plan the guardrail picked, that disagreement is **surfaced
explicitly** ("Plan B doesn't capture your full employer match — that's tier 2
of the options waterfall"), never left for the user to spot. An unexplained
contradiction between two panels is worse than either panel alone.

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
  important test in the doc. Assert it for **both** `Amount` and
  `AmountAfterBills`, and assert the advisor picked the bill-aware one exactly
  when `ObligationCoverage > 0`.
- **Median, not mean:** a household with a one-off bonus month in the trailing
  window produces the same slack as one without it. This is the property the
  doc's original formula ("income so far + projected income") would have broken.
- **Private accounts stay invisible:** a member's private account changes
  neither the slack figure nor any option.
- Ranking is deterministic and stable: same inputs, same order, twice — and
  **total**: no two options can compare equal, since ties break on account name.
- **The waterfall, tier by tier.** A household with no starter EF is offered
  *only* the EF option, even holding a 24% card. A household with unclaimed
  match and a 22% card is offered the match first. A 3.5% mortgage never
  outranks tax-advantaged headroom, and lands in tier 7 beside taxable
  investing rather than ranked against it.
- **Employer match:** the annual figure equals `networth.annualMatch` for the
  same account, to the cent; a household with a match percentage but no stated
  salary produces **no match option at all** rather than a zero or a guess; the
  option disappears once the year's pay periods are exhausted.
- Interest-avoided figures against hand-computed fixtures. Include the
  APR-percentage-vs-fraction trap.
- **Unknown APR:** a debt with NULL in both `liabilities.apr` and
  `account_terms.apr` appears in the list, unranked, labelled — and is never
  sorted as if its rate were zero.
- **Suppression:** an option key suppressed through preferences is absent from
  the next run; dismissing the insight does not suppress individual options.
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

## Shipped notes

The package is `backend/internal/advisor/` — `rank.go` (the waterfall), `briefing.go`
(the deterministic briefing), `inputs.go` (slack + baseline assembly), `slack.go`,
`matchwindow.go`, `suppress.go` (per-option suppression through the preferences
store), and `advisor.go` tying the weekly pass together. Exposed through the
Advisor page and `api/chat_tools_advisor.go`. **No migration**, as advertised —
every option is recomputed from existing state. TODO #3 is now marked shipped.

Three things worth knowing before touching this area.

### 1. Slack is `BuildSafeToSpend`, and TODO #3's formula is wrong

TODO #3 describes slack as "income so far + projected income − …". That is not
the function the app ships and never was: `reporting.BuildSafeToSpend` is a
**median of up to six prior full months** (zero-income months skipped, not
counted as zero), minus the median trailing fixed costs, discretionary budgets,
and scheduled goal contributions. The advisor calls it rather than restating it,
and uses `AmountAfterBills` (the bill-aware variant) when obligation coverage is
positive, naming which it used. **Two surfaces disagreeing about slack is worse
than neither existing** — the agreement test asserts the advisor's figure equals
the Budgets page to the cent. Where TODO #3 contradicts this doc, this doc is
right (the README's standing rule).

### 2. The waterfall is the rule, written verbatim

The ranking rule is the corrected waterfall, with the hurdle derived from the
household's own assumed real return (floored at 6%) rather than a constant. A
household with no starter emergency fund is offered *only* the EF option even
holding a 24% card; unclaimed employer match outranks debt; a 3.5% mortgage
never outranks tax-advantaged headroom and lands in tier 7 beside taxable
investing as a tradeoff rather than ranked against it. The rule is total (ties
break on computed dollar value then account name) so the order is stable and
quotable — "the model decided" is never the answer to why one option beat
another.

### 3. Employer match is tier 2, and the ranker stays silent without a salary

The unclaimed-match option — the single highest-value thing the ranker can say —
is computed from `networth.annualMatch`, which refuses to guess without match
terms *and* a salary. A household with a match percentage but no stated salary
produces **no match option at all** rather than a zero or a guess, and the
option disappears once the year's pay periods are exhausted (the match is
calendar-bounded). An accepted option writes an `advisor_action_item`
(`source='option'`) from doc 31's surface.
