# Goals

![Goals](../screenshots/budget-goals.png)
<em>Savings targets that track against a target date.</em>

Goals are what you're saving toward. Progress updates automatically from your
balances and cash flow — you set the target, the app tracks it.

## Your goals

Each goal card shows:

- **Name** and scope (a `personal` badge if it's a user-scoped goal).
- **Current vs. target**, with a target date if set.
- A **status chip**:

    | Chip | Meaning |
    | --- | --- |
    | Achieved | Current ≥ target |
    | Open-ended | No target date — no pace verdict |
    | On track | Saving enough to hit the target date |
    | `$X/mo short` | Not saving enough; shows the monthly shortfall |

- A progress bar and, for dated goals, the required monthly contribution and
  months left.
- **Archive** to set the goal aside without deleting it.

## Add a goal

| Field | What it does |
| --- | --- |
| Name | What you're saving for |
| Target amount | The number |
| Target date *(optional)* | Drives the on-track / monthly-required maths; leave blank for open-ended |
| Linked account *(optional)* | Track progress by that account's balance |
| Related category *(optional)* | Associate with a spending category |
| Just for me | Make it a personal (user-scoped) goal instead of household |

If you leave the account unlinked, progress is tracked by your accumulated
surplus instead of a specific balance.

!!! note "Savings goals only"
    The `goals.kind` column allows `debt_payoff` in the schema, but the
    feasibility maths, the parser, and this UI handle `savings` only. Creating a
    payoff goal isn't possible yet — see [Roadmap](../roadmap.md).

## Describe a goal *(AI)*

When AI is enabled, a **Describe a goal** box lets you say it in plain English —
"save $10k for a trip by December". The parse is a **proposal only**: it renders
a confirmation card, and only on **Confirm** does it write through the same path
the form uses. Nothing is saved implicitly.
