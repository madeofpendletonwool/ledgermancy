# Goals

![Goals](../screenshots/budget-goals.png)
<em>Savings targets and debt payoffs, tracked against a target date.</em>

Goals are what you're working toward — money you're putting aside, or debt
you're clearing. Progress updates automatically from your balances and cash
flow; you set the target, the app tracks it.

There are two kinds, and they use different maths:

| Kind | What it tracks | How the pace is computed |
| --- | --- | --- |
| **Savings** | Money accumulating toward a target | `remaining ÷ months left` |
| **Debt payoff** | A balance being cleared | Month-by-month amortization, including interest |

That difference is the whole point of separating them. Savings maths has no
notion of interest, so applied to a debt it understates every figure — you'd be
told a card takes 25 months when it really takes 33, or that a payment works
when it never clears the balance at all.

## Your goals

Each goal card shows:

- **Name** and scope (a `personal` badge if it's a user-scoped goal).
- **Current vs. target**, with a target date if set. On a payoff goal this reads
  "$1,000.00 of $5,000.00 **paid off**".
- A **status chip**:

    | Chip | Meaning |
    | --- | --- |
    | Achieved / Paid off | Target reached, or the debt is clear |
    | Never paid off | The payment is at or below the monthly interest — see below |
    | No terms | The linked account reports no rate or payment, so there's no schedule |
    | Open-ended | No target date — no pace verdict |
    | On track | Keeping up with the target date |
    | `$X/mo short` | Not keeping up; shows the monthly gap |

- A progress bar and, for dated goals, the required monthly contribution and
  months left.
- **Archive** to set the goal aside without deleting it.

## Debt payoff goals

Pick **Pay off a debt** as the goal type and choose the debt. A payoff card adds
a panel showing:

- **Paid off** — the date the balance reaches zero, and how many payments.
- **Interest to come** — total interest over the remaining schedule.
- **Paying** — the monthly payment and APR the schedule assumes.
- **To hit your date** — the smallest payment that clears it by your target date.

Everything is worked out from your linked account: the balance owed, the APR,
and the minimum payment all come from the institution. There's no amount to type
in, and nothing to keep updated by hand.

!!! warning "\"This debt is never paid off\""
    If the monthly payment is at or below the interest the balance accrues, the
    debt never clears — the balance stays put or grows, however long you keep
    paying. The card says so outright rather than reporting a large number of
    months:

    > At $25.00/mo this debt is never paid off — the interest alone is
    > $285.52/mo.

    This is the single most useful thing the feature can tell you, so it
    overrides every other status.

### Progress on a payoff goal

The target is **the balance to eliminate**, captured from the account when you
create the goal. Progress is `original balance − what's still owed`, so it
climbs from zero to the target as you pay the debt down, and the progress bar
reads the same way as a savings goal's.

The original is captured once, on purpose. If it were re-read on every refresh,
progress would sit at zero forever no matter how much you'd paid off.

### If a debt doesn't appear

The picker only offers accounts your institution reports loan terms for — an APR
and a payment. Without those there's nothing to amortize. If a card or loan is
missing, check that `PLAID_OPTIONAL_PRODUCTS` includes `liabilities`; see
[Accounts & Plaid](accounts.md#products). Enabling it works on accounts you've
already linked, with no relink.

## Add a goal

| Field | What it does |
| --- | --- |
| Goal type | Save toward something, or pay off a debt |
| Name | What you're saving for, or which debt you're clearing |
| Target amount *(savings)* | The number |
| Debt to pay off *(payoff)* | Which debt — only accounts with loan terms are listed |
| Target date *(optional)* | Drives the on-track / monthly-required maths; leave blank for open-ended |
| Linked account *(savings, optional)* | Track progress by that account's balance |
| Related category *(savings, optional)* | Associate with a spending category |
| Just for me | Make it a personal (user-scoped) goal instead of household |

For a savings goal, leaving the account unlinked tracks progress by your
accumulated surplus instead of a specific balance.

## Describe a goal *(AI)*

When AI is enabled, a **Describe a goal** box lets you say it in plain English —
"save $10k for a trip by December", or "pay off my credit card by December". The
parse is a **proposal only**: it renders a confirmation card, and only on
**Confirm** does it write through the same path the form uses. Nothing is saved
implicitly.

For a payoff sentence the model is given your actual debts and must name one of
them. If you mention a debt you don't have, it says so rather than quietly
pointing the goal at a different account. It never computes a schedule either —
it reads the sentence, and the amortization is worked out in exact decimal
afterwards.
