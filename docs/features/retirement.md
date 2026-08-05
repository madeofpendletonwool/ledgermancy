# Retirement

<em>When the money is enough, and whether it lasts. Every account projected on its
own terms.</em>

Retirement is the forward view that the [Net Worth projection](net-worth.md) is
honest about *not* being. The Net Worth model compounds one pooled balance at one
rate, which is fine as a sanity check and misleading as a retirement plan — a
401(k), a Roth IRA, and a 529 do not behave alike, and the difference is where a
retirement number quietly becomes wrong.

This page projects **each account separately** by its tax treatment, applies the
IRS contribution limits that apply to it, and answers the question everyone
actually asks: *when does the money support the spending, and does it last?*

!!! note "It is not the Net Worth projection"
    The two sit beside each other deliberately. The Net Worth page's straight-line
    model is simple enough to check by hand, and its simplicity is a feature. This
    page trades that for account-awareness; nothing about the Net Worth projection
    changed when it arrived.

## The assumptions are the honest part

A projection whose workings are hidden is not something to plan around, so the
inputs stay on screen — always visible, always editable, never behind a
disclosure triangle. **Every number below that panel is produced from these.**

| Assumption | What it drives |
| --- | --- |
| **Real return** | The growth rate, **after inflation**. 5% real is roughly 8% nominal. |
| **Inflation** | Labels the basis ("today's dollars at 3% inflation"); it does not gross figures up. |
| **Withdrawal rate** | The annual draw the nest egg is assumed to support. The convention is 4%; it is a convention, not a law. |
| **Your age today** | Ages the projection and gates catch-up limits. |
| **Target retirement age** | Where the nest-egg and savings-rate answers land. Leave blank if undecided. |
| **Annual spending to support** | What retirement needs to cover. **Blank uses your own trailing-year spend** — the app knows this and does not make you guess it. |
| **Social Security / pension** + starting age | Counted toward supported spending **from that age, not a month sooner.** |

**Everything is in today's dollars.** Because the return rate is already real
(net of inflation), no figure here is discounted afterwards. Mixing a nominal
return with today's spending is the most common way a retirement projection lies,
and it always lies in the flattering direction.

The **Inflation** field shows what CPI-U actually did over the last decade,
compounded, beside whatever you have set — with a button to adopt it. It is
offered, never applied: the projection uses the rate you choose, and there is no
second inflation input anywhere in the app. The default is 3%, and the point of
showing the measured figure is that 3% should be a decision rather than a number
nobody ever checked. See
[Real dollars and nominal dollars](../concepts.md#real-dollars-and-nominal-dollars).

## What it answers

Three headline figures:

- **Financial independence** — the first age at which the portfolio supports your
  target annual spending. `Now` if you are already there; `Age N` with the month;
  or an honest *Not within N years* when it is not reached inside the projected
  horizon. The curve is **never extrapolated** past where it was computed — "not
  reached" is an answer, and a truer one than a guess.
- **Nest egg at your target age** — the projected balance, **excluding 529 and
  other custodial balances**, because college money is not retirement money.
- **Supported spending at that age** — the nest egg × withdrawal rate, plus Social
  Security once it has started.

The chart splits the balance into **opening balance, your contributions,
employer match, and assumed growth** — because a thirty-year curve at 5% is
mostly assumption by the end, and how much of the number is money saved versus
money assumed must be legible at a glance.

## Contribution limits, applied honestly

IRS limits are part of the projection, and several rules keep them honest:

- **Pooled across accounts that share one.** Two 401(k)s share a single elective
  limit; a traditional and a Roth IRA share one IRA limit. Limits are applied to
  the *group*, not per account, or headroom would be overstated for anyone
  holding two of a kind. When a cap binds, the group is scaled proportionally —
  the projection does not get to decide which account you should fill.
- **An employer match is a percentage of salary**, bounded by both the plan's
  annual cap and what you actually deferred. A match with no salary behind it is
  not an amount, and the page will not invent one.
- **Catch-ups are age-gated** — the 50+ catch-up, the SECURE 2.0 60–63 band, and
  the HSA 55+ catch-up — against an age that, when you have entered a birthdate,
  is derived rather than typed once and left to decay.
- **Limits live in code, keyed by tax year**, and the year is shown ("2026
  limits"). A year that is not configured is reported as such and contributions
  are projected **uncapped** rather than silently applying a stale year's number.
  HSA defaults to the self-only cap unless you say family — the conservative
  direction.

The per-account contribution editor shows headroom against the shared cap
(*"$8,000 of $24,500 used across all your 401(k)s"*).

## Account treatment

A **529** (and other custodial accounts — UTMA/UGMA, Coverdell, custodial Roth,
and the Trump account) runs to its **beneficiary's** horizon, not the retirement
horizon, and is **excluded from the nest egg entirely**. A UTMA is irrevocably
the child's property from the moment it is funded; counting it as household
retirement savings overstates the position by the whole balance, and it is the
kind of error nobody catches because it moves the number the flattering way.

**Untagged accounts are excluded and named**, with the value they hold. An
account with no confirmed tax treatment has an unknown limit, an unknown horizon,
and an unknown withdrawal treatment — there is no defensible default for any of
them, and silently omitting one produces a confidently wrong number. (Tag them on
the [Investments](investments.md#account-tax-treatment) page.)

## Solve for the savings rate

Given a target retirement age, the page answers *what monthly contribution would
get me there*. The solve is a **bounded bisection**:

- It returns **"not reachable"** rather than an absurd rate. "Save 340% of your
  income" is not an answer — it is a bug with a decimal point.
- The required amount is spread across your tagged retirement accounts in
  proportion to what is already planned there — the projection decides *how much
  in total*, not which account you should fill.
- Expressed as a **rate of income** only when income is known. A savings "rate"
  with no denominator is not one.

## Sequence-of-returns test *(optional, off by default)*

Behind `RETIREMENT_MONTE_CARLO_ENABLED` — see
[Configuration](../configuration.md#retirement-sequence-of-returns-test). It is
off by default for a reason that is *not* an outbound call: a "success rate" is
the most quotable number a retirement tool can print, and Ledgermancy bundles no
market history to compute one from.

What it actually does is draw return sequences around **your own stated real
return and a volatility you set**, and report how many survive the withdrawal
phase. It is **not a historical backtest**, and the panel says so rather than
footnoting it. Seeds are derived from the inputs, so the same scenario always
yields the same number — a figure that moves on reload is not one to plan around.

## What it does not model

Listed on the page rather than left to be discovered, because these are the
places the projection could flatter:

- **Tax on withdrawals** (traditional vs. Roth ordering). Real, large, and
  dependent on income data the app does not yet hold.
- **Required Minimum Distributions.**
- **Return variability** beyond the opt-in panel above.

Everything else on the page — the projection, FI age, supported spending,
contribution headroom, the savings-rate solve — works without the Monte Carlo
panel. It adds a view; it is not load-bearing.

!!! warning "This is a projection, not advice"
    The engine computes outcomes from the assumptions you set; it does not
    recommend. Every figure is somebody's life decision, and no part of it is
    produced by a model — the math is exact decimal, and the only AI anywhere near
    this feature narrates finished strings.
