# Alerts

![Alerts](../screenshots/alerts.png)
<em>Rules that watch your spending and surface anything worth a glance.</em>

Alerts are the **real-time push path**. Unlike [insights](insights.md), which
surface in-app and in the digest, an alert can ping your notification channel
the moment it fires. You opt in to push **per rule**.

## Recent

The top of the page lists recent alert events, with an unread dot, a
deterministic title and detail, and — when AI is on — an italic explanation of
why it fired. **Mark all read** or mark individual events read. The header
notification bell shows the unread count.

## Rule types

| Rule | Fires when |
| --- | --- |
| **Big purchase** | A single purchase exceeds an amount you set |
| **Budget threshold** | Spending nears a category budget for the month (% of budget) |
| **New merchant** | A merchant has only just started appearing (first seen within N days, over a minimum amount) |
| **Low leftover** | Money left this month (income − spending) drops below a floor |
| **Predicted low balance** | A cash balance is **projected** to fall below a floor within N days, once known [bills](schedule.md) clear |

The last one is the only **forward-looking** rule — the others look at what has
already happened. It reads the same projected balance the
[Schedule](schedule.md) page draws, so "will this account go negative before the
next paycheck" is an alert rather than something you have to remember to check.
Use it, not *Low leftover*, when the worry is running out of money before a bill
or a payday.

Each rule has its own configurable fields (money / percent / int), rendered
from data rather than duplicated forms.

## Enabling a rule

For each rule type:

1. Toggle it **On**.
2. Set the threshold values.
3. Optionally tick **Also push this to my notification channel** — sends to
   every household member who has set a channel in **Settings →
   Notifications**.

A rule that's off can't push; the two never disagree in the stored row. Push
requires a notification channel set up first (ntfy). See
[Configuration → Push notifications](../configuration.md#push-notifications-ntfy).

Rules evaluate after each sync and on a schedule in the worker.

## Describe an alert *(AI)*

When AI is enabled, **Describe an alert** lets you say what you want in plain
English — "tell me about any purchase over $300", or "budget $600 a month for
groceries".

The parse is a **proposal only**. It renders a confirmation card showing exactly
what the engine will enforce, plus any caveats:

- **Confirm** writes through the existing mutations (creating or updating the
  rule, preserving an existing rule's push choice; brand-new rules start
  in-app only).
- **Edit in form** seeds the matching rule's form with the parsed config.
- Unsupported requests show a reason and no save — the app won't guess.

If your request is actually a **budget**, the parser says so and routes it to
the [Budgets](budgets.md) page rather than silently misfiling it.

!!! note "Alerts vs. insights"
    Alerts are the opt-in, real-time, configurable path — every rule pushes only
    because you turned that on. Insights are the automatic path: in-app and in the
    digest, with the high-priority ones pushing too. They overlap in spirit but
    are distinct systems — see [Insights → Push and digests](insights.md#push-and-digests).
